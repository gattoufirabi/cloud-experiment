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

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/mail"
	"platform/internal/lib/netguard"
	"platform/internal/lib/secure"
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
	if web.IsNoRows(err) {
		return nil, nil
	}
	return &v, err
}

func (s *API) suppressTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, address string) error {
	_, addr, err := mail.ParseAddress(strings.TrimSpace(address))
	if err != nil {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "address: a valid email address"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO email_suppressions (org_id, address, reason) VALUES ($1, $2, 'manual') ON CONFLICT DO NOTHING`, o.ID, addr); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "email.suppressed", "email-address:"+addr, nil)
	return nil
}

func (s *API) unsuppressTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, address string) error {
	addr := strings.ToLower(strings.TrimSpace(address))
	tag, err := tx.Exec(r.Context(), `DELETE FROM email_suppressions WHERE address = $1`, addr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return web.APIErr{Status: http.StatusNotFound, Msg: addr + " is not on the suppression list"}
	}
	s.Audit(r, tx, o.ID, "email.unsuppressed", "email-address:"+addr, nil)
	return nil
}

// setWebhookTx saves the delivery webhook; a new secret is returned when one was created.
func (s *API) setWebhookTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, url string, events []string) (string, error) {
	url = strings.TrimSpace(url)
	if err := netguard.ValidateURL(url, s.Cfg.CookieSecure, s.Cfg.AlertAllowPrivate); err != nil {
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "url: " + err.Error()}
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
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "events: choose at least one of delivered, bounced, failed, suppressed"}
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
		enc, err := s.Box.Seal([]byte(secret), "emailhook:"+o.ID)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO email_webhooks (org_id, url, secret_enc, events) VALUES ($1, $2, $3, $4)`, o.ID, url, enc, evs); err != nil {
			return "", err
		}
	}
	s.Audit(r, tx, o.ID, "email.webhook_saved", "email-webhook", map[string]any{"url": url, "events": evs})
	return secret, nil
}

func (s *API) deleteWebhookTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo) error {
	tag, err := tx.Exec(r.Context(), `DELETE FROM email_webhooks`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return web.APIErr{Status: http.StatusNotFound, Msg: "no delivery webhook"}
	}
	s.Audit(r, tx, o.ID, "email.webhook_deleted", "email-webhook", nil)
	return nil
}

// ---------- console ----------

func (s *API) emailFormResult(w http.ResponseWriter, r *http.Request, o *web.OrgInfo, anchor string, err error, ok string) {
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/email#"+anchor, "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, o.Path()+"/email#"+anchor, ok, "")
	}
}

func (s *API) emailSuppressionAction(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	addr := r.PostFormValue("address")
	remove := r.PostFormValue("remove") == "1"
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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

func (s *API) emailWebhookAction(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		web.Back(w, r, o.Path()+"/email#webhook", "", "Invalid form.")
		return
	}
	var secret string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
		web.Back(w, r, o.Path()+"/email#webhook", "Delivery webhook removed.", "")
	case secret != "":
		// shown once, on this response only (never in a URL)
		s.renderEmailPage(w, r, o, web.Data{"WebhookSecret": secret, "Msg": "Delivery webhook saved. Copy its signing secret now: it is not shown again."})
	default:
		web.Back(w, r, o.Path()+"/email#webhook", "Delivery webhook saved.", "")
	}
}

// ---------- REST ----------

func (s *API) apiSuppressions(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []suppression
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		out, err = loadSuppressions(r.Context(), tx, 1000)
		return err
	})
	if web.APITx(w, s.Server, err) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiAddSuppression(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"address": "someone@example.org"}`)
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.suppressTx(r, tx, o, in.Address) })
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *API) apiDeleteSuppression(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.unsuppressTx(r, tx, o, r.PathValue("address")) })
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *API) apiEmailWebhook(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var v *webhookView
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		v, err = loadEmailWebhook(r.Context(), tx)
		return err
	})
	if !web.APITx(w, s.Server, err) {
		return
	}
	if v == nil {
		web.APIError(w, http.StatusNotFound, "no delivery webhook")
		return
	}
	web.WriteJSON(w, v)
}

func (s *API) apiSetEmailWebhook(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"url": "https://example.com/hooks/email", "events": ["delivered", "bounced", "failed", "suppressed"]}`)
		return
	}
	if len(in.Events) == 0 {
		in.Events = emailEvents
	}
	var secret string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		secret, err = s.setWebhookTx(r, tx, o, in.URL, in.Events)
		return err
	})
	if web.APITx(w, s.Server, err) {
		out := map[string]string{"status": "saved"}
		if secret != "" {
			out["secret"] = secret
		}
		web.WriteJSON(w, out)
	}
}

func (s *API) apiDeleteEmailWebhook(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.deleteWebhookTx(r, tx, o) })
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}
