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
	"net/http"
	"time"

	"platform/internal/mail"
)

// Delivery of queued email, one result per recipient:
//   - suppressed addresses (hard bounces, manual entries) are skipped;
//   - delivered, bounced (refused for good: the address is suppressed from then on),
//     failed (the message was refused, or temporary failures lasted too long) or still
//     queued (retried later);
//   - the message is "sent" when every recipient got it, "partial" when some did,
//     "failed" when none did; its body is deleted once nothing is left to retry.
// Each result can call the organization's delivery webhook (email_events outbox).

type outgoing struct {
	ID, OrgID, From, ReplyTo, Subject, Text, HTML string
	DomainID                                      *string
	To                                            []string
	Raw                                           []byte
	Attempts                                      int
}

// sendEmails delivers one batch; returns how many messages it claimed.
func (a *Agent) sendEmails(ctx context.Context) int {
	// Messages left "sending" by a crash go back to the queue.
	_, _ = a.pool.Exec(ctx, `UPDATE email_messages SET status = 'queued' WHERE status = 'sending' AND next_attempt_at < now()`)
	rows, err := a.pool.Query(ctx, `
		UPDATE email_messages SET status = 'sending', attempts = attempts + 1, next_attempt_at = now() + interval '10 minutes'
		WHERE id IN (SELECT id FROM email_messages WHERE status = 'queued' AND next_attempt_at <= now()
		             ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id, org_id, domain_id::text, from_addr, to_addrs, reply_to, subject, body_text, body_html, raw, attempts`, emailBatch)
	if err != nil {
		a.log.Warn("email claim", "err", err)
		return 0
	}
	var batch []outgoing
	for rows.Next() {
		var m outgoing
		if err := rows.Scan(&m.ID, &m.OrgID, &m.DomainID, &m.From, &m.To, &m.ReplyTo, &m.Subject, &m.Text, &m.HTML, &m.Raw, &m.Attempts); err == nil {
			batch = append(batch, m)
		}
	}
	rows.Close()
	for _, m := range batch {
		a.processEmail(ctx, m)
	}
	return len(batch)
}

