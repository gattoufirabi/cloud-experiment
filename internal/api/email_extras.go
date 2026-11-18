package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/mail"
	"platform/internal/netguard"
	"platform/internal/secure"
)

// Email extras: the suppression list (addresses not sent to: hard bounces, manual
// entries), the delivery webhook (signed JSON for each recipient's result) and each
// message's per-recipient results.

var emailEvents = []string{"delivered", "bounced", "failed", "suppressed"}

type suppression struct {
	Address string    `json:"address"`
	Reason  string    `json:"reason"`
	Detail  string    `json:"detail"`
	Created time.Time `json:"created_at"`
}

func loadSuppressions(ctx context.Context, tx pgx.Tx, limit int) ([]suppression, error) {
	rows, err := tx.Query(ctx, `SELECT address, reason, detail, created_at FROM email_suppressions ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[suppression])
	if out == nil {
		out = []suppression{}
	}
	return out, err
}

type recipientView struct {
	Address string    `json:"address"`
	Status  string    `json:"status"`
	Detail  string    `json:"detail"`
	Updated time.Time `json:"updated_at"`
}

func loadRecipients(ctx context.Context, tx pgx.Tx, messageID string) ([]recipientView, error) {
	rows, err := tx.Query(ctx, `SELECT address, status, detail, updated_at FROM email_recipients WHERE message_id = $1 ORDER BY address`, messageID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[recipientView])
	if out == nil {
		out = []recipientView{}
	}
	return out, err
}

type webhookView struct {
	URL        string   `json:"url"`
	Events     []string `json:"events"`
	Enabled    bool     `json:"enabled"`
	LastStatus string   `json:"last_status"`
}

func loadEmailWebhook(ctx context.Context, tx pgx.Tx) (*webhookView, error) {
	var v webhookView
	err := tx.QueryRow(ctx, `SELECT url, events, enabled, last_status FROM email_webhooks`).Scan(&v.URL, &v.Events, &v.Enabled, &v.LastStatus)
	if isNoRows(err) {
		return nil, nil
	}
	return &v, err
}

func (s *Server) suppressTx(r *http.Request, tx pgx.Tx, o *orgInfo, address string) error {
	_, addr, err := mail.ParseAddress(strings.TrimSpace(address))
	if err != nil {
		return apiErr{http.StatusUnprocessableEntity, "address: a valid email address"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO email_suppressions (org_id, address, reason) VALUES ($1, $2, 'manual') ON CONFLICT DO NOTHING`, o.ID, addr); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "email.suppressed", "email-address:"+addr, nil)
	return nil
}

func (s *Server) unsuppressTx(r *http.Request, tx pgx.Tx, o *orgInfo, address string) error {
	addr := strings.ToLower(strings.TrimSpace(address))
	tag, err := tx.Exec(r.Context(), `DELETE FROM email_suppressions WHERE address = $1`, addr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apiErr{http.StatusNotFound, addr + " is not on the suppression list"}
	}
	s.audit(r, tx, o.ID, "email.unsuppressed", "email-address:"+addr, nil)
	return nil
}

// setWebhookTx saves the delivery webhook; a new secret is returned when one was created.
func (s *Server) setWebhookTx(r *http.Request, tx pgx.Tx, o *orgInfo, url string, events []string) (string, error) {
	url = strings.TrimSpace(url)
	if err := netguard.ValidateURL(url, s.cfg.CookieSecure, s.cfg.AlertAllowPrivate); err != nil {
		return "", apiErr{http.StatusUnprocessableEntity, "url: " + err.Error()}
	}
	var evs []string
	for _, e := range emailEvents {
		for _, x := range events {
			if x == e {
				evs = append(evs, e)
				break
			}
		}
	}
	if len(evs) == 0 {
		return "", apiErr{http.StatusUnprocessableEntity, "events: choose at least one of delivered, bounced, failed, suppressed"}
	}
	var exists bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM email_webhooks)`).Scan(&exists); err != nil {
		return "", err
	}
	secret := ""
	if exists {
		if _, err := tx.Exec(r.Context(), `UPDATE email_webhooks SET url = $1, events = $2, enabled = true`, url, evs); err != nil {
			return "", err
		}
	} else {
		secret = "whsec_" + secure.NewToken()
		enc, err := s.box.Seal([]byte(secret), "emailhook:"+o.ID)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO email_webhooks (org_id, url, secret_enc, events) VALUES ($1, $2, $3, $4)`, o.ID, url, enc, evs); err != nil {
			return "", err
		}
	}
	s.audit(r, tx, o.ID, "email.webhook_saved", "email-webhook", map[string]any{"url": url, "events": evs})
	return secret, nil
}

