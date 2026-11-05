package egl

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Date is a calendar day, without time or zone.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf returns the calendar day of t in t's location.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{y, m, d}
}

// Time returns midnight UTC of the day, for date arithmetic and API params.
func (d Date) Time() time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }

// In returns local midnight of the day in loc.
func (d Date) In(loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// AddDays returns the day n days later (or earlier if n < 0).
func (d Date) AddDays(n int) Date { return DateOf(d.Time().AddDate(0, 0, n)) }

// Before reports whether d is strictly before o.
func (d Date) Before(o Date) bool { return d.Time().Before(o.Time()) }

func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

// Reading is one day of consumption.
type Reading struct {
	Date   Date
	Liters float64
	// MinFlowLH is the lowest flow the meter measured that day, in L/h
	// (debitMin). Without a leak, water use stops at some point (usually at
	// night), so it's 0. Nil when EGL doesn't report it.
	MinFlowLH *float64
	// LeakLiters is EGL's own estimate of the volume leaked that day
	// (volumeEstimeFuite). Nil when EGL doesn't report it.
	LeakLiters *float64
}

type rawReading struct {
	Consommation      *float64 `json:"consommation"`
	DebitMin          *float64 `json:"debitMin"`
	VolumeEstimeFuite *float64 `json:"volumeEstimeFuite"`
	Annee             int      `json:"annee"`
	Mois              int      `json:"mois"`
	Jour              int      `json:"jour"`
}

// Units are the units EGL declared in a response ("unites"), as received.
type Units struct {
	Consommation      string `json:"consommation"`
	DebitMin          string `json:"debitMin"`
	VolumeEstimeFuite string `json:"volumeEstimeFuite"`
}

// ParseReadings decodes a consommationsJournalieres response for the
// requested [from, to] range.
//
// Observed format: {"postes":[{"data":[{"consommation":N,"annee":Y,"mois":M,
// "jour":D}]}],"unites":{"consommation":"m3"|"l"}}. A bare list of entries
// is accepted too.
//
// Months are 0-based (JS Date style): confirmed on the live API in 2026-11
// (see testdata/consommationsJournalieres.json, where mois 9 / jour 31 is
// October 31). As this isn't documented and could change, the base is still
// detected from the data: month 0 means 0-based, month 12 means 1-based, and
// otherwise the reading whose dates fall inside the requested range wins
// (the wrong one shifts every date by a month); ties default to 0-based.
func ParseReadings(body []byte, from, to Date) ([]Reading, error) {
	r, _, _, err := parseReadings(body, from, to, nil)
	return r, err
}

// MonthBase records how month numbers were interpreted.
type MonthBase struct {
	ZeroBased bool
	// Decisive is false when the data fit both interpretations equally and
	// the default was used.
	Decisive bool
}

// parseReadings is ParseReadings with an optional known month base, which
// skips detection.
func parseReadings(body []byte, from, to Date, known *MonthBase) ([]Reading, MonthBase, Units, error) {
	var raws []rawReading
	var units Units
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, MonthBase{}, units, fmt.Errorf("decode readings: %w", err)
		}
	} else {
		var resp struct {
			Postes []struct {
				Data []rawReading `json:"data"`
			} `json:"postes"`
			Unites Units `json:"unites"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, MonthBase{}, units, fmt.Errorf("decode readings: %w", err)
		}
		if len(resp.Postes) > 0 {
			raws = resp.Postes[0].Data
		}
		units = resp.Unites
	}
	if len(raws) == 0 {
		return nil, MonthBase{}, units, nil
	}

	unit := strings.ToLower(strings.TrimSpace(units.Consommation))
	if unit == "" {
		unit = "l"
	}
	factor, err := litersFactor(unit)
	if err != nil {
		return nil, MonthBase{}, units, err
	}
	// The leak fields only feed leak detection: an unknown unit for them
	// falls back to the default instead of failing the sync.
	flowFactor := flowLHFactor(units.DebitMin)
	leakFactor, err := litersFactor(strings.ToLower(strings.TrimSpace(units.VolumeEstimeFuite)))
	if err != nil {
		leakFactor = 1
	}

	base := MonthBase{}
	if known != nil {
		base = *known
	} else if base, err = detectMonthBase(raws, from, to); err != nil {
		return nil, MonthBase{}, units, err
	}
	zeroBased := base.ZeroBased

	byDay := map[Date]Reading{}
	for _, r := range raws {
		d, ok := toDate(r, zeroBased)
		if !ok {
			return nil, MonthBase{}, units, fmt.Errorf("invalid date annee=%d mois=%d jour=%d", r.Annee, r.Mois, r.Jour)
		}
		v := 0.0
		if r.Consommation != nil {
			v = *r.Consommation * factor
		}
		if !validAmount(v) {
			return nil, MonthBase{}, units, fmt.Errorf("invalid consumption %v on %s", v, d)
		}
		reading := Reading{Date: d, Liters: v}
		if r.DebitMin != nil && validAmount(*r.DebitMin) {
			f := *r.DebitMin * flowFactor
			reading.MinFlowLH = &f
		}
		if r.VolumeEstimeFuite != nil && validAmount(*r.VolumeEstimeFuite) {
			l := *r.VolumeEstimeFuite * leakFactor
			reading.LeakLiters = &l
		}
		byDay[d] = reading // a later entry for the same day wins
	}
	out := make([]Reading, 0, len(byDay))
	for _, r := range byDay {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, base, units, nil
}

func validAmount(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// flowLHFactor converts a declared flow unit to L/h. Unknown or missing
// units are assumed to be L/h.
func flowLHFactor(unit string) float64 {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(unit), " ", "")) {
	case "m3/h", "m³/h":
		return 1000
	case "l/min":
		return 60
	case "m3/j", "m³/j", "m3/d", "m³/d":
		return 1000.0 / 24
	case "l/j", "l/d":
		return 1.0 / 24
	default: // "l/h", "l" and anything unrecognized
		return 1
	}
}

func litersFactor(unit string) (float64, error) {
	switch unit {
	case "l", "litre", "litres", "liter", "liters":
		return 1, nil
	case "m3", "m³":
		return 1000, nil
	default:
		return 0, fmt.Errorf("unknown consumption unit %q", unit)
	}
}

func toDate(r rawReading, zeroBased bool) (Date, bool) {
	m := r.Mois
	if zeroBased {
		m++
	}
	if m < 1 || m > 12 || r.Jour < 1 || r.Annee < 2000 {
		return Date{}, false
	}
	t := time.Date(r.Annee, time.Month(m), r.Jour, 0, 0, 0, 0, time.UTC)
	if t.Day() != r.Jour { // e.g. 31 of a 30-day month
		return Date{}, false
	}
	return DateOf(t), true
}

func detectMonthBase(raws []rawReading, from, to Date) (MonthBase, error) {
	for _, r := range raws {
		switch r.Mois {
		case 0:
			return MonthBase{ZeroBased: true, Decisive: true}, nil
		case 12:
			return MonthBase{ZeroBased: false, Decisive: true}, nil
		}
	}
	score := func(zeroBased bool) int {
		n := 0
		for _, r := range raws {
			d, ok := toDate(r, zeroBased)
			if !ok {
				return -1
			}
			if !d.Before(from) && !to.Before(d) {
				n++
			}
		}
		return n
	}
	zero, one := score(true), score(false)
	switch {
	case zero > one:
		return MonthBase{ZeroBased: true, Decisive: true}, nil
	case one > zero:
		return MonthBase{ZeroBased: false, Decisive: true}, nil
	case zero >= 0:
		// Can't tell (e.g. a single day): assume 0-based, which is what
		// ha-egl-water does against the live API.
		return MonthBase{ZeroBased: true}, nil
	default:
		return MonthBase{}, fmt.Errorf("cannot interpret month numbers in response")
	}
}
