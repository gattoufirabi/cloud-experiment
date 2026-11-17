package agent

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"platform/internal/mail"
)

// Outgoing email: the primary verifies sending domains through DNS (an ownership TXT
// record and the DKIM key) and delivers queued messages, signed with the domain's
// DKIM key, through the platform's SMTP relay. Retries back off: 2, 4, 8, 16 minutes.

const (
	emailMaxAttempts = 5
	emailBatch       = 20
)

func (a *Agent) pokeEmail() {
	select {
	case a.emailWake <- struct{}{}:
	default:
	}
}

func (a *Agent) emailLoop(ctx context.Context) {
	if !a.isPrimary() {
		return
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var lastPrune time.Time
	for {
		a.verifyEmailDomains(ctx)
		if a.cfg.SMTPHost != "" {
			for a.sendEmails(ctx) == emailBatch && ctx.Err() == nil {
			}
		}
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			_, _ = a.pool.Exec(ctx, `DELETE FROM email_messages WHERE created_at < now() - interval '30 days'`)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.emailWake:
		}
	}
}

// resolver uses EMAIL_DNS_RESOLVER when set (tests), the system resolver otherwise.
func (a *Agent) resolver() *net.Resolver {
	if a.cfg.EmailDNSResolver == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		return d.DialContext(ctx, network, a.cfg.EmailDNSResolver)
	}}
}

func hasTXT(ctx context.Context, r *net.Resolver, name, want string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	txts, err := r.LookupTXT(ctx, name)
	if err != nil {
		return false
	}
	norm := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "\"", "") }
	for _, t := range txts {
		if norm(t) == norm(want) {
			return true
		}
	}
	return false
}

// verifyEmailDomains checks pending domains every minute (at once when asked through
// a notification: checked_at is cleared) and verified ones daily.
func (a *Agent) verifyEmailDomains(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT id, org_id, domain, verify_token, dkim_selector, dkim_txt, status FROM email_domains
		WHERE (status <> 'verified' AND (checked_at IS NULL OR checked_at < now() - interval '1 minute'))
		   OR (status = 'verified' AND checked_at < now() - interval '1 day')
		ORDER BY checked_at NULLS FIRST LIMIT 20`)
	if err != nil {
		a.log.Warn("email domains", "err", err)
		return
	}
	type dom struct{ ID, OrgID, Domain, Token, Selector, TXT, Status string }
	var doms []dom
	for rows.Next() {
		var d dom
		if err := rows.Scan(&d.ID, &d.OrgID, &d.Domain, &d.Token, &d.Selector, &d.TXT, &d.Status); err == nil {
			doms = append(doms, d)
		}
	}
	rows.Close()
	r := a.resolver()
	for _, d := range doms {
		owner := hasTXT(ctx, r, "_platform-verify."+d.Domain, "platform-verify="+d.Token)
		dkim := hasTXT(ctx, r, d.Selector+"._domainkey."+d.Domain, d.TXT)
		status, msg := "verified", ""
		switch {
		case !owner && !dkim:
			msg = "Neither DNS record is visible yet."
		case !owner:
			msg = "The ownership TXT record is not visible yet."
		case !dkim:
			msg = "The DKIM TXT record is not visible yet."
		}
		if msg != "" {
			status = "pending"
			if d.Status == "verified" {
				status, msg = "failed", "The DNS records were removed; sending from this domain is paused. "+msg
			}
		}
		_, _ = a.pool.Exec(ctx, `
			UPDATE email_domains SET status = $2, message = $3, checked_at = now(),
			       verified_at = CASE WHEN $2 = 'verified' THEN coalesce(verified_at, now()) ELSE verified_at END
			WHERE id = $1`, d.ID, status, msg)
		if status != d.Status {
			_ = auditAgent(ctx, a, d.OrgID, "email.domain_"+status, "email-domain:"+d.Domain, nil)
		}
	}
}

type outgoing struct {
	ID, OrgID, From, ReplyTo, Subject, Text, HTML string
	DomainID                                      *string
	To                                            []string
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
		RETURNING id, org_id, domain_id::text, from_addr, to_addrs, reply_to, subject, body_text, body_html, attempts`, emailBatch)
	if err != nil {
		a.log.Warn("email claim", "err", err)
		return 0
	}
	var batch []outgoing
	for rows.Next() {
		var m outgoing
		if err := rows.Scan(&m.ID, &m.OrgID, &m.DomainID, &m.From, &m.To, &m.ReplyTo, &m.Subject, &m.Text, &m.HTML, &m.Attempts); err == nil {
			batch = append(batch, m)
		}
	}
	rows.Close()
	for _, m := range batch {
		err := a.deliverEmail(ctx, m)
		switch {
		case err == nil:
			_, _ = a.pool.Exec(ctx, `UPDATE email_messages SET status = 'sent', sent_at = now(), error = '', body_text = '', body_html = '' WHERE id = $1`, m.ID)
			if m.DomainID != nil {
				hour := time.Now().UTC().Truncate(time.Hour)
				_, _ = a.pool.Exec(ctx, `
					INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, emails)
					SELECT $1, $2, 'email', d.domain, $3, $4 FROM email_domains d WHERE d.id = $2
					ON CONFLICT (resource_id, hour) DO UPDATE SET emails = usage_hourly.emails + EXCLUDED.emails`,
					m.OrgID, *m.DomainID, hour, len(m.To))
			}
		case m.Attempts >= emailMaxAttempts || permanent(err):
			_, _ = a.pool.Exec(ctx, `UPDATE email_messages SET status = 'failed', error = $2, body_text = '', body_html = '' WHERE id = $1`, m.ID, truncate(err.Error(), 500))
		default:
			_, _ = a.pool.Exec(ctx, `UPDATE email_messages SET status = 'queued', error = $2, next_attempt_at = now() + make_interval(mins => $3) WHERE id = $1`,
				m.ID, truncate(err.Error(), 500), 1<<m.Attempts)
		}
	}
	return len(batch)
}

type permanentErr struct{ error }

func permanent(err error) bool { _, ok := err.(permanentErr); return ok }

func (a *Agent) deliverEmail(ctx context.Context, m outgoing) error {
	if m.DomainID == nil {
		return permanentErr{fmt.Errorf("the sending domain was deleted")}
	}
	var domain, selector, status string
	var keyEnc []byte
	if err := a.pool.QueryRow(ctx, `SELECT domain, dkim_selector, dkim_key_enc, status FROM email_domains WHERE id = $1`, *m.DomainID).
		Scan(&domain, &selector, &keyEnc, &status); err != nil {
		return permanentErr{fmt.Errorf("the sending domain was deleted")}
	}
	if status != "verified" {
		return permanentErr{fmt.Errorf("the domain %s is not verified", domain)}
	}
	key, err := a.box.Open(keyEnc, "emaildkim:"+*m.DomainID)
	if err != nil {
		return err
	}
	_, envFrom, err := mail.ParseAddress(m.From)
	if err != nil {
		return permanentErr{err}
	}
	now := time.Now()
	msg := mail.Compose(mail.Message{From: m.From, ReplyTo: m.ReplyTo, To: m.To, Subject: m.Subject, Text: m.Text, HTML: m.HTML}, domain, now)
	sig, err := mail.DKIMSign(msg, domain, selector, key, now)
	if err != nil {
		return err
	}
	rcpts := make([]string, 0, len(m.To))
	for _, t := range m.To {
		_, addr, err := mail.ParseAddress(t)
		if err != nil {
			return permanentErr{err}
		}
		rcpts = append(rcpts, addr)
	}
	return mail.SendRaw(a.cfg, envFrom, rcpts, sig+msg)
}