func (s *Server) deleteWebhookTx(r *http.Request, tx pgx.Tx, o *orgInfo) error {
	tag, err := tx.Exec(r.Context(), `DELETE FROM email_webhooks`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apiErr{http.StatusNotFound, "no delivery webhook"}
	}
	s.audit(r, tx, o.ID, "email.webhook_deleted", "email-webhook", nil)
	return nil
}

// ---------- console ----------

func (s *Server) emailFormResult(w http.ResponseWriter, r *http.Request, o *orgInfo, anchor string, err error, ok string) {
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/email#"+anchor, "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/email#"+anchor, ok, "")
	}
}

func (s *Server) emailSuppressionAction(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	addr := r.PostFormValue("address")
	remove := r.PostFormValue("remove") == "1"
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if remove {
			return s.unsuppressTx(r, tx, o, addr)
		}
		return s.suppressTx(r, tx, o, addr)
	})
	msg := addr + " added to the suppression list: nothing is sent to it any more."
	if remove {
		msg = addr + " removed from the suppression list."
	}
	s.emailFormResult(w, r, o, "suppressions", err, msg)
}

func (s *Server) emailWebhookAction(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		back(w, r, o.Path()+"/email#webhook", "", "Invalid form.")
		return
	}
	var secret string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if r.PostFormValue("remove") == "1" {
			return s.deleteWebhookTx(r, tx, o)
		}
		var err error
		secret, err = s.setWebhookTx(r, tx, o, r.PostFormValue("url"), r.PostForm["events"])
		return err
	})
	switch {
	case err != nil:
		s.emailFormResult(w, r, o, "webhook", err, "")
	case r.PostFormValue("remove") == "1":
		back(w, r, o.Path()+"/email#webhook", "Delivery webhook removed.", "")
	case secret != "":
		// shown once, on this response only (never in a URL)
		s.renderEmailPage(w, r, o, data{"WebhookSecret": secret, "Msg": "Delivery webhook saved. Copy its signing secret now: it is not shown again."})
	default:
		back(w, r, o.Path()+"/email#webhook", "Delivery webhook saved.", "")
	}
}

// ---------- REST ----------

func (s *Server) apiSuppressions(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []suppression
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		out, err = loadSuppressions(r.Context(), tx, 1000)
		return err
	})
	if apiTx(w, s, err) {
		writeJSON(w, out)
	}
}

func (s *Server) apiAddSuppression(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var in struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"address": "someone@example.org"}`)
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error { return s.suppressTx(r, tx, o, in.Address) })
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) apiDeleteSuppression(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error { return s.unsuppressTx(r, tx, o, r.PathValue("address")) })
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) apiEmailWebhook(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var v *webhookView
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		v, err = loadEmailWebhook(r.Context(), tx)
		return err
	})
	if !apiTx(w, s, err) {
		return
	}
	if v == nil {
		apiError(w, http.StatusNotFound, "no delivery webhook")
		return
	}
	writeJSON(w, v)
}

func (s *Server) apiSetEmailWebhook(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var in struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"url": "https://example.com/hooks/email", "events": ["delivered", "bounced", "failed", "suppressed"]}`)
		return
	}
	if len(in.Events) == 0 {
		in.Events = emailEvents
	}
	var secret string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		secret, err = s.setWebhookTx(r, tx, o, in.URL, in.Events)
		return err
	})
	if apiTx(w, s, err) {
		out := map[string]string{"status": "saved"}
		if secret != "" {
			out["secret"] = secret
		}
		writeJSON(w, out)
	}
}

func (s *Server) apiDeleteEmailWebhook(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error { return s.deleteWebhookTx(r, tx, o) })
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// sendMail sends one of the platform's own notices: at once through the relay, or queued
// for the agent, which delivers it directly (platform_mail).
func (s *Server) sendMail(ctx context.Context, to, subject, body string) error {
	if !mail.Configured(s.cfg) {
		return mail.ErrNotConfigured
	}
	if mail.Relay(s.cfg) {
		return mail.Send(s.cfg, to, subject, body)
	}
	if _, _, _, err := mail.Plain(s.cfg, to, subject, body); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO platform_mail (to_addr, subject, body) VALUES ($1, $2, $3)`, to, subject, body); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `SELECT pg_notify('deployments', 'email')`)
	return err
}
