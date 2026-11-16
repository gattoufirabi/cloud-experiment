package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/netguard"
)

var (
	alertKinds = []string{"service_down", "deploy_failed", "backup_failed", "critical_vulns", "cpu_high", "memory_high"}
	topicRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type channelView struct {
	ID, Name, Kind, URL, Topic string
	HasSecret                  bool
	Events                     []string
	Enabled                    bool
	LastStatus                 string
	LastDelivery               *time.Time
}

type incidentView struct {
	Kind, Status, Summary string
	Service               string
	StartedAt             time.Time
	ResolvedAt            *time.Time
}

func (i incidentView) Title() string {
	switch i.Kind {
	case "service_down":
		return "Service down"
	case "cpu_high":
		return "High CPU"
	case "memory_high":
		return "High memory"
	case "deploy_failed":
		return "Deployment failed"
	case "backup_failed":
		return "Backup failed"
	case "critical_vulns":
		return "Critical vulnerabilities"
	case "disk_quota":
		return "Disk limit"
	}
	return "Test notification"
}

func loadChannels(r *http.Request, tx pgx.Tx) ([]channelView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT id, name, kind, url, topic, secret_enc IS NOT NULL, events, enabled, last_status, last_delivery_at
		FROM alert_channels ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[channelView])
}

// loadIncidents returns open incidents first, then the most recent ones.
func loadIncidents(r *http.Request, tx pgx.Tx, serviceID string, limit int) ([]incidentView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT i.kind, i.status, i.summary, coalesce(s.slug, ''), i.started_at, i.resolved_at
		FROM alert_incidents i LEFT JOIN services s ON s.id = i.service_id
		WHERE i.kind <> 'test' AND ($1 = '' OR i.service_id::text = $1)
		ORDER BY (i.status = 'firing') DESC, i.started_at DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[incidentView])
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		back(w, r, o.Path(), "", "Invalid form.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	kind := r.PostFormValue("kind")
	rawURL := strings.TrimSpace(r.PostFormValue("url"))
	topic := strings.TrimSpace(r.PostFormValue("topic"))
	secret := r.PostFormValue("secret")
	var events []string
	for _, k := range alertKinds {
		for _, v := range r.PostForm["events"] {
			if v == k {
				events = append(events, k)
			}
		}
	}
	fail := func(msg string) { back(w, r, o.Path()+"#alerts", "", msg) }
	switch {
	case name == "" || len(name) > 60:
		fail("Give the channel a name of up to 60 characters.")
		return
	case kind != "webhook" && kind != "ntfy":
		fail("Choose webhook or ntfy.")
		return
	case kind == "ntfy" && !topicRe.MatchString(topic):
		fail("ntfy needs a topic: letters, digits, - and _ (up to 64).")
		return
	case len(events) == 0:
		fail("Select at least one event.")
		return
	case len(secret) > 500:
		fail("The secret is too long.")
		return
	}
	if kind == "webhook" {
		topic = ""
	}
	if err := netguard.ValidateURL(rawURL, s.cfg.CookieSecure, s.cfg.AlertAllowPrivate); err != nil {
		fail(err.Error())
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO alert_channels (org_id, name, kind, url, topic, events) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			o.ID, name, kind, rawURL, topic, events).Scan(&id); err != nil {
			return err
		}
		if secret != "" {
			enc, err := s.box.Seal([]byte(secret), "alert:"+id)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `UPDATE alert_channels SET secret_enc = $2 WHERE id = $1`, id, enc); err != nil {
				return err
			}
		}
		s.audit(r, tx, o.ID, "alert_channel.created", "channel:"+name, map[string]any{"kind": kind, "events": events})
		return nil
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"#alerts", "Channel added. Use \"Send test\" to check it.", "")
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `DELETE FROM alert_channels WHERE id = $1 RETURNING name`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "alert_channel.deleted", "channel:"+name, nil)
		return nil
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"#alerts", "Channel removed.", "")
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	if !s.limiter.allow("alert-test:"+o.ID, 10, 10*time.Minute) {
		back(w, r, o.Path()+"#alerts", "", "Too many test notifications. Wait a few minutes.")
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `SELECT name FROM alert_channels WHERE id = $1`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		var incident string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO alert_incidents (org_id, kind, status, summary, resolved_at)
			VALUES ($1, 'test', 'event', $2, now()) RETURNING id`,
			o.ID, "Test notification from "+o.Name+": this channel works.").Scan(&incident); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO alert_notifications (org_id, incident_id, channel_id, transition) VALUES ($1, $2, $3, 'event')`,
			o.ID, incident, r.PathValue("id")); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'alert')`)
		return err
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"#alerts", "Test notification queued. The delivery status appears next to the channel.", "")
}

func (s *Server) alertThresholds(w http.ResponseWriter, r *http.Request) {
	parse := func(k string) any {
		v, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(k)))
		if err != nil || v < 1 || v > 100 {
			return nil // empty or out of range = no alert
		}
		return v
	}
	cpu, mem := parse("cpu_pct"), parse("mem_pct")
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if _, err := tx.Exec(r.Context(), `UPDATE services SET alert_cpu_pct = $2, alert_mem_pct = $3 WHERE id = $1`, sv.ID, cpu, mem); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "alert.thresholds", "service:"+sv.Slug, map[string]any{"cpu_pct": cpu, "mem_pct": mem})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#alerts", "Alert thresholds saved.", "")
}
