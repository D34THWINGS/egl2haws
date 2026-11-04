// Package ha is a minimal Home Assistant websocket API client, limited to what
// egl2haws needs: importing external statistics and reading them back.
package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ErrAuth is returned when Home Assistant rejects the access token.
var ErrAuth = errors.New("home assistant rejected the access token")

// Client is a connected Home Assistant websocket session. Not safe for
// concurrent use.
type Client struct {
	conn *websocket.Conn
	id   int
	// HAVersion is reported by HA during the handshake.
	HAVersion string
}

// Dial connects to url (e.g. ws://homeassistant:8123/api/websocket) and
// authenticates with a long-lived access token (or the add-on Supervisor
// token).
func Dial(ctx context.Context, url, token string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", url, err)
	}
	conn.SetReadLimit(64 << 20) // statistics responses can be large
	c := &Client{conn: conn}
	if err := c.auth(ctx, token); err != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil, err
	}
	return c, nil
}

type message struct {
	ID        int             `json:"id"`
	Type      string          `json:"type"`
	Success   bool            `json:"success"`
	Result    json.RawMessage `json:"result"`
	HAVersion string          `json:"ha_version"`
	Message   string          `json:"message"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) auth(ctx context.Context, token string) error {
	var m message
	if err := wsjson.Read(ctx, c.conn, &m); err != nil {
		return fmt.Errorf("read auth_required: %w", err)
	}
	if m.Type != "auth_required" {
		return fmt.Errorf("expected auth_required, got %q", m.Type)
	}
	if err := wsjson.Write(ctx, c.conn, map[string]string{"type": "auth", "access_token": token}); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}
	if err := wsjson.Read(ctx, c.conn, &m); err != nil {
		return fmt.Errorf("read auth result: %w", err)
	}
	switch m.Type {
	case "auth_ok":
		c.HAVersion = m.HAVersion
		return nil
	case "auth_invalid":
		return fmt.Errorf("%w: %s", ErrAuth, m.Message)
	default:
		return fmt.Errorf("unexpected auth response %q", m.Type)
	}
}

// Close ends the session.
func (c *Client) Close() error { return c.conn.Close(websocket.StatusNormalClosure, "") }

// CommandError is an error reported by Home Assistant for a command.
type CommandError struct {
	Command string
	Code    string
	Message string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("home assistant %s: %s: %s", e.Command, e.Code, e.Message)
}

func (c *Client) call(ctx context.Context, msg map[string]any, out any) error {
	c.id++
	id := c.id
	msg["id"] = id
	cmd, _ := msg["type"].(string)
	if err := wsjson.Write(ctx, c.conn, msg); err != nil {
		return fmt.Errorf("send %s: %w", cmd, err)
	}
	for {
		var m message
		if err := wsjson.Read(ctx, c.conn, &m); err != nil {
			return fmt.Errorf("read %s result: %w", cmd, err)
		}
		if m.Type != "result" || m.ID != id {
			continue // events or stale messages
		}
		if !m.Success {
			ce := &CommandError{Command: cmd, Code: "unknown_error"}
			if m.Error != nil {
				ce.Code, ce.Message = m.Error.Code, m.Error.Message
			}
			return ce
		}
		if out == nil || len(m.Result) == 0 || string(m.Result) == "null" {
			return nil
		}
		if err := json.Unmarshal(m.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", cmd, err)
		}
		return nil
	}
}

// Metadata describes an external statistic. HA >= 2026.11 requires MeanType
// and UnitClass to be sent explicitly.
type Metadata struct {
	StatisticID       string
	Source            string
	Name              string
	UnitOfMeasurement string
	UnitClass         string // empty means null (e.g. currency)
}

// Stat is one hourly statistics row. Start must be on the hour.
type Stat struct {
	Start time.Time
	State float64
	Sum   float64
}

// importBatch keeps each websocket message well under HA's size limits.
const importBatch = 500

// ImportStatistics creates or overwrites rows of an external statistic.
func (c *Client) ImportStatistics(ctx context.Context, md Metadata, stats []Stat) error {
	var unitClass any
	if md.UnitClass != "" {
		unitClass = md.UnitClass
	}
	meta := map[string]any{
		"has_mean":            false, // deprecated, still sent for older HA
		"mean_type":           0,
		"has_sum":             true,
		"name":                md.Name,
		"source":              md.Source,
		"statistic_id":        md.StatisticID,
		"unit_of_measurement": md.UnitOfMeasurement,
		"unit_class":          unitClass,
	}
	for i := 0; i < len(stats); i += importBatch {
		end := min(i+importBatch, len(stats))
		rows := make([]map[string]any, 0, end-i)
		for _, s := range stats[i:end] {
			if s.Start.Minute() != 0 || s.Start.Second() != 0 || s.Start.Nanosecond() != 0 {
				return fmt.Errorf("statistic start %s is not on the hour", s.Start)
			}
			rows = append(rows, map[string]any{
				"start": s.Start.Format(time.RFC3339),
				"state": round(s.State),
				"sum":   round(s.Sum),
			})
		}
		err := c.call(ctx, map[string]any{"type": "recorder/import_statistics", "metadata": meta, "stats": rows}, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// Row is a statistics row read back from Home Assistant.
type Row struct {
	Start time.Time
	State float64
	Sum   float64
}

// Rows returns the hourly rows of a statistic with start in [from, to).
func (c *Client) Rows(ctx context.Context, statisticID string, from, to time.Time) ([]Row, error) {
	var res map[string][]struct {
		Start json.RawMessage `json:"start"`
		State *float64        `json:"state"`
		Sum   *float64        `json:"sum"`
	}
	err := c.call(ctx, map[string]any{
		"type":          "recorder/statistics_during_period",
		"start_time":    from.UTC().Format(time.RFC3339),
		"end_time":      to.UTC().Format(time.RFC3339),
		"statistic_ids": []string{statisticID},
		"period":        "hour",
		"types":         []string{"state", "sum"},
	}, &res)
	if err != nil {
		return nil, err
	}
	var out []Row
	for _, r := range res[statisticID] {
		start, err := parseStart(r.Start)
		if err != nil {
			return nil, err
		}
		row := Row{Start: start}
		if r.State != nil {
			row.State = *r.State
		}
		if r.Sum != nil {
			row.Sum = *r.Sum
		}
		out = append(out, row)
	}
	return out, nil
}

// parseStart accepts both formats HA has used: epoch milliseconds (current)
// and ISO 8601 strings (older releases).
func parseStart(raw json.RawMessage) (time.Time, error) {
	var ms float64
	if err := json.Unmarshal(raw, &ms); err == nil {
		return time.UnixMilli(int64(ms)).UTC(), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, fmt.Errorf("unexpected start %s", raw)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("unexpected start %q: %w", s, err)
	}
	return t.UTC(), nil
}

// round avoids float noise like 1234.0000000002 in HA's database.
func round(f float64) float64 { return math.Round(f*1e6) / 1e6 }
