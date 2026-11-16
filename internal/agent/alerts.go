package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/model"
	"platform/internal/netguard"
)

// Alerts: the agent probes every live application, compares CPU/memory with the
// thresholds clients set, records events (failed deploys and backups, critical CVEs in
// running images), and delivers notifications to each organization's own channels.
//
// Incidents are deduplicated (one open incident per service and kind) and resolved
// automatically, with a "resolved" notification. Delivery retries with backoff.

const downAfter = 3 // consecutive failed probes before "service down"

func (a *Agent) alertLoop(ctx context.Context) {
	if !a.cfg.AlertsEnabled {
		return
	}
	tick := time.NewTicker(a.cfg.AlertInterval)
	defer tick.Stop()
	prune := time.NewTicker(24 * time.Hour)
	defer prune.Stop()
	go func() { // keep 90 days of uptime history
		for {
			_, _ = a.pool.Exec(ctx, `DELETE FROM service_uptime WHERE hour < now() - interval '90 days'`)
			select {
			case <-ctx.Done():
				return
			case <-prune.C:
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.checkHealth(ctx)
			a.checkResources(ctx)
		case <-a.alertWake:
		}
		a.deliverAlerts(ctx)
	}
}

func (a *Agent) pokeAlerts() {
	select {
	case a.alertWake <- struct{}{}:
	default:
	}
}

// ---------- incidents ----------

func (a *Agent) queueNotifications(ctx context.Context, orgID, incidentID, kind, transition string) {
	_, err := a.pool.Exec(ctx, `
		INSERT INTO alert_notifications (org_id, incident_id, channel_id, transition)
		SELECT $1, $2, c.id, $3 FROM alert_channels c
		WHERE c.org_id = $1 AND c.enabled AND ($4 = ANY (c.events) OR $4 = 'disk_quota')`, orgID, incidentID, transition, kind)
	if err != nil {
		a.log.Error("queue notifications", "err", err)
		return
	}
	a.pokeAlerts()
}

// raise opens an incident unless one is already open for this service and kind.
func (a *Agent) raise(ctx context.Context, orgID, serviceID, kind, summary string) {
	var id string
	err := a.pool.QueryRow(ctx, `
		INSERT INTO alert_incidents (org_id, service_id, kind, status, summary) VALUES ($1, $2, $3, 'firing', $4)
		ON CONFLICT (service_id, kind) WHERE status = 'firing' DO NOTHING
		RETURNING id`, orgID, serviceID, kind, truncate(summary, 500)).Scan(&id)
	if err != nil {
		return // already open (no row returned) or a transient error
	}
	a.log.Info("incident opened", "kind", kind, "service", serviceID)
	a.queueNotifications(ctx, orgID, id, kind, "firing")
}

func (a *Agent) resolve(ctx context.Context, serviceID, kind string) {
	var id, orgID string
	err := a.pool.QueryRow(ctx, `
		UPDATE alert_incidents SET status = 'resolved', resolved_at = now()
		WHERE service_id = $1 AND kind = $2 AND status = 'firing'
		RETURNING id, org_id`, serviceID, kind).Scan(&id, &orgID)
	if err != nil {
		return
	}
	a.log.Info("incident resolved", "kind", kind, "service", serviceID)
	a.queueNotifications(ctx, orgID, id, kind, "resolved")
}

// alertEvent records a one-off event (failed deploy, failed backup, critical CVEs).
func (a *Agent) alertEvent(ctx context.Context, orgID, serviceID, kind, summary string) {
	if !a.cfg.AlertsEnabled {
		return
	}
	var svc any
	if serviceID != "" {
		svc = serviceID
	}
	var id string
	if err := a.pool.QueryRow(ctx, `
		INSERT INTO alert_incidents (org_id, service_id, kind, status, summary, resolved_at)
		VALUES ($1, $2, $3, 'event', $4, now()) RETURNING id`, orgID, svc, kind, truncate(summary, 500)).Scan(&id); err != nil {
		a.log.Error("record alert event", "err", err)
		return
	}
	a.queueNotifications(ctx, orgID, id, kind, "event")
}

// ---------- health probes ----------

type probeTarget struct {
	OrgID, ServiceID, ContainerID, ProjectID, Service, Project string
	Port                                                       int
	HealthPath                                                 string
}

func (a *Agent) checkHealth(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.org_id, d.service_id, coalesce(d.container_id, ''), s.project_id, s.slug, p.slug,
		       (d.spec->>'port')::int, d.spec->>'health_path'
		FROM deployments d JOIN services s ON s.id = d.service_id
		JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = d.org_id
		WHERE d.status = 'live' AND NOT o.suspended`)
	if err != nil {
		a.log.Error("load probe targets", "err", err)
		return
	}
	targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[probeTarget])
	if err != nil {
		return
	}
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// Probe in parallel (bounded), so one hanging app cannot delay the others.
	reasons := make([]string, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t probeTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			reasons[i] = a.probe(ctx, client, t)
		}(i, t)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, t := range targets {
		seen[t.ServiceID] = true
		reason := reasons[i]
		// Hourly uptime totals feed the public status pages (30-day bars and percentages).
		okInc := 0
		if reason == "" {
			okInc = 1
		}
		if _, err := a.pool.Exec(ctx, `
			INSERT INTO service_uptime (org_id, service_id, hour, ok, total) VALUES ($1, $2, date_trunc('hour', now()), $3, 1)
			ON CONFLICT (service_id, hour) DO UPDATE SET ok = service_uptime.ok + EXCLUDED.ok, total = service_uptime.total + 1`,
			t.OrgID, t.ServiceID, okInc); err != nil {
			a.log.Warn("record uptime", "err", err)
		}
		a.mu.Lock()
		if reason == "" {
			a.probeFails[t.ServiceID] = 0
		} else {
			a.probeFails[t.ServiceID]++
		}
		fails := a.probeFails[t.ServiceID]
		a.mu.Unlock()
		switch {
		case reason == "":
			a.resolve(ctx, t.ServiceID, "service_down")
		case fails >= downAfter:
			a.raise(ctx, t.OrgID, t.ServiceID, "service_down",
				fmt.Sprintf("%s (%s) is not responding: %s", t.Service, t.Project, reason))
		}
	}
	a.mu.Lock()
	for id := range a.probeFails {
		if !seen[id] {
			delete(a.probeFails, id)
		}
	}
	a.mu.Unlock()
}

// probe returns "" when healthy, otherwise a short reason.
func (a *Agent) probe(ctx context.Context, client *http.Client, t probeTarget) string {
	if t.ContainerID == "" {
		return "no container"
	}
	info, err := a.dk.ContainerInspect(ctx, t.ContainerID)
	if err != nil {
		return "container missing"
	}
	if !info.State.Running {
		if info.State.OOMKilled {
			return "killed: out of memory"
		}
		return fmt.Sprintf("container stopped (exit code %d)", info.State.ExitCode)
	}
	if info.State.Status == "paused" {
		return "container paused"
	}
	ip := info.NetworkSettings.Networks[model.NetworkName(t.ProjectID)].IPAddress
	if ip == "" {
		return "no network address"
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(pctx, http.MethodGet, "http://"+net.JoinHostPort(ip, strconv.Itoa(t.Port))+t.HealthPath, nil)
	resp, err := client.Do(req)
	if err != nil {
		return "health check got no answer within 3 s"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Sprintf("health check returned HTTP %d", resp.StatusCode)
	}
	return ""
}

// ---------- resource thresholds ----------

func (a *Agent) checkResources(ctx context.Context) {
	if a.cfg.VictoriaMetricsURL == "" {
		return
	}
	type svc struct {
		OrgID, ServiceID, Slug string
		CPUMillis              int
		CPUPct, MemPct         *int
	}
	rows, err := a.pool.Query(ctx, `
		SELECT s.org_id, s.id, s.slug, s.cpu_millis, s.alert_cpu_pct, s.alert_mem_pct
		FROM services s JOIN orgs o ON o.id = s.org_id
		WHERE NOT o.suspended AND (s.alert_cpu_pct IS NOT NULL OR s.alert_mem_pct IS NOT NULL)
		  AND EXISTS (SELECT 1 FROM deployments d WHERE d.service_id = s.id AND d.status = 'live')`)
	if err != nil {
		return
	}
	services, err := pgx.CollectRows(rows, pgx.RowToStructByPos[svc])
	if err != nil || len(services) == 0 {
		return
	}
	// "Above the threshold for the whole last 5 minutes": the minimum over the window.
	cpu := a.vmVector(ctx, `min_over_time((sum by (service_id) (rate(platform_container_cpu_seconds_total[1m])))[5m:30s])`)
	mem := a.vmVector(ctx, `min_over_time((sum by (service_id) (platform_container_memory_bytes) / sum by (service_id) (platform_container_memory_limit_bytes))[5m:30s])`)
	for _, s := range services {
		if s.CPUPct != nil {
			if v, ok := cpu[s.ServiceID]; ok {
				pct := v / (float64(s.CPUMillis) / 1000) * 100
				if pct >= float64(*s.CPUPct) {
					a.raise(ctx, s.OrgID, s.ServiceID, "cpu_high",
						fmt.Sprintf("%s has used at least %.0f%% of its CPU limit for 5 minutes (alert at %d%%)", s.Slug, pct, *s.CPUPct))
				} else {
					a.resolve(ctx, s.ServiceID, "cpu_high")
				}
			}
		}
		if s.MemPct != nil {
			if v, ok := mem[s.ServiceID]; ok {
				pct := v * 100
				if pct >= float64(*s.MemPct) {
					a.raise(ctx, s.OrgID, s.ServiceID, "memory_high",
						fmt.Sprintf("%s has used at least %.0f%% of its memory limit for 5 minutes (alert at %d%%)", s.Slug, pct, *s.MemPct))
				} else {
					a.resolve(ctx, s.ServiceID, "memory_high")
				}
			}
		}
	}
}

func (a *Agent) vmVector(ctx context.Context, query string) map[string]float64 {
	out := map[string]float64{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.cfg.VictoriaMetricsURL+"/api/v1/query?"+url.Values{"query": {query}}.Encode(), nil)
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
				out[r.Metric["service_id"]] = v
			}
		}
	}
	return out
}

// ---------- delivery ----------

type delivery struct {
	ID, Transition                     string
	Attempts                           int
	ChannelID, ChannelKind, URL, Topic string
	SecretEnc                          []byte
	IncidentID, Kind, Summary          string
	StartedAt                          time.Time
	ResolvedAt                         *time.Time
	OrgSlug, Project, Service          string
}

var kindTitles = map[string]string{
	"service_down": "Service down", "cpu_high": "High CPU", "memory_high": "High memory",
	"deploy_failed": "Deployment failed", "backup_failed": "Backup failed",
	"critical_vulns": "Critical vulnerabilities", "test": "Test notification", "disk_quota": "Disk limit",
}

func (a *Agent) deliverAlerts(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT n.id, n.transition, n.attempts, c.id, c.kind, c.url, c.topic, c.secret_enc,
		       i.id, i.kind, i.summary, i.started_at, i.resolved_at,
		       o.slug, coalesce(p.slug, ''), coalesce(s.slug, '')
		FROM alert_notifications n
		JOIN alert_channels c ON c.id = n.channel_id
		JOIN alert_incidents i ON i.id = n.incident_id
		JOIN orgs o ON o.id = n.org_id
		LEFT JOIN services s ON s.id = i.service_id
		LEFT JOIN projects p ON p.id = s.project_id
		WHERE n.status = 'pending' AND n.next_attempt_at <= now()
		ORDER BY n.created_at LIMIT 50`)
	if err != nil {
		a.log.Error("load notifications", "err", err)
		return
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[delivery])
	if err != nil {
		a.log.Error("scan notifications", "err", err)
		return
	}
	for _, d := range items {
		err := a.send(ctx, d)
		status := "sent"
		if err == nil {
			_, _ = a.pool.Exec(ctx, `UPDATE alert_notifications SET status = 'sent', attempts = attempts + 1, sent_at = now(), last_error = '' WHERE id = $1`, d.ID)
		} else {
			status = truncate(err.Error(), 200)
			next := time.Duration(30<<d.Attempts) * time.Second // 30 s, 1 min, 2 min, 4 min
			final := "pending"
			if d.Attempts+1 >= 5 {
				final = "failed"
			}
			_, _ = a.pool.Exec(ctx, `UPDATE alert_notifications SET status = $2, attempts = attempts + 1, last_error = $3, next_attempt_at = now() + $4::interval WHERE id = $1`,
				d.ID, final, status, fmt.Sprintf("%d seconds", int(next.Seconds())))
			a.log.Warn("notification failed", "channel", d.ChannelID, "err", err)
		}
		_, _ = a.pool.Exec(ctx, `UPDATE alert_channels SET last_status = $2, last_delivery_at = now() WHERE id = $1`, d.ChannelID, status)
	}
}

