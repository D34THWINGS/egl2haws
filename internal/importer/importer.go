// Package importer imports EGL daily readings into Home Assistant statistics.
//
// It keeps no state of its own: Home Assistant's statistics are the source of
// truth for what was already imported.
//
//   - First run (no volume statistic yet): import HistoryDays of history.
//   - Every run after: re-read the last RefreshDays and overwrite them. EGL
//     publishes late, in batches, and sometimes corrects days already
//     published, so a fixed rewritten window absorbs all of that without
//     tracking what changed.
//   - Cost is fully recomputed from the volume statistic on every run, so a
//     price change is applied retroactively according to the schedule.
//
// Each daily reading is stored at local midnight of its day.
package importer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/egl"
	"github.com/D34THWINGS/egl2haws/internal/ha"
)

// Source is the statistic source (the part before ":" in statistic IDs).
const Source = "egl2haws"

// EGL is the subset of the EGL client used here.
type EGL interface {
	Readings(ctx context.Context, from, to egl.Date) ([]egl.Reading, error)
}

// HA is the subset of the Home Assistant client used here.
type HA interface {
	ImportStatistics(ctx context.Context, md ha.Metadata, stats []ha.Stat) error
	Rows(ctx context.Context, statisticID string, from, to time.Time) ([]ha.Row, error)
}

// Config drives a sync.
type Config struct {
	// Name prefixes statistic names and IDs ("egl" -> egl2haws:egl_volume).
	Name        string
	Location    *time.Location
	Prices      PriceSchedule // nil: no cost statistic
	HistoryDays int           // depth of the first import
	RefreshDays int           // window rewritten on every run
	ChunkDays   int           // days per EGL request
}

// Result summarizes a sync, for logs.
type Result struct {
	FirstRun     bool
	From, To     egl.Date // requested range
	Readings     int      // days returned by EGL
	LastReading  egl.Date // latest day with data (zero if none)
	LastLiters   float64
	VolumeRows   int
	CostRows     int
	TotalLiters  float64 // volume sum after the sync
	TotalCostEUR float64
}

// Error wraps failures with the system that failed, for logs and alerting.
type Error struct {
	System string // "egl" or "home_assistant"
	Err    error
}

