package agent

import (
	"context"
	"errors"
	"time"

	"platform/internal/mail"
)

// The platform's own emails (alerts, trial notices; invitations and password resets
// queued by the API). With a relay they are sent at once; otherwise they wait in
// platform_mail and the primary delivers them straight to the recipient's mail server,
// signed with the DKIM key of SMTP_FROM's domain when that domain is a verified sending
// domain on the platform.

// sendMail sends a notice now (relay) or queues it (direct delivery).
func (a *Agent) sendMail(ctx context.Context, to, subject, body string) error {
	if !mail.Configured(a.cfg) {
		return mail.ErrNotConfigured
	}
	if mail.Relay(a.cfg) {
		return mail.Send(a.cfg, to, subject, body)
	}
	_, err := a.pool.Exec(ctx, `INSERT INTO platform_mail (to_addr, subject, body) VALUES ($1, $2, $3)`, to, subject, body)
	a.pokeEmail()
	return err
}

// sendPlatformMail delivers queued notices (called by the email loop on the primary).
func (a *Agent) sendPlatformMail(ctx context.Context) {
	if mail.Relay(a.cfg) || !mail.Configured(a.cfg) {
		return
	}
	_, _ = a.pool.Exec(ctx, `UPDATE platform_mail SET status = 'queued' WHERE status = 'sending' AND next_attempt_at < now()`)
	rows, err := a.pool.Query(ctx, `
		UPDATE platform_mail SET status = 'sending', attempts = attempts + 1, next_attempt_at = now() + interval '10 minutes'
		WHERE id IN (SELECT id FROM platform_mail WHERE status = 'queued' AND next_attempt_at <= now()
		             ORDER BY next_attempt_at LIMIT 20 FOR UPDATE SKIP LOCKED)
		RETURNING id, to_addr, subject, body, attempts`)
	if err != nil {
		return
	}
	type pm struct {
		ID, To, Subject, Body string
		Attempts              int
	}
	var list []pm
	for rows.Next() {
		var m pm
		if rows.Scan(&m.ID, &m.To, &m.Subject, &m.Body, &m.Attempts) == nil {
			list = append(list, m)
		}
	}
	rows.Close()
	for _, m := range list {
		res, err := a.deliverPlatformMail(ctx, m.To, m.Subject, m.Body)
		switch {
		case err == nil && res.Status == mail.Delivered:
			_, _ = a.pool.Exec(ctx, `UPDATE platform_mail SET status = 'sent', sent_at = now(), body = '', error = '' WHERE id = $1`, m.ID)
		case err == nil && res.Status == mail.Deferred && m.Attempts < emailMaxAttempts:
			_, _ = a.pool.Exec(ctx, `UPDATE platform_mail SET status = 'queued', error = $2, next_attempt_at = now() + make_interval(mins => $3) WHERE id = $1`,
				m.ID, truncate(res.Detail, 500), 1<<m.Attempts)
		default:
			msg := res.Detail
			if err != nil {
				msg = err.Error()
			}
			_, _ = a.pool.Exec(ctx, `UPDATE platform_mail SET status = 'failed', body = '', error = $2 WHERE id = $1`, m.ID, truncate(msg, 500))
			a.log.Warn("platform email not delivered", "id", m.ID, "err", msg)
		}
	}
	if time.Now().Minute() == 0 {
		_, _ = a.pool.Exec(ctx, `DELETE FROM platform_mail WHERE created_at < now() - interval '30 days'`)
	}
}

func (a *Agent) deliverPlatformMail(ctx context.Context, to, subject, body string) (mail.Result, error) {
	msg, envFrom, domain, err := mail.Plain(a.cfg, to, subject, body)
	if err != nil {
		return mail.Result{}, err
	}
	_, rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return mail.Result{}, errors.New("invalid recipient")
	}
	// DKIM: the sender's domain, when it is a verified sending domain on the platform.
	var id, selector string
	var keyEnc []byte
	if a.pool.QueryRow(ctx, `SELECT id, dkim_selector, dkim_key_enc FROM email_domains WHERE domain = $1 AND status = 'verified' ORDER BY verified_at LIMIT 1`, domain).
		Scan(&id, &selector, &keyEnc) == nil {
		if key, err := a.box.Open(keyEnc, "emaildkim:"+id); err == nil {
			if sig, err := mail.DKIMSign(msg, domain, selector, key, time.Now()); err == nil {
				msg = sig + msg
			}
		}
	}
	d := mail.Direct{Resolver: a.resolver(), HeloName: a.cfg.MailHost(), Port: a.cfg.EmailDirectPort, AllowPrivate: a.cfg.EmailDirectAllowPrivate}
	res := mail.SendDirect(ctx, d, envFrom, []string{rcpt}, msg)
	if len(res) == 0 {
		return mail.Result{}, errors.New("no result")
	}
	return res[0], nil
}
