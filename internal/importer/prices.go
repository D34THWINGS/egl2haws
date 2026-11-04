package importer

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/egl"
)

// PriceSchedule maps days to a water price in €/m³ (TTC, all included).
type PriceSchedule []PricePeriod

// PricePeriod is a price applying from Start (inclusive) until the next
// period. A zero Start means "since the beginning".
type PricePeriod struct {
	Start egl.Date
	Price float64
}

// ParsePrices reads "3.56" or "3.56;2026-01-01=4.12;2027-01-01=4.30".
// An empty string means no cost tracking.
func ParsePrices(s string) (PriceSchedule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out PriceSchedule
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p := PricePeriod{}
		value := part
		if date, v, ok := strings.Cut(part, "="); ok {
			t, err := time.Parse("2006-01-02", strings.TrimSpace(date))
			if err != nil {
				return nil, fmt.Errorf("price %q: bad date: %w", part, err)
			}
			p.Start, value = egl.DateOf(t), v
		}
		price, err := strconv.ParseFloat(strings.TrimSpace(strings.Replace(value, ",", ".", 1)), 64)
		if err != nil || price < 0 {
			return nil, fmt.Errorf("price %q: not a positive number", part)
		}
		p.Price = price
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// At returns the price for a day, and false if no period covers it.
func (s PriceSchedule) At(d egl.Date) (float64, bool) {
	price, ok := 0.0, false
	for _, p := range s {
		if p.Start == (egl.Date{}) || !d.Before(p.Start) {
			price, ok = p.Price, true
		}
	}
	return price, ok
}
