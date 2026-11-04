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
}

type rawReading struct {
	Consommation *float64 `json:"consommation"`
	Annee        int      `json:"annee"`
	Mois         int      `json:"mois"`
	Jour         int      `json:"jour"`
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
	r, _, err := parseReadings(body, from, to, nil)
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
func parseReadings(body []byte, from, to Date, known *MonthBase) ([]Reading, MonthBase, error) {
	var raws []rawReading
	unit := "l"
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, MonthBase{}, fmt.Errorf("decode readings: %w", err)
		}
	} else {
		var resp struct {
			Postes []struct {
				Data []rawReading `json:"data"`
			} `json:"postes"`
			Unites struct {
				Consommation string `json:"consommation"`
			} `json:"unites"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, MonthBase{}, fmt.Errorf("decode readings: %w", err)
		}
		if len(resp.Postes) > 0 {
			raws = resp.Postes[0].Data
		}
		if u := strings.ToLower(strings.TrimSpace(resp.Unites.Consommation)); u != "" {
			unit = u
		}
	}
	if len(raws) == 0 {
		return nil, MonthBase{}, nil
	}

	factor, err := litersFactor(unit)
	if err != nil {
		return nil, MonthBase{}, err
	}

	base := MonthBase{}
	if known != nil {
		base = *known
	} else if base, err = detectMonthBase(raws, from, to); err != nil {
		return nil, MonthBase{}, err
	}
	zeroBased := base.ZeroBased

	byDay := map[Date]float64{}
	for _, r := range raws {
		d, ok := toDate(r, zeroBased)
		if !ok {
			return nil, MonthBase{}, fmt.Errorf("invalid date annee=%d mois=%d jour=%d", r.Annee, r.Mois, r.Jour)
		}
		v := 0.0
		if r.Consommation != nil {
			v = *r.Consommation * factor
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, MonthBase{}, fmt.Errorf("invalid consumption %v on %s", v, d)
		}
		byDay[d] = v // a later entry for the same day wins
	}
	out := make([]Reading, 0, len(byDay))
	for d, v := range byDay {
		out = append(out, Reading{Date: d, Liters: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, base, nil
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
