package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadFromEnv(t *testing.T) {
	c, err := Load(env(map[string]string{
		"EGL_USERNAME": "me@example.com",
		"EGL_PASSWORD": " pass with spaces ",
		"HA_URL":       "http://home-assistant.home.svc.cluster.local:8080",
		"HA_TOKEN":     "tok",
		"PRICE_PER_M3": "3.56",
		"UPDATE_TIMES": "16:30, 07:05",
		"NAME":         "Maison",
	}), filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.HAURL != "ws://home-assistant.home.svc.cluster.local:8080/api/websocket" {
		t.Errorf("HAURL %q", c.HAURL)
	}
	if c.Password != " pass with spaces " {
		t.Errorf("password must not be trimmed: %q", c.Password)
	}
	if len(c.UpdateTimes) != 2 || c.UpdateTimes[0] != (TimeOfDay{7, 5}) {
		t.Errorf("update times not sorted: %v", c.UpdateTimes)
	}
	if c.Location.String() != "Europe/Paris" || c.HistoryDays != 730 || c.RefreshDays != 90 || len(c.Prices) != 1 {
		t.Errorf("defaults: %+v", c)
	}
	if !strings.HasPrefix(c.EGLBaseURL, "https://agence.eaudugrandlyon.com") {
		t.Errorf("EGLBaseURL %q", c.EGLBaseURL)
	}
}

func TestLoadAsAddon(t *testing.T) {
	p := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(p, []byte(`{"username":"addon@example.com","password":"pw","price_per_m3":"4.1","update_times":"09:00","history_days":365}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{"SUPERVISOR_TOKEN": "sup", "EGL_USERNAME": "override@example.com"}), p)
	if err != nil {
		t.Fatal(err)
	}
	if c.HAURL != "ws://supervisor/core/websocket" || c.HAToken != "sup" {
		t.Errorf("add-on HA connection: %q %q", c.HAURL, c.HAToken)
	}
	if c.Username != "override@example.com" || c.Password != "pw" || c.HistoryDays != 365 {
		t.Errorf("env should override options: %+v", c)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := Load(env(map[string]string{"PRICE_PER_M3": "abc", "UPDATE_TIMES": "25:00", "TZ": "Mars/Olympus", "HISTORY_DAYS": "0"}), "/nonexistent/options.json")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"EGL_USERNAME", "EGL_PASSWORD", "HA_TOKEN", "HA_URL", "PRICE_PER_M3", "UPDATE_TIMES", "TZ", "HISTORY_DAYS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s in: %v", want, err)
		}
	}
}

func TestWebsocketURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://ha:8123":                 "ws://ha:8123/api/websocket",
		"https://ha.example.com/":        "wss://ha.example.com/api/websocket",
		"ws://ha:8123/api/websocket":     "ws://ha:8123/api/websocket",
		"ws://supervisor/core/websocket": "ws://supervisor/core/websocket",
	} {
		if got, err := websocketURL(in); err != nil || got != want {
			t.Errorf("%s: got %q, %v", in, got, err)
		}
	}
	if _, err := websocketURL("ftp://x"); err == nil {
		t.Error("expected scheme error")
	}
}

func TestNext(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	times := []TimeOfDay{{8, 0}, {16, 0}}
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 11, 4, 7, 0, 0, 0, paris), time.Date(2026, 11, 4, 8, 0, 0, 0, paris)},
		{time.Date(2026, 11, 4, 8, 0, 0, 0, paris), time.Date(2026, 11, 4, 16, 0, 0, 0, paris)},
		{time.Date(2026, 11, 4, 20, 0, 0, 0, paris), time.Date(2026, 11, 5, 8, 0, 0, 0, paris)},
		// Across the DST change (Mar 29, 2026): still 08:00 local.
		{time.Date(2026, 3, 28, 20, 0, 0, 0, paris), time.Date(2026, 3, 29, 8, 0, 0, 0, paris)},
	} {
		if got := Next(tc.now, times, paris); !got.Equal(tc.want) {
			t.Errorf("Next(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}