func (a *Agent) send(ctx context.Context, d delivery) error {
	var secret string
	if d.SecretEnc != nil {
		plain, err := a.box.Open(d.SecretEnc, "alert:"+d.ChannelID)
		if err != nil {
			return fmt.Errorf("decrypt channel secret: %w", err)
		}
		secret = string(plain)
	}
	title := kindTitles[d.Kind]
	if d.Transition == "resolved" {
		title = "Resolved: " + title
	}
	where := d.OrgSlug
	if d.Service != "" {
		where = d.OrgSlug + "/" + d.Project + "/" + d.Service
	}
	link := a.cfg.BaseURL + "/o/" + d.OrgSlug
	if d.Service != "" {
		link += "/p/" + d.Project + "/s/" + d.Service
	}

	var req *http.Request
	var err error
	switch d.ChannelKind {
	case "ntfy":
		body := d.Summary
		if d.Transition == "resolved" {
			body = "Back to normal. " + d.Summary
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.URL, "/")+"/"+d.Topic, strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Title", title+" · "+where)
		req.Header.Set("Click", link)
		prio := "high"
		if d.Transition == "resolved" || d.Kind == "test" {
			prio = "default"
		}
		req.Header.Set("Priority", prio)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
	default: // webhook
		payload := map[string]any{
			"incident_id": d.IncidentID, "kind": d.Kind, "status": d.Transition, "title": title,
			"summary": d.Summary, "organization": d.OrgSlug, "project": d.Project, "service": d.Service,
			"started_at": d.StartedAt.UTC().Format(time.RFC3339), "console_url": link,
		}
		if d.ResolvedAt != nil {
			payload["resolved_at"] = d.ResolvedAt.UTC().Format(time.RFC3339)
		}
		body, _ := json.Marshal(payload)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "platform-alerts/1")
		if secret != "" {
			// Receivers verify: HMAC-SHA256(secret, raw body) == X-Platform-Signature.
			m := hmac.New(sha256.New, []byte(secret))
			m.Write(body)
			req.Header.Set("X-Platform-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
	}
	resp, err := a.alertHTTP.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("receiver answered HTTP %d", resp.StatusCode)
	}
	return nil
}

func newAlertHTTP(allowPrivate bool) *http.Client { return netguard.Client(allowPrivate) }
