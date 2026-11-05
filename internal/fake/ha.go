package fake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// HAToken is the access token the fake accepts.
const HAToken = "good"

// HA speaks enough of Home Assistant's websocket API: the auth handshake,
// statistics import (validating what HA >= 2026.11 requires, and storing
// rows like the recorder: one per statistic and start, overwritten on
// re-import) and statistics reads.
type HA struct {
	URL string // ws:// URL

	mu     sync.Mutex
	rows   map[string]map[int64]map[string]any // id -> start ms -> row
	meta   map[string]map[string]any
	states map[string]State
}

// State is an entity state set through the REST API.
type State struct {
	State      string
	Attributes map[string]any
}

// NewHA starts a fake HA server, closed when the test ends.
func NewHA(t testing.TB) *HA {
	f := &HA{rows: map[string]map[int64]map[string]any{}, meta: map[string]map[string]any{}, states: map[string]State{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/states/") {
			f.setState(w, r)
			return
		}
		f.serve(t, w, r)
	}))
	t.Cleanup(srv.Close)
	f.URL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/websocket"
	return f
}

// Row is a stored statistics row. Mean rows have Mean set and no State/Sum.
type Row struct {
	Start            time.Time
	State, Sum, Mean float64
}

func num(row map[string]any, k string) float64 {
	v, _ := row[k].(float64)
	return v
}

// Rows returns the stored rows of a statistic, oldest first.
func (f *HA) Rows(id string) []Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Row
	for ms, r := range f.rows[id] {
		out = append(out, Row{Start: time.UnixMilli(ms).UTC(), State: num(r, "state"), Sum: num(r, "sum"), Mean: num(r, "mean")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// EntityState returns an entity state set through the REST API.
func (f *HA) EntityState(id string) (State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[id]
	return st, ok
}

func (f *HA) setState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+HAToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		State      string         `json:"state"`
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State == "" {
		http.Error(w, `{"message":"State data should be valid JSON"}`, http.StatusBadRequest)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/states/")
	f.mu.Lock()
	_, existed := f.states[id]
	f.states[id] = State{State: body.State, Attributes: body.Attributes}
	f.mu.Unlock()
	if existed {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"entity_id": id, "state": body.State})
}

// Metadata returns the last metadata imported for a statistic.
func (f *HA) Metadata(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meta[id]
}

func (f *HA) serve(t testing.TB, w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20)
	ctx := r.Context()
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "auth_required", "ha_version": "2026.11.0"})
	var auth map[string]any
	if err := wsjson.Read(ctx, conn, &auth); err != nil {
		return
	}
	if auth["access_token"] != HAToken {
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "auth_invalid", "message": "Invalid access token or password"})
		return
	}
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "auth_ok", "ha_version": "2026.11.0"})
	for {
		var msg map[string]any
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		id := msg["id"]
		// An unrelated event first: clients must skip it.
		_ = wsjson.Write(ctx, conn, map[string]any{"id": 999, "type": "event", "event": map[string]any{}})
		reply := func(result any) {
			_ = wsjson.Write(ctx, conn, map[string]any{"id": id, "type": "result", "success": true, "result": result})
		}
		fail := func(code, m string) {
			_ = wsjson.Write(ctx, conn, map[string]any{"id": id, "type": "result", "success": false, "error": map[string]any{"code": code, "message": m}})
		}
		switch msg["type"] {
		case "recorder/import_statistics":
			md, _ := msg["metadata"].(map[string]any)
			if md == nil {
				fail("invalid_format", "metadata required")
				continue
			}
			if _, ok := md["mean_type"]; !ok {
				fail("invalid_format", "mean_type required")
				continue
			}
			if _, ok := md["unit_class"]; !ok {
				fail("invalid_format", "unit_class required")
				continue
			}
			sid, _ := md["statistic_id"].(string)
			if src, _ := md["source"].(string); !strings.HasPrefix(sid, src+":") {
				fail("invalid_format", "statistic_id must start with source")
				continue
			}
			bad := false
			f.mu.Lock()
			f.meta[sid] = md
			if f.rows[sid] == nil {
				f.rows[sid] = map[int64]map[string]any{}
			}
			for _, s := range msg["stats"].([]any) {
				row := s.(map[string]any)
				start, err := time.Parse(time.RFC3339, row["start"].(string))
				if err != nil || start.Minute() != 0 || start.Second() != 0 {
					bad = true
					break
				}
				// Rows must match the statistic's kind.
				_, hasMean := row["mean"]
				_, hasSum := row["sum"]
				if md["mean_type"] == float64(1) && (!hasMean || md["has_sum"] == true) ||
					md["mean_type"] == float64(0) && (!hasSum || md["has_sum"] != true) {
					bad = true
					break
				}
				f.rows[sid][start.UnixMilli()] = row
			}
			f.mu.Unlock()
			if bad {
				fail("invalid_format", "invalid timestamp")
				continue
			}
			reply(nil)
		case "recorder/statistics_during_period":
			sid := msg["statistic_ids"].([]any)[0].(string)
			from, _ := time.Parse(time.RFC3339, msg["start_time"].(string))
			to, _ := time.Parse(time.RFC3339, msg["end_time"].(string))
			var rows []map[string]any
			for _, r := range f.Rows(sid) {
				if !r.Start.Before(from) && r.Start.Before(to) {
					rows = append(rows, map[string]any{"start": r.Start.UnixMilli(), "end": r.Start.Add(time.Hour).UnixMilli(), "state": r.State, "sum": r.Sum, "mean": r.Mean})
				}
			}
			result := map[string]any{}
			if len(rows) > 0 {
				result[sid] = rows
			}
			reply(result)
		default:
			fail("unknown_command", "Unknown command.")
		}
	}
}
