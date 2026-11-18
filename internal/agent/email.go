package agent

import (
	"context"
	"net"
	"strings"
	"time"
)

// Outgoing email: the primary verifies sending domains through DNS (an ownership TXT
// record and the DKIM key) and delivers queued messages, signed with the domain's
// DKIM key, to each recipient's mail servers (or through the platform's SMTP relay).
// Each recipient has its own result; temporary failures are retried with back-off
// (2, 4, 8, 16 minutes). See emailsend.go.

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
		if a.cfg.EmailMode() != "off" {
			for a.sendEmails(ctx) == emailBatch && ctx.Err() == nil {
			}
			a.sendEmailEvents(ctx)
		}
		a.sendPlatformMail(ctx) // the platform's own notices, when there is no relay
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			_, _ = a.pool.Exec(ctx, `DELETE FROM email_messages WHERE created_at < now() - interval '30 days'`)
			_, _ = a.pool.Exec(ctx, `DELETE FROM email_events WHERE created_at < now() - interval '7 days'`)
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
