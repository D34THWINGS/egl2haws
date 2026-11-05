// Package leak flags probable water leaks from EGL's daily readings.
//
// EGL publishes one reading per day, 1 to 3 days late, so this finds slow,
// continuous leaks (a running toilet, a dripping pipe) that would otherwise
// go unnoticed for weeks, not a burst pipe. Three rules, any of which flags
// a leak:
//
//   - egl_estimate: EGL's own leak estimate (volumeEstimeFuite) is above 0
//     on the latest day.
//   - continuous_flow: the meter's minimum flow of the day (debitMin) stayed
//     above MinFlowLH for Days consecutive days. Without a leak, water use
//     stops at some point (usually at night), so the minimum is 0.
//   - unusual_consumption (off unless SpikeFactor > 0): the latest day used
//     more than SpikeFactor times the median of the previous 30 days, and at
//     least SpikeMinLiters more.
package leak

import (
	"sort"

	"github.com/D34THWINGS/egl2haws/internal/egl"
)

// Reasons a leak can be flagged.
const (
	ReasonEGLEstimate        = "egl_estimate"
	ReasonContinuousFlow     = "continuous_flow"
	ReasonUnusualConsumption = "unusual_consumption"
)

// Config tunes the rules.
type Config struct {
	// MinFlowLH: a day counts towards continuous_flow when its minimum flow
	// is strictly above this (L/h). 0 means any flow at all.
	MinFlowLH float64
	// Days of continuous flow in a row before flagging (>= 1).
	Days int
	// SpikeFactor enables unusual_consumption when > 0.
	SpikeFactor float64
	// SpikeMinLiters avoids flagging tiny absolute increases.
	SpikeMinLiters float64
}

// baselineDays is the window of the unusual_consumption median, and
// minBaseline how many of those days must have data for it to apply.
const (
	baselineDays = 30
	minBaseline  = 14
)

// Status is the evaluation of the latest readings.
type Status struct {
	// Known is false when there are no readings to evaluate.
	Known   bool
	Leak    bool
	Reasons []string
	// Date is the latest day evaluated (EGL's latest published reading).
	Date egl.Date
	// Liters is the latest day's consumption.
	Liters float64
	// MinFlowLH is the latest day's minimum flow, nil if EGL didn't report it.
	MinFlowLH *float64
	// FlowDays is how many days in a row, up to Date, had a minimum flow
	// above the threshold.
	FlowDays int
	// EGLEstimateLiters is EGL's leak estimate for the latest day, if any.
	EGLEstimateLiters *float64
	// BaselineLiters is the median daily consumption over the previous 30
	// days, 0 if there isn't enough data.
	BaselineLiters float64
}

// Evaluate applies the rules to readings (any order; gaps allowed).
func Evaluate(readings []egl.Reading, cfg Config) Status {
	if len(readings) == 0 {
		return Status{}
	}
	if cfg.Days < 1 {
		cfg.Days = 1
	}
	rs := append([]egl.Reading(nil), readings...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Date.Before(rs[j].Date) })
	latest := rs[len(rs)-1]
	st := Status{
		Known:             true,
		Date:              latest.Date,
		Liters:            latest.Liters,
		MinFlowLH:         latest.MinFlowLH,
		EGLEstimateLiters: latest.LeakLiters,
	}

	if latest.LeakLiters != nil && *latest.LeakLiters > 0 {
		st.Reasons = append(st.Reasons, ReasonEGLEstimate)
	}

	// Days in a row, ending on the latest day, with flow above threshold. A
	// missing day or a day without the field breaks the streak.
	expect := latest.Date
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if r.Date != expect || r.MinFlowLH == nil || *r.MinFlowLH <= cfg.MinFlowLH {
			break
		}
		st.FlowDays++
		expect = expect.AddDays(-1)
	}
	if st.FlowDays >= cfg.Days {
		st.Reasons = append(st.Reasons, ReasonContinuousFlow)
	}

	var base []float64
	from := latest.Date.AddDays(-baselineDays)
	for _, r := range rs[:len(rs)-1] {
		if !r.Date.Before(from) {
			base = append(base, r.Liters)
		}
	}
	if len(base) >= minBaseline {
		st.BaselineLiters = median(base)
		if cfg.SpikeFactor > 0 && latest.Liters > cfg.SpikeFactor*st.BaselineLiters &&
			latest.Liters-st.BaselineLiters >= cfg.SpikeMinLiters {
			st.Reasons = append(st.Reasons, ReasonUnusualConsumption)
		}
	}

	st.Leak = len(st.Reasons) > 0
	return st
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
