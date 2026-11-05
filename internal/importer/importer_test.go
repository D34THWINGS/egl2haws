package importer

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/egl"
	"github.com/D34THWINGS/egl2haws/internal/ha"
)

var paris, _ = time.LoadLocation("Europe/Paris")

// fakeHA stores statistics like the recorder does: one row per (id, start),
// overwritten on re-import.
type fakeHA struct {
	rows    map[string]map[time.Time]ha.Stat
	meta    map[string]ha.Metadata
	imports int
}

func newFakeHA() *fakeHA {
	return &fakeHA{rows: map[string]map[time.Time]ha.Stat{}, meta: map[string]ha.Metadata{}}
}

func (f *fakeHA) ImportStatistics(_ context.Context, md ha.Metadata, stats []ha.Stat) error {
	f.imports++
	f.meta[md.StatisticID] = md
	if f.rows[md.StatisticID] == nil {
		f.rows[md.StatisticID] = map[time.Time]ha.Stat{}
	}
	for _, s := range stats {
		f.rows[md.StatisticID][s.Start.UTC()] = s
	}
	return nil
}

func (f *fakeHA) Rows(_ context.Context, id string, from, to time.Time) ([]ha.Row, error) {
	var out []ha.Row
	for start, s := range f.rows[id] {
		if !start.Before(from) && start.Before(to) {
			out = append(out, ha.Row{Start: start, State: s.State, Sum: s.Sum})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func (f *fakeHA) sorted(id string) []ha.Stat {
	var out []ha.Stat
	for _, s := range f.rows[id] {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// fakeEGL serves readings from a map and records requested ranges.
type fakeEGL struct {
	data  map[egl.Date]float64
	calls [][2]egl.Date
	err   error
}

func (f *fakeEGL) Readings(_ context.Context, from, to egl.Date) ([]egl.Reading, error) {
	f.calls = append(f.calls, [2]egl.Date{from, to})
	if f.err != nil {
		return nil, f.err
	}
	var out []egl.Reading
	for d, l := range f.data {
		if !d.Before(from) && !to.Before(d) {
			out = append(out, egl.Reading{Date: d, Liters: l})
		}
	}
	return out, nil
}

func d(m time.Month, day int) egl.Date { return egl.Date{Year: 2026, Month: m, Day: day} }

func cfg(prices string) Config {
	p, err := ParsePrices(prices)
	if err != nil {
		panic(err)
	}
	return Config{Name: "Home", Location: paris, Prices: p, HistoryDays: 730, RefreshDays: 10, ChunkDays: 90}
}

func approx(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func TestFirstRunImportsDenseHistoryAtLocalMidnight(t *testing.T) {
	e := &fakeEGL{data: map[egl.Date]float64{
		d(10, 24): 100, // DST ends in Paris on Oct 25
		d(10, 25): 150,
		// Oct 26 missing: counted as 0
		d(10, 27): 200,
	}}
	h := newFakeHA()
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, paris)
	res, err := Run(context.Background(), cfg("3.5;2026-10-26=4"), now, e, h)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FirstRun || res.Readings != 3 || res.VolumeRows != 4 || res.LastReading != d(10, 27) {
		t.Fatalf("unexpected result %+v", res)
	}
	vol := h.sorted("egl2haws:home_volume")
	wantStates := []float64{100, 150, 0, 200}
	wantSums := []float64{100, 250, 250, 450}
	for i, s := range vol {
		day := d(10, 24).AddDays(i)
		if !s.Start.Equal(day.In(paris)) {
			t.Errorf("row %d starts %s, want local midnight of %s", i, s.Start, day)
		}
		approx(t, "state", s.State, wantStates[i])
		approx(t, "sum", s.Sum, wantSums[i])
	}
	if md := h.meta["egl2haws:home_volume"]; md.UnitOfMeasurement != "L" || md.UnitClass != "volume" || md.Source != "egl2haws" {
		t.Errorf("volume metadata %+v", md)
	}
	cost := h.sorted("egl2haws:home_cost")
	// 100L*3.5 + 150L*3.5 + 0 + 200L*4 (new price from Oct 26)
	approx(t, "cost sum", cost[len(cost)-1].Sum, 0.35+0.525+0+0.8)
	if md := h.meta["egl2haws:home_cost"]; md.UnitOfMeasurement != "EUR" || md.UnitClass != "" {
		t.Errorf("cost metadata %+v", md)
	}
	// 730 days in 90-day chunks
	if len(e.calls) != 9 || e.calls[0][0] != egl.DateOf(now).AddDays(-730) || e.calls[8][1] != egl.DateOf(now) {
		t.Errorf("unexpected EGL calls %v", e.calls)
	}
}

func TestRefreshOverwritesWindowAndKeepsSums(t *testing.T) {
	e := &fakeEGL{data: map[egl.Date]float64{}}
	for i := 0; i < 30; i++ {
		e.data[d(9, 1).AddDays(i)] = 10
	}
	h := newFakeHA()
	ctx := context.Background()
	if _, err := Run(ctx, cfg("4"), time.Date(2026, 10, 1, 9, 0, 0, 0, paris), e, h); err != nil {
		t.Fatal(err)
	}

	// Later: EGL corrects Sep 25 and publishes new days.
	e.data[d(9, 25)] = 50
	e.data[d(10, 1)] = 20
	e.data[d(10, 2)] = 30
	e.calls = nil
	res, err := Run(ctx, cfg("4"), time.Date(2026, 10, 3, 9, 0, 0, 0, paris), e, h)
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstRun || res.From != d(9, 23) || len(e.calls) != 1 {
		t.Fatalf("unexpected refresh %+v calls=%v", res, e.calls)
	}
	vol := h.sorted("egl2haws:home_volume")
	if len(vol) != 32 {
		t.Fatalf("got %d rows, want 32", len(vol))
	}
	running := 0.0
	for _, s := range vol {
		running += s.State
		approx(t, "running sum "+s.Start.String(), s.Sum, running)
	}
	approx(t, "total", res.TotalLiters, 29*10+50+20+30)
	cost := h.sorted("egl2haws:home_cost")
	approx(t, "cost total", cost[len(cost)-1].Sum, res.TotalLiters/1000*4)
}

func TestPriceChangeIsRetroactive(t *testing.T) {
	e := &fakeEGL{data: map[egl.Date]float64{d(1, 10): 1000, d(6, 10): 1000}}
	h := newFakeHA()
	ctx := context.Background()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, paris)
	if _, err := Run(ctx, cfg("3"), now, e, h); err != nil {
		t.Fatal(err)
	}
	// Jan 10 is outside the refresh window, but its cost must follow the new schedule.
	if _, err := Run(ctx, cfg("3;2026-06-01=5"), now, e, h); err != nil {
		t.Fatal(err)
	}
	cost := h.sorted("egl2haws:home_cost")
	approx(t, "cost total", cost[len(cost)-1].Sum, 3+5)
}

func TestNoReadingsWritesNothing(t *testing.T) {
	h := newFakeHA()
	res, err := Run(context.Background(), cfg(""), time.Now(), &fakeEGL{}, h)
	if err != nil || res.Readings != 0 || h.imports != 0 {
		t.Fatalf("res=%+v err=%v imports=%d", res, err, h.imports)
	}
}

func TestErrorsNameTheFailingSystem(t *testing.T) {
	_, err := Run(context.Background(), cfg(""), time.Now(), &fakeEGL{err: egl.ErrInvalidCredentials}, newFakeHA())
	var e *Error
	if !errors.As(err, &e) || e.System != "egl" || !IsAuth(err) {
		t.Fatalf("got %v", err)
	}
}

func TestParsePrices(t *testing.T) {
	s, err := ParsePrices(" 3,56 ; 2026-01-01=4.1 ")
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := s.At(d(12, 31).AddDays(-366)); p != 3.56 {
		t.Errorf("before: %v", p)
	}
	if p, _ := s.At(d(1, 1)); p != 4.1 {
		t.Errorf("after: %v", p)
	}
	if s, _ := ParsePrices("2026-01-01=4"); func() bool { _, ok := s.At(d(1, 1).AddDays(-1)); return ok }() {
		t.Error("no price expected before the first dated period")
	}
	for _, bad := range []string{"abc", "-1", "2026-13-01=4"} {
		if _, err := ParsePrices(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Home": "home", "Maison Lyon 3e": "maison_lyon_3e", "--": "egl"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// leakEGL serves readings with the leak fields set.
type leakEGL struct{ rs []egl.Reading }

func (l leakEGL) Readings(_ context.Context, from, to egl.Date) ([]egl.Reading, error) {
	var out []egl.Reading
	for _, r := range l.rs {
		if !r.Date.Before(from) && !to.Before(r.Date) {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestLeakStatistics(t *testing.T) {
	flow, est := 6.0, 0.0
	e := leakEGL{rs: []egl.Reading{
		{Date: d(10, 30), Liters: 100},                                     // no leak fields reported
		{Date: d(10, 31), Liters: 150, MinFlowLH: &flow, LeakLiters: &est}, // reported
	}}
	h := newFakeHA()
	res, err := Run(context.Background(), cfg(""), time.Date(2026, 11, 2, 9, 0, 0, 0, paris), e, h)
	if err != nil {
		t.Fatal(err)
	}
	if res.LeakRows != 2 || len(res.Fetched) != 2 {
		t.Fatalf("result %+v", res)
	}
	mf := h.sorted("egl2haws:home_min_flow")
	if len(mf) != 1 || mf[0].State != 6 || !mf[0].Start.Equal(d(10, 31).In(paris)) {
		t.Fatalf("min flow rows %+v", mf)
	}
	if md := h.meta["egl2haws:home_min_flow"]; !md.Mean || md.UnitOfMeasurement != "L/h" {
		t.Errorf("min flow metadata %+v", md)
	}
	if md := h.meta["egl2haws:home_leak_estimate"]; !md.Mean || md.UnitOfMeasurement != "L" || md.UnitClass != "volume" {
		t.Errorf("leak estimate metadata %+v", md)
	}
}
