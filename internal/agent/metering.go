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
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	invoicedMonth := ""
	for {
		a.meterMinute(ctx)
		a.meterEgress(ctx, lastTx)
		if a.cfg.BillingEnabled {
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
		SELECT d.org_id, d.service_id, 'service', p.slug || '/' || s.slug, date_trunc('hour', now()),
		       (d.spec->>'cpu_millis')::int, (d.spec->>'memory_mb')::int
		FROM deployments d
		JOIN services s ON s.id = d.service_id
		JOIN projects p ON p.id = s.project_id
		JOIN orgs o ON o.id = d.org_id
		WHERE d.status = 'live' AND NOT o.suspended
		UNION ALL
		SELECT db.org_id, db.id, 'database', p.slug || '/' || db.slug, date_trunc('hour', now()), db.cpu_millis, db.memory_mb
		FROM databases db
		JOIN projects p ON p.id = db.project_id
		JOIN orgs o ON o.id = db.org_id
		WHERE db.status = 'running' AND NOT o.suspended
		ON CONFLICT (resource_id, hour) DO UPDATE SET
		    cpu_milli_minutes = usage_hourly.cpu_milli_minutes + EXCLUDED.cpu_milli_minutes,
		    mem_mb_minutes    = usage_hourly.mem_mb_minutes + EXCLUDED.mem_mb_minutes,
		    name              = EXCLUDED.name`); err != nil {
		a.log.Warn("meter usage", "err", err)
	}
	// Keep a week of ticks; older minutes can no longer be metered anyway.
	_, _ = a.pool.Exec(ctx, `DELETE FROM meter_ticks WHERE minute < now() - interval '7 days'`)
}

func (a *Agent) meterEgress(ctx context.Context, last map[string]uint64) {
	seen := map[string]bool{}
	for _, t := range a.currentTargets() {
		seen[t.ContainerID] = true
		st, err := a.dk.ContainerStats(ctx, t.ContainerID)
		if err != nil {
			continue
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