func (e *Error) Error() string { return e.System + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a display name into a statistic object ID.
func Slug(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if s == "" {
		s = "egl"
	}
	return s
}

// VolumeID and CostID are the statistic IDs written for a config.
func (c Config) VolumeID() string { return Source + ":" + Slug(c.Name) + "_volume" }
func (c Config) CostID() string   { return Source + ":" + Slug(c.Name) + "_cost" }

// sentinel is "before any data" for statistics queries.
var sentinel = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Run performs one sync.
func Run(ctx context.Context, cfg Config, now time.Time, e EGL, h HA) (Result, error) {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.ChunkDays <= 0 {
		cfg.ChunkDays = 90
	}
	var res Result
	today := egl.DateOf(now.In(cfg.Location))

	existing, err := h.Rows(ctx, cfg.VolumeID(), sentinel, now.Add(24*time.Hour))
	if err != nil {
		return res, &Error{"home_assistant", err}
	}
	res.FirstRun = len(existing) == 0
	days := cfg.RefreshDays
	if res.FirstRun {
		days = cfg.HistoryDays
	}
	res.From, res.To = today.AddDays(-days), today

	readings, err := fetch(ctx, e, res.From, res.To, cfg.ChunkDays)
	if err != nil {
		return res, &Error{"egl", err}
	}
	res.Readings = len(readings)
	if len(readings) == 0 {
		res.TotalLiters = lastSum(existing)
		return res, nil
	}
	last := readings[len(readings)-1]
	res.LastReading, res.LastLiters = last.Date, last.Liters

	// Dense series: days EGL skipped inside the window count as 0, so sums
	// stay consistent with rows written by earlier runs. It never starts
	// before the earliest known data (in HA or from EGL), so a refresh window
	// reaching further back than the history doesn't add empty days.
	earliest := readings[0].Date
	if len(existing) > 0 {
		if d := egl.DateOf(existing[0].Start.In(cfg.Location)); d.Before(earliest) {
			earliest = d
		}
	}
	start := res.From
	if start.Before(earliest) {
		start = earliest
	}
	byDay := make(map[egl.Date]float64, len(readings))
	for _, r := range readings {
		byDay[r.Date] = r.Liters
	}

	windowStart := start.In(cfg.Location)
	sum := 0.0
	for _, r := range existing {
		if r.Start.Before(windowStart) {
			sum = r.Sum
		}
	}
	var vol []ha.Stat
	for d := start; !last.Date.Before(d); d = d.AddDays(1) {
		l := byDay[d]
		sum += l
		vol = append(vol, ha.Stat{Start: d.In(cfg.Location), State: l, Sum: sum})
	}
	err = h.ImportStatistics(ctx, ha.Metadata{
		StatisticID:       cfg.VolumeID(),
		Source:            Source,
		Name:              cfg.Name + " water",
		UnitOfMeasurement: "L",
		UnitClass:         "volume",
	}, vol)
	if err != nil {
		return res, &Error{"home_assistant", err}
	}
	res.VolumeRows, res.TotalLiters = len(vol), sum

	if len(cfg.Prices) == 0 {
		return res, nil
	}
	if cost := costRows(cfg, existing, windowStart, vol); len(cost) > 0 {
		err = h.ImportStatistics(ctx, ha.Metadata{
			StatisticID:       cfg.CostID(),
			Source:            Source,
			Name:              cfg.Name + " water cost",
			UnitOfMeasurement: "EUR",
		}, cost)
		if err != nil {
			return res, &Error{"home_assistant", err}
		}
		res.CostRows, res.TotalCostEUR = len(cost), cost[len(cost)-1].Sum
	}
	return res, nil
}

// costRows rebuilds the whole cost series from the volume history: rows
// before the window as they were in HA, then the rows just written.
func costRows(cfg Config, existing []ha.Row, windowStart time.Time, vol []ha.Stat) []ha.Stat {
	type day struct {
		start  time.Time
		liters float64
	}
	var days []day
	for _, r := range existing {
		if r.Start.Before(windowStart) {
			days = append(days, day{r.Start, r.State})
		}
	}
	for _, s := range vol {
		days = append(days, day{s.Start, s.State})
	}
	var out []ha.Stat
	sum := 0.0
	for _, d := range days {
		price, ok := cfg.Prices.At(egl.DateOf(d.start.In(cfg.Location)))
		c := 0.0
		if ok {
			c = d.liters / 1000 * price
		}
		sum += c
		out = append(out, ha.Stat{Start: d.start, State: c, Sum: sum})
	}
	return out
}

func fetch(ctx context.Context, e EGL, from, to egl.Date, chunk int) ([]egl.Reading, error) {
	var all []egl.Reading
	seen := map[egl.Date]int{}
	for start := from; !to.Before(start); start = start.AddDays(chunk) {
		end := start.AddDays(chunk - 1)
		if to.Before(end) {
			end = to
		}
		rs, err := e.Readings(ctx, start, end)
		if err != nil {
			return nil, fmt.Errorf("readings %s..%s: %w", start, end, err)
		}
		for _, r := range rs {
			if r.Date.Before(from) || to.Before(r.Date) {
				continue // outside what we asked for
			}
			if i, ok := seen[r.Date]; ok {
				all[i] = r
				continue
			}
			seen[r.Date] = len(all)
			all = append(all, r)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Date.Before(all[j].Date) })
	return all, nil
}

func lastSum(rows []ha.Row) float64 {
	if len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].Sum
}

// IsAuth reports whether err is a credentials problem on either side, which
// retrying won't fix.
func IsAuth(err error) bool {
	return errors.Is(err, egl.ErrInvalidCredentials) || errors.Is(err, ha.ErrAuth)
}
