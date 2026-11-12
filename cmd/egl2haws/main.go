// Command egl2haws imports Eau du Grand Lyon water consumption into Home
// Assistant's long-term statistics (Energy dashboard, Water section).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo

	"github.com/D34THWINGS/egl2haws/internal/config"
	"github.com/D34THWINGS/egl2haws/internal/dump"
	"github.com/D34THWINGS/egl2haws/internal/egl"
	"github.com/D34THWINGS/egl2haws/internal/ha"
	"github.com/D34THWINGS/egl2haws/internal/importer"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// retryDelays applies to transient failures between scheduled runs.
var retryDelays = []time.Duration{15 * time.Minute, time.Hour}

const usage = `egl2haws imports Eau du Grand Lyon water consumption into Home Assistant.

Usage:
  egl2haws [run]       sync now, then at each UPDATE_TIMES (default)
  egl2haws once        sync once and exit (non-zero on failure)
  egl2haws dump [-out DIR] [-days N]
                       log in to EGL and save its responses, with personal
                       data removed, for bug reports and test fixtures
  egl2haws version

Configuration is read from environment variables (see README), or from
/data/options.json when running as a Home Assistant add-on.
`

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(version)
		return
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return
	case "run", "once", "dump":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if cmd == "dump" {
		os.Exit(runDump(ctx, args))
	}

	cfg, err := config.Load(os.Getenv, config.AddonOptionsPath)
	log := newLogger(cfg, os.Stdout)
	if err != nil {
		log.Error("invalid configuration", "error", err.Error())
		os.Exit(2)
	}
	pub, err := newLeakPublisher(cfg)
	if err != nil {
		log.Error("invalid configuration", "error", "leak detection: "+err.Error())
		os.Exit(2)
	}
	log.Info("egl2haws starting", "version", version, "mode", cmd,
		"volume_statistic", importerConfig(cfg).VolumeID(), "cost_tracking", len(cfg.Prices) > 0,
		"leak_detection", cfg.LeakDetection,
		"update_times", fmt.Sprint(cfg.UpdateTimes), "timezone", cfg.Location.String())

	if cmd == "once" {
		if err := syncOnce(ctx, cfg, log, pub); err != nil {
			os.Exit(1)
		}
		return
	}
	loop(ctx, cfg, log, pub)
}

func loop(ctx context.Context, cfg config.Config, log *slog.Logger, pub *leakPublisher) {
	attempt := 0
	next := time.Now()
	for {
		// Between syncs, wake up to re-set the leak entities, which Home
		// Assistant forgets on restart.
		for {
			wake := next
			if due := pub.due(); !due.IsZero() && due.Before(wake) {
				wake = due
			}
			if !sleepUntil(ctx, wake) {
				log.Info("egl2haws stopping")
				return
			}
			if !time.Now().Before(next) {
				break
			}
			if err := pub.publish(ctx); err != nil {
				log.Error("leak entities not published", "event", "leak_publish", "error", err.Error())
				pub.published = time.Now() // retry at the next interval, not in a tight loop
			}
		}
		err := syncOnce(ctx, cfg, log, pub)
		scheduled := config.Next(time.Now(), cfg.UpdateTimes, cfg.Location)
		next = scheduled
		if err != nil && !importer.IsAuth(err) && attempt < len(retryDelays) {
			if retry := time.Now().Add(retryDelays[attempt]); retry.Before(scheduled) {
				next = retry
			}
			attempt++
		} else {
			attempt = 0
		}
		log.Info("next sync scheduled", "at", next.In(cfg.Location).Format(time.RFC3339))
	}
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func importerConfig(cfg config.Config) importer.Config {
	return importer.Config{
		Name:        cfg.Name,
		Location:    cfg.Location,
		Prices:      cfg.Prices,
		HistoryDays: cfg.HistoryDays,
		RefreshDays: cfg.RefreshDays,
		ChunkDays:   90,
	}
}

// syncOnce runs one sync and logs exactly one "sync finished" line with
// outcome=success|failure, the line monitors should key on.
func syncOnce(ctx context.Context, cfg config.Config, log *slog.Logger, pub *leakPublisher) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	e := egl.New(cfg.Username, cfg.Password)
	e.BaseURL = cfg.EGLBaseURL
	e.ContractID = cfg.Contract
	res, err := func() (importer.Result, error) {
		h, err := ha.Dial(ctx, cfg.HAURL, cfg.HAToken)
		if err != nil {
			return importer.Result{}, &importer.Error{System: "home_assistant", Err: err}
		}
		defer h.Close()
		return importer.Run(ctx, importerConfig(cfg), time.Now(), e, h)
	}()

	attrs := []any{
		"event", "sync",
		"duration_ms", time.Since(start).Milliseconds(),
		"first_run", res.FirstRun,
		"range_from", res.From.String(),
		"range_to", res.To.String(),
		"readings", res.Readings,
	}
	if err != nil {
		attrs = append(attrs, "outcome", "failure", "error", err.Error())
		attrs = append(attrs, errorAttrs(err)...)
		log.Error("sync finished", attrs...)
		return err
	}
	attrs = append(attrs, "outcome", "success", "volume_rows", res.VolumeRows, "cost_rows", res.CostRows,
		"total_liters", res.TotalLiters, "total_cost_eur", res.TotalCostEUR, "leak_rows", res.LeakRows)
	if res.Readings > 0 {
		lag := int(time.Since(res.LastReading.In(cfg.Location)).Hours() / 24)
		attrs = append(attrs, "last_reading", res.LastReading.String(), "last_reading_liters", res.LastLiters, "publication_lag_days", lag,
			"egl_units", fmt.Sprintf("consommation=%s debitMin=%s volumeEstimeFuite=%s", e.Units.Consommation, e.Units.DebitMin, e.Units.VolumeEstimeFuite))
	}
	attrs = append(attrs, pub.evaluate(ctx, res, log)...)
	log.Info("sync finished", attrs...)
	return nil
}

