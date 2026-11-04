// Package config loads egl2haws settings from environment variables and, when
// running as a Home Assistant add-on, from /data/options.json.
// Environment variables win over the options file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/egl"
	"github.com/D34THWINGS/egl2haws/internal/importer"
)

// AddonOptionsPath is where the Supervisor writes add-on options.
const AddonOptionsPath = "/data/options.json"

// supervisorWS is the Core websocket as seen from an add-on.
const supervisorWS = "ws://supervisor/core/websocket"

// Config is the resolved configuration.
type Config struct {
	EGLBaseURL  string
	Username    string
	Password    string
	Contract    string
	HAURL       string
	HAToken     string
	Name        string
	Prices      importer.PriceSchedule
	UpdateTimes []TimeOfDay
	Location    *time.Location
	HistoryDays int
	RefreshDays int
	LogLevel    string
	LogFormat   string
}

// TimeOfDay is a local "HH:MM".
type TimeOfDay struct{ Hour, Minute int }

func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// options mirrors the add-on schema (config.yaml in the add-on folder).
type options struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	Contract    string `json:"contract"`
	Name        string `json:"name"`
	PricePerM3  string `json:"price_per_m3"`
	UpdateTimes string `json:"update_times"`
	HistoryDays int    `json:"history_days"`
	RefreshDays int    `json:"refresh_days"`
	LogLevel    string `json:"log_level"`
}

// Load resolves the configuration. getenv is os.Getenv in production.
func Load(getenv func(string) string, optionsPath string) (Config, error) {
	var opts options
	if b, err := os.ReadFile(optionsPath); err == nil {
		if err := json.Unmarshal(b, &opts); err != nil {
			return Config{}, fmt.Errorf("%s: %w", optionsPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%s: %w", optionsPath, err)
	}

	pick := func(env, opt, def string) string {
		if v := strings.TrimSpace(getenv(env)); v != "" {
			return v
		}
		if strings.TrimSpace(opt) != "" {
			return strings.TrimSpace(opt)
		}
		return def
	}
	pickInt := func(env string, opt, def int) (int, error) {
		if v := strings.TrimSpace(getenv(env)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%s: %q is not a positive integer", env, v)
			}
			return n, nil
		}
		if opt > 0 {
			return opt, nil
		}
		return def, nil
	}

	var errs []error
	c := Config{
		EGLBaseURL: pick("EGL_BASE_URL", "", egl.DefaultBaseURL),
		Username:   pick("EGL_USERNAME", opts.Username, ""),
		Password:   getenvOr(getenv, "EGL_PASSWORD", opts.Password),
		Contract:   pick("EGL_CONTRACT", opts.Contract, ""),
		Name:       pick("NAME", opts.Name, "EGL"),
		LogLevel:   strings.ToLower(pick("LOG_LEVEL", opts.LogLevel, "info")),
		LogFormat:  strings.ToLower(pick("LOG_FORMAT", "", "json")),
	}
	if c.Username == "" {
		errs = append(errs, errors.New("EGL_USERNAME is required"))
	}
	if c.Password == "" {
		errs = append(errs, errors.New("EGL_PASSWORD is required"))
	}

	supervisorToken := getenv("SUPERVISOR_TOKEN")
	c.HAToken = pick("HA_TOKEN", "", supervisorToken)
	if c.HAToken == "" {
		errs = append(errs, errors.New("HA_TOKEN is required (a Home Assistant long-lived access token)"))
	}
	rawURL := pick("HA_URL", "", "")
	if rawURL == "" && supervisorToken != "" {
		rawURL = supervisorWS
	}
	if rawURL == "" {
		errs = append(errs, errors.New("HA_URL is required (e.g. http://homeassistant:8123)"))
	} else if u, err := websocketURL(rawURL); err != nil {
		errs = append(errs, err)
	} else {
		c.HAURL = u
	}

	var err error
	if c.Prices, err = importer.ParsePrices(pick("PRICE_PER_M3", opts.PricePerM3, "")); err != nil {
		errs = append(errs, fmt.Errorf("PRICE_PER_M3: %w", err))
	}
	if c.UpdateTimes, err = parseTimes(pick("UPDATE_TIMES", opts.UpdateTimes, "08:00,16:00")); err != nil {
		errs = append(errs, fmt.Errorf("UPDATE_TIMES: %w", err))
	}
	tz := pick("TZ", "", "Europe/Paris")
	if c.Location, err = time.LoadLocation(tz); err != nil {
		errs = append(errs, fmt.Errorf("TZ: %w", err))
	}
	if c.HistoryDays, err = pickInt("HISTORY_DAYS", opts.HistoryDays, 730); err != nil {
		errs = append(errs, err)
	}
	if c.RefreshDays, err = pickInt("REFRESH_DAYS", opts.RefreshDays, 90); err != nil {
		errs = append(errs, err)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL: unknown level %q", c.LogLevel))
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT: must be json or text, got %q", c.LogFormat))
	}
	return c, errors.Join(errs...)
}

// getenvOr doesn't trim: passwords may legitimately contain spaces.
func getenvOr(getenv func(string) string, env, opt string) string {
	if v := getenv(env); v != "" {
		return v
	}
	return opt
}

// websocketURL accepts the HA base URL (http://host:8123) or the websocket
// URL itself.
func websocketURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("HA_URL: %q is not a URL", raw)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("HA_URL: unsupported scheme %q", u.Scheme)
	}
	if p := strings.TrimSuffix(u.Path, "/"); !strings.HasSuffix(p, "/websocket") {
		u.Path = p + "/api/websocket"
	}
	return u.String(), nil
}

func parseTimes(s string) ([]TimeOfDay, error) {
	var out []TimeOfDay
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		t, err := time.Parse("15:04", part)
		if err != nil {
			return nil, fmt.Errorf("%q is not HH:MM", part)
		}
		out = append(out, TimeOfDay{t.Hour(), t.Minute()})
	}
	if len(out) == 0 {
		return nil, errors.New("at least one time is required")
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Hour*60+out[i].Minute < out[j].Hour*60+out[j].Minute
	})
	return out, nil
}

// Next returns the first scheduled time strictly after now, in loc.
func Next(now time.Time, times []TimeOfDay, loc *time.Location) time.Time {
	local := now.In(loc)
	for day := 0; day < 2; day++ {
		y, m, d := local.AddDate(0, 0, day).Date()
		for _, t := range times {
			at := time.Date(y, m, d, t.Hour, t.Minute, 0, 0, loc)
			if at.After(now) {
				return at
			}
		}
	}
	// Unreachable with at least one time; fall back to a day later.
	return now.Add(24 * time.Hour)
}
