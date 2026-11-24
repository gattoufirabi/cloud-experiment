package agent

import (
	"context"
	"errors"
	"time"

	"platform/internal/billing"
)

// Usage metering, once a minute:
//   - allocated CPU and memory of every live service and running database are added to
//     usage_hourly. meter_ticks makes each minute count once, even across restarts;
//   - network out of each service container is added as the difference since the last
//     reading (a restarted container starts again from zero).
//
// When billing is enabled, the previous month's invoices are issued on the 1st.

func (a *Agent) meterLoop(ctx context.Context) {
	lastTx := map[string]uint64{} // container ID -> tx bytes at the last reading
	abuse := newAbuseState()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	invoicedMonth := ""
	for {
		a.meterEgress(ctx, lastTx, abuse)
		a.watchCPU(ctx, abuse)
		if a.isPrimary() { // platform-wide bookkeeping runs once, on the primary
			a.meterMinute(ctx)
			a.expireTrials(ctx)
			a.remindTrials(ctx)
		}
		if a.cfg.BillingEnabled && a.isPrimary() {
			prev, _ := billing.MonthBounds(time.Now().UTC().AddDate(0, 0, -1))
			if key := prev.Format("2006-01"); key != invoicedMonth && time.Now().UTC().Day() == 1 {
				n, err := billing.Generate(ctx, a.pool, a.cfg, prev)
				switch {
				case errors.Is(err, billing.ErrMonthNotOver):
				case err != nil:
					a.log.Error("generate invoices", "month", key, "err", err)
				default:
					invoicedMonth = key
					if n > 0 {
						a.log.Info("invoices issued", "month", key, "count", n)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) meterMinute(ctx context.Context) {
	tag, err := a.pool.Exec(ctx, `INSERT INTO meter_ticks VALUES (date_trunc('minute', now())) ON CONFLICT DO NOTHING`)
	if err != nil {
		a.log.Warn("meter tick", "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return // this minute is already metered
	}
	if _, err := a.pool.Exec(ctx, `
		INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, cpu_milli_minutes, mem_mb_minutes)
		SELECT d.org_id, d.service_id, 'service', p.slug || '/' || coalesce(ps.slug || ' preview ' || s.preview_name, s.slug),
		       date_trunc('hour', now()),
		       (d.spec->>'cpu_millis')::int * `+meteredInstancesSQL+`,
		       (d.spec->>'memory_mb')::int * `+meteredInstancesSQL+`
		FROM deployments d
		JOIN services s ON s.id = d.service_id
		LEFT JOIN services ps ON ps.id = s.preview_of
		JOIN projects p ON p.id = s.project_id
		JOIN orgs o ON o.id = d.org_id
		WHERE d.status = 'live' AND NOT o.suspended
		  -- applications on the customer's own servers use the customer's hardware
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = p.node_id AND n.role = 'customer')
		UNION ALL
		SELECT db.org_id, db.id, 'database', p.slug || '/' || db.slug, date_trunc('hour', now()), db.cpu_millis, db.memory_mb
		FROM databases db
		JOIN projects p ON p.id = db.project_id
		JOIN orgs o ON o.id = db.org_id
		WHERE db.status = 'running' AND NOT o.suspended
		UNION ALL
		SELECT r.org_id, r.job_id, 'service', p.slug || '/' || s.slug || ' job ' || j.slug, date_trunc('hour', now()),
		       sum(r.cpu_millis), sum(r.memory_mb)
		FROM job_runs r JOIN jobs j ON j.id = r.job_id JOIN services s ON s.id = j.service_id JOIN projects p ON p.id = s.project_id
		WHERE r.status = 'running'
		GROUP BY r.org_id, r.job_id, p.slug, s.slug, j.slug
		ON CONFLICT (resource_id, hour) DO UPDATE SET
		    cpu_milli_minutes = usage_hourly.cpu_milli_minutes + EXCLUDED.cpu_milli_minutes,
		    mem_mb_minutes    = usage_hourly.mem_mb_minutes + EXCLUDED.mem_mb_minutes,
		    name              = EXCLUDED.name`); err != nil {
		a.log.Warn("meter usage", "err", err)
	}
	a.expirePreviews(ctx)
	// Keep a week of ticks; older minutes can no longer be metered anyway.
	_, _ = a.pool.Exec(ctx, `DELETE FROM meter_ticks WHERE minute < now() - interval '7 days'`)
}

// meteredInstancesSQL is a live deployment's instance count: the revision's own, or on
// Kubernetes the pods its autoscaler runs now (kept in services.instances by reconcileKube).
const meteredInstancesSQL = `greatest(1, CASE WHEN coalesce(d.instances->0->>'id', '') LIKE 'k8s:%' THEN s.instances
		ELSE coalesce((d.spec->>'instances')::int, 1) END)`

func (a *Agent) meterEgress(ctx context.Context, last map[string]uint64, abuse *abuseState) {
	seen := map[string]bool{}
	cpuLimit := map[string]int{} // container ID -> allocated millicores
	if rows, err := a.pool.Query(ctx, `
		SELECT coalesce(e->>'id', d.container_id), (d.spec->>'cpu_millis')::int
		FROM deployments d LEFT JOIN LATERAL jsonb_array_elements(d.instances) e ON true
		WHERE d.status = 'live' AND coalesce(e->>'id', d.container_id) IS NOT NULL`); err == nil {
		for rows.Next() {
			var id string
			var m int
			if rows.Scan(&id, &m) == nil {
				cpuLimit[id] = m
			}
		}
		rows.Close()
	}
	node, _ := a.nodeID(ctx)
	for _, t := range a.currentTargets() {
		seen[t.ContainerID] = true
		st, err := a.dk.ContainerStats(ctx, t.ContainerID)
		if err != nil {
			continue
		}
		abuse.sample(t.ContainerID, st.CPUStats.CPUUsage.TotalUsage)
		if lim := cpuLimit[t.ContainerID]; lim > 0 {
			pct := 0.0
			if s, ok := abuse.cpu[t.ContainerID]; ok {
				pct = s.util * 100 / (float64(lim) / 1000)
			}
			mem := st.MemoryStats.Usage
			if inactive := st.MemoryStats.Stats["inactive_file"]; inactive < mem {
				mem -= inactive
			}
			_, _ = a.pool.Exec(ctx, `
				INSERT INTO service_stats (service_id, org_id, node_id, cpu_pct, mem_mb, updated_at)
				VALUES ($1, $2, $3, $4, $5, now())
				ON CONFLICT (service_id) DO UPDATE SET node_id = EXCLUDED.node_id, cpu_pct = EXCLUDED.cpu_pct,
				    mem_mb = EXCLUDED.mem_mb, updated_at = now()`,
				t.ServiceID, t.OrgID, node, min(pct, 99999), int(mem>>20))
		}
		var tx uint64
		for _, n := range st.Networks {
			tx += n.TxBytes
		}
		prev, known := last[t.ContainerID]
		last[t.ContainerID] = tx
		if !known {
			continue // first reading is the baseline
		}
		delta := tx - prev
		if tx < prev {
			delta = tx // counters reset
		}
		if delta == 0 {
			continue
		}
		if _, err := a.pool.Exec(ctx, `
			INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, egress_bytes)
			SELECT s.org_id, s.id, 'service', p.slug || '/' || s.slug, date_trunc('hour', now()), $2
			FROM services s JOIN projects p ON p.id = s.project_id WHERE s.id = $1
			ON CONFLICT (resource_id, hour) DO UPDATE SET egress_bytes = usage_hourly.egress_bytes + EXCLUDED.egress_bytes`,
			t.ServiceID, int64(delta)); err != nil {
			a.log.Warn("meter egress", "err", err)
		}
	}
	for id := range last {
		if !seen[id] {
			delete(last, id)
		}
	}
}

// expirePreviews deletes previews past their expiry; reconcile then removes their
// containers and the proxy route.
func (a *Agent) expirePreviews(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		DELETE FROM services s USING services ps, projects p
		WHERE s.expires_at < now() AND ps.id = s.preview_of AND p.id = s.project_id
		RETURNING s.org_id, p.slug || '/' || ps.slug, s.preview_name`)
	if err != nil {
		a.log.Warn("expire previews", "err", err)
		return
	}
	type gone struct{ org, target, name string }
	var list []gone
	for rows.Next() {
		var g gone
		if rows.Scan(&g.org, &g.target, &g.name) == nil {
			list = append(list, g)
		}
	}
	rows.Close()
	for _, g := range list {
		_ = auditAgent(ctx, a, g.org, "preview.expired", "service:"+g.target, map[string]string{"preview": g.name})
		a.log.Info("preview expired", "service", g.target, "preview", g.name)
	}
	if len(list) > 0 {
		a.poke()
	}
}

// generateInvoices issues a past month's invoices when an operator asks from the admin
// panel (organizations already invoiced for that month are skipped).
func (a *Agent) generateInvoices(ctx context.Context, month string) {
	if !a.isPrimary() {
		return
	}
	m, err := time.Parse("2006-01", month)
	if err != nil {
		return
	}
	n, err := billing.Generate(ctx, a.pool, a.cfg, m)
	if err != nil {
		a.log.Warn("generate invoices", "month", month, "err", err)
		return
	}
	a.log.Info("invoices issued on request", "month", month, "count", n)
}
