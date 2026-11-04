package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/config"
	"github.com/D34THWINGS/egl2haws/internal/fake"
	"github.com/D34THWINGS/egl2haws/internal/importer"
)

// TestSyncEndToEnd runs a real sync against the fake EGL website and the
// fake Home Assistant, and checks the log line monitors rely on.
func TestSyncEndToEnd(t *testing.T) {
	e := fake.NewEGL(t)
	h := fake.NewHA(t)
	paris, _ := time.LoadLocation("Europe/Paris")
	yesterday := time.Now().In(paris).AddDate(0, 0, -1)
	before := yesterday.AddDate(0, 0, -1)
	e.SetDays(map[string]float64{
		before.Format("2006-01-02"):    0.180,
		yesterday.Format("2006-01-02"): 0.120,
	})

	prices, _ := importer.ParsePrices("4")
	cfg := config.Config{
		EGLBaseURL: e.URL, Username: fake.EGLUsername, Password: fake.EGLPassword,
		HAURL: h.URL, HAToken: fake.HAToken, Name: "EGL", Prices: prices,
		Location: paris, HistoryDays: 730, RefreshDays: 90, LogFormat: "json", LogLevel: "info",
	}
	var logs bytes.Buffer
	if err := syncOnce(context.Background(), cfg, newLogger(cfg, &logs)); err != nil {
		t.Fatalf("sync failed: %v\nlogs: %s", err, logs.String())
	}

	vol := h.Rows("egl2haws:egl_volume")
	if len(vol) != 2 || vol[1].Sum != 300 || vol[1].State != 120 {
		t.Fatalf("volume rows %+v", vol)
	}
	cost := h.Rows("egl2haws:egl_cost")
	if len(cost) != 2 || cost[1].Sum != 1.2 {
		t.Fatalf("cost rows %+v", cost)
	}

	line := lastLog(t, logs.String())
	if line["msg"] != "sync finished" || line["outcome"] != "success" || line["event"] != "sync" ||
		line["service"] != "egl2haws" || line["level"] != "INFO" || line["readings"] != float64(2) ||
		line["last_reading"] != yesterday.Format("2006-01-02") || line["first_run"] != true {
		t.Errorf("unexpected log line %v", line)
	}
	// Second run: a refresh, with yesterday corrected by EGL.
	e.SetDays(map[string]float64{
		before.Format("2006-01-02"):    0.180,
		yesterday.Format("2006-01-02"): 0.150,
	})
	logs.Reset()
	if err := syncOnce(context.Background(), cfg, newLogger(cfg, &logs)); err != nil {
		t.Fatalf("second sync failed: %v\nlogs: %s", err, logs.String())
	}
	vol = h.Rows("egl2haws:egl_volume")
	if line := lastLog(t, logs.String()); line["first_run"] != false || len(vol) != 2 || vol[1].Sum != 330 {
		t.Fatalf("refresh: log %v rows %+v", line, vol)
	}

	if strings.Contains(logs.String(), fake.EGLPassword) || strings.Contains(logs.String(), fake.HAToken+`"`) {
		t.Error("secrets in logs")
	}
}

func TestSyncFailureIsClassified(t *testing.T) {
	e := fake.NewEGL(t)
	h := fake.NewHA(t)
	paris, _ := time.LoadLocation("Europe/Paris")
	for _, tc := range []struct {
		name     string
		setup    func(*config.Config)
		system   string
		kind     string
		step     string
		readings string
	}{
		{"bad EGL password", func(c *config.Config) { c.Password = "nope" }, "egl", "auth", "login", ""},
		{"bad HA token", func(c *config.Config) { c.HAToken = "nope" }, "home_assistant", "auth", "", ""},
		{"EGL changed format", nil, "egl", "egl_api_changed", "readings", `{"postes":[{"data":[{"consommation":1,"annee":2026,"mois":40,"jour":1}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.readings != "" {
				e.SetReadings(tc.readings)
			}
			cfg := config.Config{
				EGLBaseURL: e.URL, Username: fake.EGLUsername, Password: fake.EGLPassword,
				HAURL: h.URL, HAToken: fake.HAToken, Name: "EGL", Location: paris,
				HistoryDays: 30, RefreshDays: 10, LogFormat: "json", LogLevel: "info",
			}
			if tc.setup != nil {
				tc.setup(&cfg)
			}
			var logs bytes.Buffer
			if err := syncOnce(context.Background(), cfg, newLogger(cfg, &logs)); err == nil {
				t.Fatal("expected failure")
			}
			line := lastLog(t, logs.String())
			if line["outcome"] != "failure" || line["level"] != "ERROR" || line["error_system"] != tc.system || line["error_kind"] != tc.kind {
				t.Errorf("unexpected log line %v", line)
			}
			if tc.step != "" && line["error_step"] != tc.step {
				t.Errorf("error_step = %v, want %s", line["error_step"], tc.step)
			}
		})
	}
}

func lastLog(t *testing.T, logs string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, logs)
	}
	return m
}
