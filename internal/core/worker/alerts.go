package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

func (a *Agent) PokeAlerts() {
	select {
	case a.AlertWake <- struct{}{}:
	default:
	}
}

// ---------- incidents ----------

func (a *Agent) QueueNotifications(ctx context.Context, orgID, incidentID, kind, transition string) {
	_, err := a.Pool.Exec(ctx, `
		INSERT INTO alert_notifications (org_id, incident_id, channel_id, transition)
		SELECT $1, $2, c.id, $3 FROM alert_channels c
		WHERE c.org_id = $1 AND c.enabled AND ($4 = ANY (c.events) OR $4 = 'disk_quota')`, orgID, incidentID, transition, kind)
	if err != nil {
		a.Log.Error("queue notifications", "err", err)
		return
	}
	a.PokeAlerts()
}

// alertEvent records a one-off event (failed deploy, failed backup, critical CVEs).
func (a *Agent) AlertEvent(ctx context.Context, orgID, serviceID, kind, summary string) {
	if !a.Cfg.AlertsEnabled {
		return
	}
	var svc any
	if serviceID != "" {
		svc = serviceID
	}
	var id string
	if err := a.Pool.QueryRow(ctx, `
		INSERT INTO alert_incidents (org_id, service_id, kind, status, summary, resolved_at)
		VALUES ($1, $2, $3, 'event', $4, now()) RETURNING id`, orgID, svc, kind, Truncate(summary, 500)).Scan(&id); err != nil {
		a.Log.Error("record alert event", "err", err)
		return
	}
	a.QueueNotifications(ctx, orgID, id, kind, "event")
}

// vmVectorBy runs an instant query and keys the values by one label of each series.
func (a *Agent) VmVectorBy(ctx context.Context, query, label string) map[string]float64 {
	out := map[string]float64{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.Cfg.VictoriaMetricsURL+"/api/v1/query?"+url.Values{"query": {query}}.Encode(), nil)
	if err != nil {
		return out
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return out
	}
	for _, r := range body.Data.Result {
		if s, ok := r.Value[1].(string); ok {
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				out[r.Metric[label]] = v
			}
		}
	}
	return out
}
