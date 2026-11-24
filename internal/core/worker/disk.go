package worker

import (
	"context"
	"fmt"
	"time"

	"platform/internal/lib/model"
)

// Disk usage per organization, every 15 minutes: the sizes of its persistent volumes
// (service folders pv-*, database data pvdb-* and dumps pbdb-*), from Docker's disk
// usage report. Owners are alerted when crossing 90% and 100% of the plan's limit;
// the API refuses new deployments and databases above 110%.

func (a *Agent) PokeDisk() {
	select {
	case a.DiskWake <- struct{}{}:
	default:
	}
}

func (a *Agent) DiskLoop(ctx context.Context) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-a.DiskWake:
		}
		a.measureDisk(ctx)
		timer.Reset(15 * time.Minute)
	}
}

func (a *Agent) measureDisk(ctx context.Context) {
	sizes, err := a.Docker.VolumeSizes(ctx)
	if err != nil {
		a.Log.Warn("disk usage", "err", err)
		return
	}
	// Volume name -> organization, from the database (volumes carry no labels).
	owner := map[string]string{}
	rows, err := a.Pool.Query(ctx, `SELECT 's', id::text, org_id::text FROM services UNION ALL SELECT 'd', id::text, org_id::text FROM databases`)
	if err != nil {
		return
	}
	for rows.Next() {
		var kind, id, org string
		if rows.Scan(&kind, &id, &org) != nil {
			continue
		}
		if kind == "s" {
			owner["pv-"+model.Compact(id)] = org
		} else {
			owner[DBDataVolume(id)] = org
			owner[dbDumpVolume(id)] = org
		}
	}
	rows.Close()
	used := map[string]int64{}
	for name, size := range sizes {
		if org, ok := owner[name]; ok && size > 0 {
			used[org] += size
		}
	}
	// Each node records what it holds; the primary adds the nodes up per organization.
	_, _ = a.Pool.Exec(ctx, `DELETE FROM node_disk WHERE node_id = $1`, a.SelfID)
	for org, bytes := range used {
		_, _ = a.Pool.Exec(ctx, `INSERT INTO node_disk (node_id, org_id, used_mb, measured_at) VALUES ($1, $2, $3, now())
			ON CONFLICT (node_id, org_id) DO UPDATE SET used_mb = EXCLUDED.used_mb, measured_at = now()`,
			a.SelfID, org, int((bytes+(1<<20-1))>>20))
	}
	if !a.IsPrimary() {
		return
	}
	used = map[string]int64{}
	if rows, err := a.Pool.Query(ctx, `SELECT org_id::text, sum(used_mb)::bigint FROM node_disk WHERE measured_at > now() - interval '2 hours' GROUP BY org_id`); err == nil {
		for rows.Next() {
			var org string
			var mb int64
			if rows.Scan(&org, &mb) == nil {
				used[org] = mb << 20
			}
		}
		rows.Close()
	}

	orgRows, err := a.Pool.Query(ctx, `
		SELECT o.id, o.slug, p.max_disk_mb, o.disk_alert_level FROM orgs o JOIN plans p ON p.id = o.plan_id`)
	if err != nil {
		return
	}
	type orgDisk struct {
		id, slug     string
		limit, level int
	}
	var orgs []orgDisk
	for orgRows.Next() {
		var o orgDisk
		if orgRows.Scan(&o.id, &o.slug, &o.limit, &o.level) == nil {
			orgs = append(orgs, o)
		}
	}
	orgRows.Close()
	for _, o := range orgs {
		mb := int((used[o.id] + (1<<20 - 1)) >> 20)
		level := 0
		switch pct := mb * 100 / max(o.limit, 1); {
		case pct >= 100:
			level = 100
		case pct >= 90:
			level = 90
		}
		if _, err := a.Pool.Exec(ctx, `UPDATE orgs SET disk_used_mb = $2, disk_measured_at = now(), disk_alert_level = $3 WHERE id = $1`,
			o.id, mb, level); err != nil {
			continue
		}
		if level > o.level { // alert once per crossing, upwards
			msg := fmt.Sprintf("Persistent storage uses %d MB of the %d MB in your plan (%d%%).", mb, o.limit, mb*100/max(o.limit, 1))
			if level == 100 {
				msg += " Above 110%, new deployments and databases are refused: delete data or ask for a larger plan."
			}
			a.AlertEvent(ctx, o.id, "", "disk_quota", msg)
			_ = AuditAgent(ctx, a, o.id, "disk.limit_alert", "org:"+o.slug, map[string]int{"used_mb": mb, "limit_mb": o.limit, "level": level})
		}
	}
}