// errorAttrs classifies a failure for alerting: which system, which step,
// and whether it's an auth problem, an API change, or a network issue.
func errorAttrs(err error) []any {
	attrs := []any{}
	var ie *importer.Error
	if errors.As(err, &ie) {
		attrs = append(attrs, "error_system", ie.System)
	}
	var ee *egl.Error
	if errors.As(err, &ee) {
		attrs = append(attrs, "error_step", ee.Step, "error_http_status", ee.Status)
	}
	kind := "unknown"
	switch {
	case importer.IsAuth(err):
		kind = "auth"
	case ee != nil && ee.Status == 0:
		kind = "network"
	case ee != nil && ee.Status >= 500:
		kind = "egl_unavailable"
	case ee != nil:
		kind = "egl_api_changed" // unexpected status or payload: likely needs a code fix
	case ie != nil && ie.System == "home_assistant":
		kind = "home_assistant"
	}
	return append(attrs, "error_kind", kind)
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts)).With("service", "egl2haws", "version", version)
}

func runDump(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	out := fs.String("out", "egl-dump", "directory to write responses to")
	days := fs.Int("days", 45, "days of readings to request")
	_ = fs.Parse(args)

	user, pass := os.Getenv("EGL_USERNAME"), os.Getenv("EGL_PASSWORD")
	if user == "" || pass == "" {
		fmt.Fprintln(os.Stderr, "EGL_USERNAME and EGL_PASSWORD are required")
		return 2
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rec := &dump.Recorder{Dir: *out, Secrets: []string{user}}
	e := egl.New(user, pass)
	if u := os.Getenv("EGL_BASE_URL"); u != "" {
		e.BaseURL = u
	}
	e.ContractID = os.Getenv("EGL_CONTRACT")
	e.OnResponse = rec.Record

	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		loc = time.UTC
	}
	today := egl.DateOf(time.Now().In(loc))
	contract, err := e.Contract(ctx)
	if contract != "" {
		rec.Secrets = append(rec.Secrets, contract)
	}
	var readings []egl.Reading
	if err == nil {
		readings, err = e.Readings(ctx, today.AddDays(-*days), today)
	}
	if cerr := rec.Close(); cerr != nil {
		fmt.Fprintln(os.Stderr, "writing dump:", cerr)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed: %v\nResponses so far are in %s (see index.txt).\n", err, *out)
		return 1
	}
	fmt.Printf("OK: %d daily readings", len(readings))
	if n := len(readings); n > 0 {
		fmt.Printf(", %s to %s", readings[0].Date, readings[n-1].Date)
	}
	fmt.Printf(".\nRedacted responses are in %s (see index.txt). Review them before sharing.\n", *out)
	return 0
}
