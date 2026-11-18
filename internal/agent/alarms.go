package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Metric alarms: every minute the primary evaluates each enabled rule's PromQL query
// within its organization's series (extra_label=org_id) and compares the worst series
// with the threshold. A breach lasting for_minutes opens an incident (kind
// metric_alarm) and notifies the channels; recovering resolves it. A query without
// data is "nodata" and resolves an open incident.

type alarmRule struct {
	ID, OrgID, Name, Query, Op, State string
	Threshold                         float64
	ForMinutes                        int
	PendingSince                      *time.Time
}

// breaches reports whether v crosses the threshold.
func breaches(v float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	}
	return false
}

// worst is the series value that matters: the highest for > and >=, the lowest otherwise.
func worst(values []float64, op string) float64 {
	w := values[0]
	for _, v := range values[1:] {
		if (op == ">" || op == ">=") && v > w || (op == "<" || op == "<=") && v < w {
			w = v
		}
	}
	return w
}

// evalPromQL runs an instant query restricted to one organization and returns the values.
func evalPromQL(ctx context.Context, vmURL, orgID, query string) ([]float64, error) {
	form := url.Values{"query": {query}, "extra_label": {"org_id=" + orgID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vmURL+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("metrics store: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("query: %s", out.Error)
	}
	var vals []float64
	parse := func(raw []any) {
		if len(raw) == 2 {
			if s, ok := raw[1].(string); ok {
				if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) {
					vals = append(vals, f)
				}
			}
		}
	}
	switch out.Data.ResultType {
	case "vector":
		var series []struct {
			Value []any `json:"value"`
		}
		_ = json.Unmarshal(out.Data.Result, &series)
		for _, s := range series {
			parse(s.Value)
		}
	case "scalar":
		var v []any
		_ = json.Unmarshal(out.Data.Result, &v)
		parse(v)
	default:
		return nil, fmt.Errorf("the query must return a number or an instant vector, not a %s", out.Data.ResultType)
	}
	return vals, nil
}

func (a *Agent) alarmLoop(ctx context.Context) {
	if !a.isPrimary() || a.cfg.VictoriaMetricsURL == "" {
		return
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		a.evaluateAlarms(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.alarmWake:
		}
	}
}

func (a *Agent) pokeAlarms() {
	select {
	case a.alarmWake <- struct{}{}:
	default:
	}
}

func fmtValue(v float64) string { return strconv.FormatFloat(v, 'g', 4, 64) }

func (a *Agent) evaluateAlarms(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT r.id, r.org_id, r.name, r.query, r.op, r.state, r.threshold, r.for_minutes, r.pending_since
		FROM alarm_rules r JOIN orgs o ON o.id = r.org_id WHERE r.enabled AND NOT o.suspended`)
	if err != nil {
		a.log.Warn("alarm rules", "err", err)
		return
	}
	var rules []alarmRule
	for rows.Next() {
		var r alarmRule
		if rows.Scan(&r.ID, &r.OrgID, &r.Name, &r.Query, &r.Op, &r.State, &r.Threshold, &r.ForMinutes, &r.PendingSince) == nil {
			rules = append(rules, r)
		}
	}
	rows.Close()
	now := time.Now()
	for _, r := range rules {
		vals, err := evalPromQL(ctx, a.cfg.VictoriaMetricsURL, r.OrgID, r.Query)
		if err != nil {
			_, _ = a.pool.Exec(ctx, `UPDATE alarm_rules SET last_error = $2, last_eval = now() WHERE id = $1`, r.ID, truncate(err.Error(), 300))
			continue
		}
		if len(vals) == 0 {
			_, _ = a.pool.Exec(ctx, `UPDATE alarm_rules SET state = 'nodata', pending_since = NULL, last_value = NULL, last_eval = now(), last_error = '' WHERE id = $1`, r.ID)
			if r.State == "firing" {
				a.resolveAlarm(ctx, r)
			}
			continue
		}
		v := worst(vals, r.Op)
		state, pending := "ok", (*time.Time)(nil)
		if breaches(v, r.Op, r.Threshold) {
			since := now
			if r.PendingSince != nil {
				since = *r.PendingSince
			}
			pending = &since
			state = "pending"
			if r.State == "firing" || now.Sub(since) >= time.Duration(r.ForMinutes)*time.Minute {
				state = "firing"
			}
		}
		_, _ = a.pool.Exec(ctx, `UPDATE alarm_rules SET state = $2, pending_since = $3, last_value = $4, last_eval = now(), last_error = '' WHERE id = $1`,
			r.ID, state, pending, v)
		switch {
		case state == "firing" && r.State != "firing":
			a.raiseAlarm(ctx, r, v)
		case state != "firing" && r.State == "firing":
			a.resolveAlarm(ctx, r)
		}
	}
}

func (a *Agent) raiseAlarm(ctx context.Context, r alarmRule, v float64) {
	var id string
	summary := fmt.Sprintf("%s: %s %s %s", r.Name, fmtValue(v), r.Op, fmtValue(r.Threshold))
	if r.ForMinutes > 0 {
		summary += fmt.Sprintf(" for %d min", r.ForMinutes)
	}
	err := a.pool.QueryRow(ctx, `
		INSERT INTO alert_incidents (org_id, alarm_id, kind, status, summary) VALUES ($1, $2, 'metric_alarm', 'firing', $3)
		ON CONFLICT (alarm_id) WHERE status = 'firing' DO NOTHING RETURNING id`, r.OrgID, r.ID, truncate(summary, 500)).Scan(&id)
	if err != nil {
		return
	}
	a.log.Info("metric alarm firing", "alarm", r.ID)
	a.queueNotifications(ctx, r.OrgID, id, "metric_alarm", "firing")
}

func (a *Agent) resolveAlarm(ctx context.Context, r alarmRule) {
	var id string
	err := a.pool.QueryRow(ctx, `
		UPDATE alert_incidents SET status = 'resolved', resolved_at = now()
		WHERE alarm_id = $1 AND status = 'firing' RETURNING id`, r.ID).Scan(&id)
	if err != nil {
		return
	}
	a.log.Info("metric alarm resolved", "alarm", r.ID)
	a.queueNotifications(ctx, r.OrgID, id, "metric_alarm", "resolved")
}
