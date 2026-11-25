package agent

import (
	"context"
	"fmt"
	"time"

	"platform/internal/core/worker"
	"platform/internal/lib/mail"
)

// remindTrials emails the owners of trial organizations once, 3 days before the trial
// ends (only when an SMTP server is configured; otherwise it retries later).
func (a *Worker) remindTrials(ctx context.Context) {
	if !mail.Configured(a.Cfg) {
		return
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT o.id, o.slug, o.name, o.trial_ends_at FROM orgs o
		WHERE o.plan_id = 'trial' AND NOT o.suspended AND o.trial_reminded_at IS NULL
		  AND o.trial_ends_at > now() AND o.trial_ends_at < now() + interval '3 days'`)
	if err != nil {
		return
	}
	type trial struct {
		id, slug, name string
		ends           time.Time
	}
	var list []trial
	for rows.Next() {
		var t trial
		if rows.Scan(&t.id, &t.slug, &t.name, &t.ends) == nil {
			list = append(list, t)
		}
	}
	rows.Close()
	for _, t := range list {
		owners, err := a.Pool.Query(ctx, `SELECT u.email FROM memberships m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND m.role = 'owner'`, t.id)
		if err != nil {
			continue
		}
		var emails []string
		for owners.Next() {
			var e string
			if owners.Scan(&e) == nil {
				emails = append(emails, e)
			}
		}
		owners.Close()
		days := int(time.Until(t.ends).Hours()/24) + 1
		sent := false
		for _, to := range emails {
			body := fmt.Sprintf("The free trial of %s ends in %d day(s), on %s.\n\n"+
				"After that your applications stop (your data is kept). To keep them running, reply to this message "+
				"or contact the platform operator to choose a plan.\n\n%s/o/%s\n",
				t.name, days, t.ends.UTC().Format("2 January 2006"), a.Cfg.BaseURL, t.slug)
			if err := a.SendMail(ctx, to, "Your trial ends soon", body); err != nil {
				a.Log.Warn("trial reminder", "org", t.slug, "err", err)
				continue
			}
			sent = true
		}
		if sent {
			_, _ = a.Pool.Exec(ctx, `UPDATE orgs SET trial_reminded_at = now() WHERE id = $1`, t.id)
			_ = worker.AuditAgent(ctx, a.Agent, t.id, "trial.reminded", "org:"+t.slug, map[string]int{"days_left": days})
		}
	}
}