func bareAddrs(list []string) []string {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, t := range list {
		if _, addr, err := mail.ParseAddress(t); err == nil && !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

func (a *Agent) processEmail(ctx context.Context, m outgoing) {
	// Recipient rows (messages queued before per-recipient results have none yet).
	for _, addr := range bareAddrs(m.To) {
		_, _ = a.pool.Exec(ctx, `INSERT INTO email_recipients (message_id, org_id, address) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, m.ID, m.OrgID, addr)
	}
	var queued []string
	if rows, err := a.pool.Query(ctx, `SELECT address FROM email_recipients WHERE message_id = $1 AND status = 'queued' ORDER BY address`, m.ID); err == nil {
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				queued = append(queued, s)
			}
		}
		rows.Close()
	}
	// Suppressed addresses are not attempted.
	if rows, err := a.pool.Query(ctx, `SELECT address, reason FROM email_suppressions WHERE org_id = $1 AND address = ANY($2)`, m.OrgID, queued); err == nil {
		skip := map[string]string{}
		for rows.Next() {
			var addr, reason string
			if rows.Scan(&addr, &reason) == nil {
				skip[addr] = reason
			}
		}
		rows.Close()
		var keep []string
		for _, addr := range queued {
			if reason, ok := skip[addr]; ok {
				a.recipientResult(ctx, m, addr, "suppressed", "on the suppression list ("+reason+")")
				continue
			}
			keep = append(keep, addr)
		}
		queued = keep
	}
	if len(queued) > 0 {
		results, fatal := a.deliverEmail(ctx, m, queued)
		if fatal != nil { // the message cannot be sent at all (domain deleted, not verified...)
			for _, addr := range queued {
				a.recipientResult(ctx, m, addr, "failed", fatal.Error())
			}
		}
		for _, r := range results {
			switch r.Status {
			case mail.Delivered:
				a.recipientResult(ctx, m, r.Addr, "delivered", r.Detail)
			case mail.Bounced:
				a.recipientResult(ctx, m, r.Addr, "bounced", r.Detail)
			case mail.Rejected:
				a.recipientResult(ctx, m, r.Addr, "failed", r.Detail)
			default: // deferred
				if m.Attempts >= emailMaxAttempts {
					a.recipientResult(ctx, m, r.Addr, "failed", "gave up after "+fmt.Sprint(m.Attempts)+" attempts: "+r.Detail)
				} else {
					_, _ = a.pool.Exec(ctx, `UPDATE email_recipients SET detail = $3, updated_at = now() WHERE message_id = $1 AND address = $2`, m.ID, r.Addr, truncate(r.Detail, 500))
				}
			}
		}
	}
	a.finishEmail(ctx, m)
}

// recipientResult records one recipient's final result, suppresses a bounced address
// and queues the webhook event.
func (a *Agent) recipientResult(ctx context.Context, m outgoing, addr, status, detail string) {
	detail = truncate(detail, 500)
	_, _ = a.pool.Exec(ctx, `UPDATE email_recipients SET status = $3, detail = $4, updated_at = now() WHERE message_id = $1 AND address = $2`, m.ID, addr, status, detail)
	switch status {
	case "delivered":
		if m.DomainID != nil {
			hour := time.Now().UTC().Truncate(time.Hour)
			_, _ = a.pool.Exec(ctx, `
				INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, emails)
				SELECT $1, $2, 'email', d.domain, $3, 1 FROM email_domains d WHERE d.id = $2
				ON CONFLICT (resource_id, hour) DO UPDATE SET emails = usage_hourly.emails + 1`, m.OrgID, *m.DomainID, hour)
		}
	case "bounced":
		_, _ = a.pool.Exec(ctx, `INSERT INTO email_suppressions (org_id, address, reason, detail) VALUES ($1, $2, 'bounce', $3) ON CONFLICT DO NOTHING`, m.OrgID, addr, detail)
	}
	_, _ = a.pool.Exec(ctx, `
		INSERT INTO email_events (org_id, message_id, recipient, event, detail)
		SELECT $1, $2, $3, $4, $5 WHERE EXISTS (SELECT 1 FROM email_webhooks w WHERE w.org_id = $1 AND w.enabled AND $4 = ANY (w.events))`,
		m.OrgID, m.ID, addr, status, detail)
}

// finishEmail sets the message's status from its recipients.
func (a *Agent) finishEmail(ctx context.Context, m outgoing) {
	var queued, delivered, total int
	var firstErr string
	_ = a.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'queued'), count(*) FILTER (WHERE status = 'delivered'), count(*),
		       coalesce((SELECT detail FROM email_recipients WHERE message_id = $1 AND status NOT IN ('delivered', 'queued') ORDER BY address LIMIT 1), '')
		FROM email_recipients WHERE message_id = $1`, m.ID).Scan(&queued, &delivered, &total, &firstErr)
	if queued > 0 {
		var lastDetail string
		_ = a.pool.QueryRow(ctx, `SELECT detail FROM email_recipients WHERE message_id = $1 AND status = 'queued' LIMIT 1`, m.ID).Scan(&lastDetail)
		_, _ = a.pool.Exec(ctx, `UPDATE email_messages SET status = 'queued', error = $2, next_attempt_at = now() + make_interval(mins => $3) WHERE id = $1`,
			m.ID, truncate(lastDetail, 500), 1<<m.Attempts)
		return
	}
	status := "sent"
	switch {
	case delivered == 0:
		status = "failed"
	case delivered < total:
		status = "partial"
	}
	_, _ = a.pool.Exec(ctx, `
		UPDATE email_messages SET status = $2, error = $3, body_text = '', body_html = '', raw = NULL,
		       sent_at = CASE WHEN $4 > 0 THEN now() ELSE sent_at END WHERE id = $1`, m.ID, status, truncate(firstErr, 500), delivered)
}

// deliverEmail signs the message with the domain's DKIM key and hands it over (to the
// recipients' servers, or the relay). fatal is set when it cannot be sent at all.
func (a *Agent) deliverEmail(ctx context.Context, m outgoing, rcpts []string) (results []mail.Result, fatal error) {
	if m.DomainID == nil {
		return nil, fmt.Errorf("the sending domain was deleted")
	}
	var domain, selector, status string
	var keyEnc []byte
	if err := a.pool.QueryRow(ctx, `SELECT domain, dkim_selector, dkim_key_enc, status FROM email_domains WHERE id = $1`, *m.DomainID).
		Scan(&domain, &selector, &keyEnc, &status); err != nil {
		return nil, fmt.Errorf("the sending domain was deleted")
	}
	if status != "verified" {
		return nil, fmt.Errorf("the domain %s is not verified", domain)
	}
	key, err := a.box.Open(keyEnc, "emaildkim:"+*m.DomainID)
	if err != nil {
		return nil, fmt.Errorf("cannot read the domain's DKIM key")
	}
	_, envFrom, err := mail.ParseAddress(m.From)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	msg := string(m.Raw)
	if msg == "" {
		msg = mail.Compose(mail.Message{From: m.From, ReplyTo: m.ReplyTo, To: m.To, Subject: m.Subject, Text: m.Text, HTML: m.HTML}, domain, now)
	}
	sig, err := mail.DKIMSign(msg, domain, selector, key, now)
	if err != nil {
		return nil, err
	}
	if a.cfg.EmailMode() == "relay" {
		return mail.SendRelay(a.cfg, envFrom, rcpts, sig+msg), nil
	}
	d := mail.Direct{Resolver: a.resolver(), HeloName: a.cfg.MailHost(), Port: a.cfg.EmailDirectPort, AllowPrivate: a.cfg.EmailDirectAllowPrivate}
	return mail.SendDirect(ctx, d, envFrom, rcpts, sig+msg), nil
}

// sendEmailEvents calls the organizations' delivery webhooks (signed like alert
// webhooks: X-Platform-Signature = "sha256=" + HMAC-SHA256(secret, body)).
func (a *Agent) sendEmailEvents(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		UPDATE email_events e SET attempts = attempts + 1, next_attempt_at = now() + interval '5 minutes'
		WHERE e.id IN (SELECT id FROM email_events WHERE status = 'pending' AND next_attempt_at <= now()
		               ORDER BY created_at LIMIT 50 FOR UPDATE SKIP LOCKED)
		RETURNING e.id, e.org_id, e.message_id, e.recipient, e.event, e.detail, e.created_at, e.attempts`)
	if err != nil {
		return
	}
	type ev struct {
		ID, OrgID, MessageID, Recipient, Event, Detail string
		Created                                        time.Time
		Attempts                                       int
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if rows.Scan(&e.ID, &e.OrgID, &e.MessageID, &e.Recipient, &e.Event, &e.Detail, &e.Created, &e.Attempts) == nil {
			evs = append(evs, e)
		}
	}
	rows.Close()
	for _, e := range evs {
		var url string
		var secretEnc []byte
		if err := a.pool.QueryRow(ctx, `SELECT url, secret_enc FROM email_webhooks WHERE org_id = $1 AND enabled`, e.OrgID).Scan(&url, &secretEnc); err != nil {
			_, _ = a.pool.Exec(ctx, `UPDATE email_events SET status = 'failed', last_error = 'no webhook' WHERE id = $1`, e.ID)
			continue
		}
		secret, err := a.box.Open(secretEnc, "emailhook:"+e.OrgID)
		if err != nil {
			continue
		}
		body, _ := json.Marshal(map[string]any{
			"id": e.ID, "type": "email." + e.Event, "message_id": e.MessageID, "recipient": e.Recipient,
			"detail": e.Detail, "created_at": e.Created.UTC().Format(time.RFC3339),
		})
		err = a.postSigned(ctx, url, secret, body, e.Event)
		switch {
		case err == nil:
			_, _ = a.pool.Exec(ctx, `UPDATE email_events SET status = 'sent', last_error = '' WHERE id = $1`, e.ID)
			_, _ = a.pool.Exec(ctx, `UPDATE email_webhooks SET last_status = $2 WHERE org_id = $1`, e.OrgID, "ok "+time.Now().UTC().Format(time.RFC3339))
		case e.Attempts >= 8:
			_, _ = a.pool.Exec(ctx, `UPDATE email_events SET status = 'failed', last_error = $2 WHERE id = $1`, e.ID, truncate(err.Error(), 300))
			_, _ = a.pool.Exec(ctx, `UPDATE email_webhooks SET last_status = $2 WHERE org_id = $1`, e.OrgID, truncate("failed: "+err.Error(), 200))
		default:
			_, _ = a.pool.Exec(ctx, `UPDATE email_events SET last_error = $2, next_attempt_at = now() + make_interval(mins => $3) WHERE id = $1`,
				e.ID, truncate(err.Error(), 300), 1<<min(e.Attempts, 7))
			_, _ = a.pool.Exec(ctx, `UPDATE email_webhooks SET last_status = $2 WHERE org_id = $1`, e.OrgID, truncate("retrying: "+err.Error(), 200))
		}
	}
}

func (a *Agent) postSigned(ctx context.Context, url string, secret, body []byte, event string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "platform-email/1")
	req.Header.Set("X-Platform-Event", "email."+event)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	req.Header.Set("X-Platform-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := a.alertHTTP.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the receiver answered HTTP %d", resp.StatusCode)
	}
	return nil
}
