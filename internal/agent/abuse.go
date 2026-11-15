package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"platform/internal/mail"
)

// Abuse controls run by the agent once a minute:
//   - trials past their end date are suspended (apps stopped, changes blocked) and the
//     owner is told by email;
//   - a container that keeps its CPU above ANOMALY_CPU_PERCENT of its limit for
//     ANOMALY_MINUTES in a row looks like crypto-mining: trial organizations are
//     suspended automatically, paid ones only raise one operator notice a day.

type cpuSample struct {
	total    uint64
	at       time.Time
	util     float64 // share of the CPU limit used since the previous sample (0..1+)
	highMins int
}

type abuseState struct {
	cpu      map[string]*cpuSample // container ID
	noticed  map[string]time.Time  // container ID -> last operator notice
	limitsAt time.Time
}

func newAbuseState() *abuseState {
	return &abuseState{cpu: map[string]*cpuSample{}, noticed: map[string]time.Time{}}
}

// sample records a container's cumulative CPU time (nanoseconds).
func (st *abuseState) sample(containerID string, total uint64) {
	now := time.Now()
	s, ok := st.cpu[containerID]
	if !ok {
		st.cpu[containerID] = &cpuSample{total: total, at: now}
		return
	}
	elapsed := now.Sub(s.at)
	if elapsed > 0 && total >= s.total {
		s.util = float64(total-s.total) / float64(elapsed.Nanoseconds()) // in cores; divided by the limit later
	} else {
		s.util = 0
	}
	s.total, s.at = total, now
}

func (a *Agent) watchCPU(ctx context.Context, st *abuseState) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.container_id, (d.spec->>'cpu_millis')::int, o.id, o.slug, o.plan_id = 'trial'
		FROM deployments d JOIN orgs o ON o.id = d.org_id
		WHERE d.status = 'live' AND d.container_id IS NOT NULL AND NOT o.suspended`)
	if err != nil {
		return
	}
	type live struct {
		cpuMillis int
		orgID     string
		orgSlug   string
		trial     bool
	}
	containers := map[string]live{}
	for rows.Next() {
		var id string
		var l live
		if rows.Scan(&id, &l.cpuMillis, &l.orgID, &l.orgSlug, &l.trial) == nil {
			containers[id] = l
		}
	}
	rows.Close()
	threshold := float64(a.cfg.AnomalyCPUPercent) / 100
	suspended := map[string]bool{}
	for id, s := range st.cpu {
		l, ok := containers[id]
		if !ok {
			delete(st.cpu, id) // gone or suspended
			continue
		}
		if l.cpuMillis <= 0 {
			continue
		}
		share := s.util / (float64(l.cpuMillis) / 1000)
		if share >= threshold {
			s.highMins++
		} else {
			s.highMins = 0
		}
		if s.highMins < a.cfg.AnomalyMinutes || suspended[l.orgID] {
			continue
		}
		detail := fmt.Sprintf("container %s used %.0f%% of its CPU limit for %d minutes", id[:12], share*100, s.highMins)
		if l.trial {
			tag, err := a.pool.Exec(ctx, `UPDATE orgs SET suspended = true, suspend_reason = 'cpu_anomaly' WHERE id = $1 AND NOT suspended`, l.orgID)
			if err == nil && tag.RowsAffected() == 1 {
				suspended[l.orgID] = true
				_ = auditAgent(ctx, a, l.orgID, "org.suspended", "org:"+l.orgSlug, map[string]string{"reason": "cpu_anomaly", "detail": detail})
				a.log.Warn("trial suspended for CPU anomaly", "org", l.orgSlug, "detail", detail)
				a.notifyOperator("Trial suspended: "+l.orgSlug, "Suspended automatically (looks like crypto-mining): "+detail+
					". Check it, then restore with: platform admin suspend-org --slug "+l.orgSlug+" --undo")
				a.poke()
			}
			s.highMins = 0
			continue
		}
		if time.Since(st.noticed[id]) > 24*time.Hour {
			st.noticed[id] = time.Now()
			_ = auditAgent(ctx, a, l.orgID, "abuse.cpu_notice", "org:"+l.orgSlug, map[string]string{"detail": detail})
			a.notifyOperator("Sustained CPU: "+l.orgSlug, detail+" (paid plan: not suspended).")
		}
	}
}

func (a *Agent) expireTrials(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		UPDATE orgs SET suspended = true, suspend_reason = 'trial_ended'
		WHERE plan_id = 'trial' AND trial_ends_at < now() AND NOT suspended
		RETURNING id, slug, name`)
	if err != nil {
		a.log.Warn("expire trials", "err", err)
		return
	}
	type ended struct{ id, slug, name string }
	var list []ended
	for rows.Next() {
		var e ended
		if rows.Scan(&e.id, &e.slug, &e.name) == nil {
			list = append(list, e)
		}
	}
	rows.Close()
	for _, e := range list {
		_ = auditAgent(ctx, a, e.id, "org.suspended", "org:"+e.slug, map[string]string{"reason": "trial_ended"})
		a.log.Info("trial ended", "org", e.slug)
		a.notifyOperator("Trial ended: "+e.slug, "The trial of "+e.name+" ended; its applications are stopped. Upgrade with: platform admin set-plan --slug "+e.slug+" --plan starter")
		if mail.Configured(a.cfg) {
			var owners []string
			if r, err := a.pool.Query(ctx, `SELECT u.email FROM memberships m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND m.role = 'owner'`, e.id); err == nil {
				for r.Next() {
					var em string
					if r.Scan(&em) == nil {
						owners = append(owners, em)
					}
				}
				r.Close()
			}
			for _, to := range owners {
				if err := mail.Send(a.cfg, to, "Your trial has ended",
					"The free trial of "+e.name+" has ended. Your applications are stopped and your data is kept.\n\n"+
						"To continue, reply to this message or contact the platform operator to choose a plan.\n\n"+a.cfg.BaseURL+"\n"); err != nil {
					a.log.Warn("trial email", "err", err)
				}
			}
		}
	}
	if len(list) > 0 {
		a.poke()
	}
}

// notifyOperator posts a short notice to OPERATOR_NTFY_URL (optional).
func (a *Agent) notifyOperator(title, msg string) {
	if a.cfg.OperatorNtfyURL == "" {
		return
	}
	req, err := http.NewRequest(http.MethodPost, a.cfg.OperatorNtfyURL, strings.NewReader(msg))
	if err != nil {
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "warning")
	c := &http.Client{Timeout: 10 * time.Second}
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
}
