package main

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/D34THWINGS/egl2haws/internal/config"
	"github.com/D34THWINGS/egl2haws/internal/ha"
	"github.com/D34THWINGS/egl2haws/internal/importer"
	"github.com/D34THWINGS/egl2haws/internal/leak"
)

// republishEvery re-sets the leak entities between syncs: entities set
// through the REST API vanish when Home Assistant restarts.
const republishEvery = 30 * time.Minute

// leakPublisher keeps the latest leak status and mirrors it to two Home
// Assistant entities:
//
//   - binary_sensor.<name>_water_leak (device class moisture): on when a rule
//     flags a leak, with the reasons and figures as attributes;
//   - sensor.<name>_water_min_flow: the latest day's minimum flow, in L/h.
type leakPublisher struct {
	states    ha.States
	name      string
	leakID    string
	flowID    string
	cfg       leak.Config
	status    *leak.Status
	published time.Time
}

func newLeakPublisher(cfg config.Config) (*leakPublisher, error) {
	if !cfg.LeakDetection {
		return nil, nil
	}
	base, err := ha.RESTBase(cfg.HAURL)
	if err != nil {
		return nil, err
	}
	slug := importer.Slug(cfg.Name)
	return &leakPublisher{
		states: ha.States{Base: base, Token: cfg.HAToken},
		name:   cfg.Name,
		leakID: "binary_sensor." + slug + "_water_leak",
		flowID: "sensor." + slug + "_water_min_flow",
		cfg:    cfg.Leak,
	}, nil
}

// due reports when the entities should next be re-set, zero if never.
func (p *leakPublisher) due() time.Time {
	if p == nil || p.status == nil {
		return time.Time{}
	}
	return p.published.Add(republishEvery)
}

func (p *leakPublisher) publish(ctx context.Context) error {
	if p == nil || p.status == nil {
		return nil
	}
	st := p.status
	state := "off"
	if st.Leak {
		state = "on"
	}
	reasons := st.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	attrs := map[string]any{
		"friendly_name":           p.name + " water leak",
		"device_class":            "moisture",
		"reasons":                 reasons,
		"last_reading":            st.Date.String(),
		"daily_liters":            st.Liters,
		"baseline_liters":         st.BaselineLiters,
		"continuous_flow_days":    st.FlowDays,
		"min_flow_threshold_l_h":  p.cfg.MinFlowLH,
		"continuous_flow_trigger": p.cfg.Days,
		"source":                  "egl2haws",
	}
	if st.MinFlowLH != nil {
		attrs["min_flow_l_h"] = *st.MinFlowLH
	}
	if st.EGLEstimateLiters != nil {
		attrs["egl_leak_estimate_l"] = *st.EGLEstimateLiters
	}
	if err := p.states.Set(ctx, p.leakID, state, attrs); err != nil {
		return err
	}
	if st.MinFlowLH != nil {
		err := p.states.Set(ctx, p.flowID, formatFloat(*st.MinFlowLH), map[string]any{
			"friendly_name":       p.name + " water minimum flow",
			"unit_of_measurement": "L/h",
			"icon":                "mdi:water-alert",
			"last_reading":        st.Date.String(),
			"source":              "egl2haws",
		})
		if err != nil {
			return err
		}
	}
	p.published = time.Now()
	return nil
}

// evaluate updates the status from a sync's readings and publishes it, and
// returns log attributes. Days without readings keep the previous status.
func (p *leakPublisher) evaluate(ctx context.Context, res importer.Result, log *slog.Logger) []any {
	if p == nil || len(res.Fetched) == 0 {
		return nil
	}
	st := leak.Evaluate(res.Fetched, p.cfg)
	p.status = &st
	attrs := []any{"leak", st.Leak, "leak_reasons", strings.Join(st.Reasons, ","), "leak_flow_days", st.FlowDays}
	if st.MinFlowLH != nil {
		attrs = append(attrs, "min_flow_l_h", *st.MinFlowLH)
	}
	if st.EGLEstimateLiters != nil {
		attrs = append(attrs, "egl_leak_estimate_l", *st.EGLEstimateLiters)
	}
	if st.Leak {
		log.Warn("leak detected", append([]any{"event", "leak", "last_reading", st.Date.String(), "daily_liters", st.Liters}, attrs...)...)
	}
	if err := p.publish(ctx); err != nil {
		attrs = append(attrs, "leak_publish_error", err.Error())
		log.Error("leak entities not published", "event", "leak_publish", "error", err.Error())
	}
	return attrs
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
