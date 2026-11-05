package ha

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/fake"
)

func TestImportAndReadBack(t *testing.T) {
	f := fake.NewHA(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, f.URL, fake.HAToken)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.HAVersion != "2026.11.0" {
		t.Errorf("version %q", c.HAVersion)
	}
	loc, _ := time.LoadLocation("Europe/Paris")
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, loc)
	var stats []Stat
	for i := 0; i < 1200; i++ { // more than one batch
		stats = append(stats, Stat{Start: start.AddDate(0, 0, i), State: 1.1, Sum: 1.1 * float64(i+1)})
	}
	md := Metadata{StatisticID: "egl2haws:home_volume", Source: "egl2haws", Name: "Home water", UnitOfMeasurement: "L", UnitClass: "volume"}
	if err := c.ImportStatistics(ctx, md, stats); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Rows(md.StatisticID)); n != 1200 {
		t.Fatalf("stored %d rows", n)
	}
	if m := f.Metadata(md.StatisticID); m["mean_type"] != float64(0) || m["unit_class"] != "volume" || m["has_sum"] != true {
		t.Errorf("metadata sent %v", m)
	}
	rows, err := c.Rows(ctx, md.StatisticID, start, start.AddDate(10, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1200 || !rows[1].Start.Equal(start.AddDate(0, 0, 1)) || rows[1].Sum != 2.2 {
		t.Fatalf("read back %d rows, second=%+v", len(rows), rows[1])
	}
	if !rows[0].Start.Equal(start) {
		t.Errorf("first row at %s, want local midnight %s", rows[0].Start, start)
	}
	// Cost statistics have no unit class: it must be sent as null.
	if err := c.ImportStatistics(ctx, Metadata{StatisticID: "egl2haws:home_cost", Source: "egl2haws", UnitOfMeasurement: "EUR"}, stats[:1]); err != nil {
		t.Fatal(err)
	}
	if m := f.Metadata("egl2haws:home_cost"); m["unit_class"] != nil {
		t.Errorf("cost unit_class = %v, want null", m["unit_class"])
	}
}

func TestRejectsBadStartAndReportsErrors(t *testing.T) {
	f := fake.NewHA(t)
	ctx := context.Background()
	c, err := Dial(ctx, f.URL, fake.HAToken)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.ImportStatistics(ctx, Metadata{StatisticID: "a:b", Source: "a"}, []Stat{{Start: time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)}})
	if err == nil || !strings.Contains(err.Error(), "not on the hour") {
		t.Errorf("expected start validation error, got %v", err)
	}
	var ce *CommandError
	err = c.call(ctx, map[string]any{"type": "nope"}, nil)
	if !errors.As(err, &ce) || ce.Code != "unknown_command" {
		t.Errorf("expected command error, got %v", err)
	}
}

func TestBadToken(t *testing.T) {
	_, err := Dial(context.Background(), fake.NewHA(t).URL, "bad")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
}

func TestParseStart(t *testing.T) {
	a, err := parseStart([]byte(`1793487600000`))
	if err != nil || !a.Equal(time.UnixMilli(1793487600000)) {
		t.Fatal(a, err)
	}
	b, err := parseStart([]byte(`"2026-10-31T23:00:00+00:00"`))
	if err != nil || !b.Equal(a) {
		t.Fatal(b, err)
	}
}

func TestImportMeanStatistic(t *testing.T) {
	f := fake.NewHA(t)
	ctx := context.Background()
	c, err := Dial(ctx, f.URL, fake.HAToken)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	md := Metadata{StatisticID: "egl2haws:home_min_flow", Source: "egl2haws", UnitOfMeasurement: "L/h", Mean: true}
	if err := c.ImportStatistics(ctx, md, []Stat{{Start: start, State: 6}}); err != nil {
		t.Fatal(err)
	}
	m := f.Metadata(md.StatisticID)
	if m["mean_type"] != float64(1) || m["has_mean"] != true || m["has_sum"] != false || m["unit_class"] != nil {
		t.Errorf("metadata %v", m)
	}
	if rows := f.Rows(md.StatisticID); len(rows) != 1 || rows[0].Mean != 6 {
		t.Errorf("rows %+v", rows)
	}
}

func TestRESTBase(t *testing.T) {
	for in, want := range map[string]string{
		"ws://ha:8123/api/websocket":                      "http://ha:8123/api",
		"wss://ha.example.com/api/websocket":              "https://ha.example.com/api",
		"ws://supervisor/core/websocket":                  "http://supervisor/core/api",
		"ws://home-assistant.home.svc:8080/api/websocket": "http://home-assistant.home.svc:8080/api",
	} {
		if got, err := RESTBase(in); err != nil || got != want {
			t.Errorf("RESTBase(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := RESTBase("http://ha:8123"); err == nil {
		t.Error("expected error for a non-websocket URL")
	}
}

func TestSetState(t *testing.T) {
	f := fake.NewHA(t)
	base, err := RESTBase(f.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := States{Base: base, Token: fake.HAToken}
	if err := s.Set(ctx, "binary_sensor.egl_water_leak", "on", map[string]any{"device_class": "moisture"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "binary_sensor.egl_water_leak", "off", nil); err != nil { // update: 200 instead of 201
		t.Fatal(err)
	}
	if st, ok := f.EntityState("binary_sensor.egl_water_leak"); !ok || st.State != "off" {
		t.Fatalf("state %+v", st)
	}
	bad := States{Base: base, Token: "nope"}
	if err := bad.Set(ctx, "binary_sensor.x", "on", nil); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
}
