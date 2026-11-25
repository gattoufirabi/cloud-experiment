package api

import (
	"net/http"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	platmail "platform/internal/lib/mail"
	"platform/internal/lib/netguard"
)

func (s *API) createChannel(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		web.Back(w, r, o.Path()+"/monitoring#channels", "", "Invalid form.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	kind := r.PostFormValue("kind")
	rawURL := strings.TrimSpace(r.PostFormValue("url"))
	topic := strings.TrimSpace(r.PostFormValue("topic"))
	secret := r.PostFormValue("secret")
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	var events []string
	for _, k := range web.AlertKinds {
		for _, v := range r.PostForm["events"] {
			if v == k {
				events = append(events, k)
			}
		}
	}
	fail := func(msg string) { web.Back(w, r, o.Path()+"/monitoring#channels", "", msg) }
	switch {
	case name == "" || len(name) > 60:
		fail("Give the channel a name of up to 60 characters.")
		return
	case kind != "webhook" && kind != "ntfy" && kind != "email":
		fail("Choose email, ntfy or webhook.")
		return
	case kind == "email" && !platmail.Configured(s.Cfg):
		fail("This platform does not send email yet (no SMTP server configured). Use ntfy or a webhook.")
		return
	case kind == "email" && !validEmail(email):
		fail("Enter a valid email address for the email channel.")
		return
	case kind == "ntfy" && !web.TopicRe.MatchString(topic):
		fail("ntfy needs a topic: letters, digits, - and _ (up to 64).")
		return
	case len(events) == 0:
		fail("Select at least one event.")
		return
	case len(secret) > 500:
		fail("The secret is too long.")
		return
	}
	if kind != "ntfy" {
		topic = ""
	}
	if kind == "email" {
		rawURL, secret = "mailto:"+email, ""
	} else if err := netguard.ValidateURL(rawURL, s.Cfg.CookieSecure, s.Cfg.AlertAllowPrivate); err != nil {
		fail(err.Error())
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO alert_channels (org_id, name, kind, url, topic, events) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			o.ID, name, kind, rawURL, topic, events).Scan(&id); err != nil {
			return err
		}
		if secret != "" {
			enc, err := s.Box.Seal([]byte(secret), "alert:"+id)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `UPDATE alert_channels SET secret_enc = $2 WHERE id = $1`, id, enc); err != nil {
				return err
			}
		}
		s.Audit(r, tx, o.ID, "alert_channel.created", "channel:"+name, map[string]any{"kind": kind, "events": events})
		return nil
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/monitoring#channels", "Channel added. Use \"Send test\" to check it.", "")
}

func (s *API) deleteChannel(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `DELETE FROM alert_channels WHERE id = $1 RETURNING name`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "alert_channel.deleted", "channel:"+name, nil)
		return nil
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/monitoring#channels", "Channel removed.", "")
}

func (s *API) testChannel(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	if !s.Limiter.Allow("alert-test:"+o.ID, 10, 10*time.Minute) {
		web.Back(w, r, o.Path()+"/monitoring#channels", "", "Too many test notifications. Wait a few minutes.")
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/monitoring#channels", "Test notification queued. The delivery status appears next to the channel.", "")
}

func (s *API) alertThresholds(w http.ResponseWriter, r *http.Request) {
	parse := func(k string) any {
		v, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(k)))
		if err != nil || v < 1 || v > 100 {
			return nil // empty or out of range = no alert
		}
		return v
	}
	cpu, mem := parse("cpu_pct"), parse("mem_pct")
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if _, err := tx.Exec(r.Context(), `UPDATE services SET alert_cpu_pct = $2, alert_mem_pct = $3 WHERE id = $1`, sv.ID, cpu, mem); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "alert.thresholds", "service:"+sv.Slug, map[string]any{"cpu_pct": cpu, "mem_pct": mem})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#alerts", "Alert thresholds saved.", "")
}

func validEmail(addr string) bool {
	a, err := netmail.ParseAddress(addr)
	return err == nil && a.Address == addr && len(addr) <= 200 && strings.Contains(addr[strings.LastIndex(addr, "@")+1:], ".")
}
