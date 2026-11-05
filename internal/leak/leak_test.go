package leak

import (
	"slices"
	"testing"

	"github.com/D34THWINGS/egl2haws/internal/egl"
)

func f(v float64) *float64 { return &v }

// days builds consecutive daily readings ending on Nov 1, 2026, from
// (liters, minFlow) pairs; a negative minFlow means "not reported".
func days(vals ...[2]float64) []egl.Reading {
	end := egl.Date{Year: 2026, Month: 11, Day: 1}
	var out []egl.Reading
	for i, v := range vals {
		r := egl.Reading{Date: end.AddDays(i - len(vals) + 1), Liters: v[0], LeakLiters: f(0)}
		if v[1] >= 0 {
			r.MinFlowLH = f(v[1])
		}
		out = append(out, r)
	}
	return out
}

var defaults = Config{MinFlowLH: 0, Days: 2, SpikeFactor: 0, SpikeMinLiters: 100}

func TestNoLeak(t *testing.T) {
	st := Evaluate(days([2]float64{150, 0}, [2]float64{200, 0}, [2]float64{180, 0}), defaults)
	if !st.Known || st.Leak || st.FlowDays != 0 || st.Date != (egl.Date{Year: 2026, Month: 11, Day: 1}) || st.Liters != 180 {
		t.Fatalf("%+v", st)
	}
}

func TestContinuousFlow(t *testing.T) {
	// One day of flow isn't enough with Days=2...
	st := Evaluate(days([2]float64{150, 0}, [2]float64{200, 0}, [2]float64{300, 6}), defaults)
	if st.Leak || st.FlowDays != 1 {
		t.Fatalf("one day: %+v", st)
	}
	// ...two in a row is.
	st = Evaluate(days([2]float64{150, 0}, [2]float64{280, 5}, [2]float64{300, 6}), defaults)
	if !st.Leak || st.FlowDays != 2 || !slices.Equal(st.Reasons, []string{ReasonContinuousFlow}) || *st.MinFlowLH != 6 {
		t.Fatalf("two days: %+v", st)
	}
	// A threshold ignores small flows (e.g. a softener).
	cfg := defaults
	cfg.MinFlowLH = 10
	if st := Evaluate(days([2]float64{280, 5}, [2]float64{300, 6}), cfg); st.Leak {
		t.Fatalf("below threshold: %+v", st)
	}
	// A missing field or a gap breaks the streak.
	if st := Evaluate(days([2]float64{280, 5}, [2]float64{290, -1}, [2]float64{300, 6}), defaults); st.Leak || st.FlowDays != 1 {
		t.Fatalf("missing field: %+v", st)
	}
	gap := days([2]float64{280, 5}, [2]float64{290, 5}, [2]float64{300, 6})
	gap = append(gap[:1], gap[2:]...)
	if st := Evaluate(gap, defaults); st.Leak || st.FlowDays != 1 {
		t.Fatalf("gap: %+v", st)
	}
}

func TestEGLEstimate(t *testing.T) {
	rs := days([2]float64{150, 0}, [2]float64{200, 0})
	rs[1].LeakLiters = f(42)
	st := Evaluate(rs, defaults)
	if !st.Leak || !slices.Equal(st.Reasons, []string{ReasonEGLEstimate}) || *st.EGLEstimateLiters != 42 {
		t.Fatalf("%+v", st)
	}
}

func TestUnusualConsumption(t *testing.T) {
	var vals [][2]float64
	for i := 0; i < 30; i++ {
		vals = append(vals, [2]float64{float64(150 + i%5*10), 0}) // median 170
	}
	vals = append(vals, [2]float64{600, 0})
	rs := days(vals...)
	if st := Evaluate(rs, defaults); st.Leak || st.BaselineLiters != 170 {
		t.Fatalf("spike rule must be off by default: %+v", st)
	}
	cfg := defaults
	cfg.SpikeFactor = 3
	if st := Evaluate(rs, cfg); !st.Leak || !slices.Equal(st.Reasons, []string{ReasonUnusualConsumption}) {
		t.Fatalf("spike: %+v", st)
	}
	cfg.SpikeMinLiters = 500 // 600 - 170 < 500
	if st := Evaluate(rs, cfg); st.Leak {
		t.Fatalf("small absolute increase: %+v", st)
	}
	// Not enough history for a baseline.
	cfg.SpikeMinLiters = 0
	if st := Evaluate(rs[len(rs)-5:], cfg); st.Leak || st.BaselineLiters != 0 {
		t.Fatalf("short history: %+v", st)
	}
}

func TestEmpty(t *testing.T) {
	if st := Evaluate(nil, defaults); st.Known || st.Leak {
		t.Fatalf("%+v", st)
	}
}
