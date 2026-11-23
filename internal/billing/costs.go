package billing

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cost reports: a month's usage priced per project, before tax and without the plan's
// monthly fee (which belongs to the organization). Services, databases, jobs and
// functions belong to their project (their metered name starts with "project/"); object
// storage, queues and email are organization-wide.

// Rows is satisfied by a pool and a transaction.
type Rows interface {
	Querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ProjectCost is one project's priced usage for a month.
type ProjectCost struct {
	Project string // project slug; "" for organization-wide resources
	Usage   Usage
	Cents   int64
}

// UsageCents prices usage without the plan fee and without tax.
func UsageCents(p Prices, u Usage) int64 {
	var sum int64
	for _, l := range Lines("", p, u, 0) {
		sum += l.Amount
	}
	return sum
}

// CostByProject prices an organization's usage in [start, end) per project, most
// expensive first (the organization-wide row last).
func CostByProject(ctx context.Context, q Rows, orgID string, p Prices, start, end time.Time) ([]ProjectCost, error) {
	byProject := map[string]*Usage{}
	get := func(k string) *Usage {
		if byProject[k] == nil {
			byProject[k] = &Usage{}
		}
		return byProject[k]
	}
	rows, err := q.Query(ctx, `
		SELECT CASE WHEN kind IN ('service', 'database', 'function') THEN split_part(name, '/', 1) ELSE '' END,
		       coalesce(sum(cpu_milli_minutes), 0)::bigint, coalesce(sum(mem_mb_minutes), 0)::bigint, coalesce(sum(egress_bytes), 0)::bigint,
		       coalesce(sum(storage_bytes), 0)::bigint, coalesce(sum(fn_requests), 0)::bigint, coalesce(sum(fn_mb_ms), 0)::bigint,
		       coalesce(sum(queue_requests), 0)::bigint, coalesce(sum(emails), 0)::bigint
		FROM usage_hourly WHERE org_id = $1 AND hour >= $2 AND hour < $3 GROUP BY 1`, orgID, start, end)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var u Usage
		if err := rows.Scan(&k, &u.CPUMilliMinutes, &u.MemMBMinutes, &u.EgressBytes, &u.StorageByteHours, &u.FnRequests, &u.FnMBms, &u.QueueRequests, &u.Emails); err != nil {
			rows.Close()
			return nil, err
		}
		b := get(k)
		b.CPUMilliMinutes += u.CPUMilliMinutes
		b.MemMBMinutes += u.MemMBMinutes
		b.EgressBytes += u.EgressBytes
		b.StorageByteHours += u.StorageByteHours
		b.FnRequests += u.FnRequests
		b.FnMBms += u.FnMBms
		b.QueueRequests += u.QueueRequests
		b.Emails += u.Emails
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// build minutes belong to the service's project
	rows, err = q.Query(ctx, `
		SELECT coalesce(p.slug, ''), coalesce(sum(extract(epoch FROM b.finished_at - b.started_at)), 0)::bigint
		FROM builds b LEFT JOIN services s ON s.id = b.service_id LEFT JOIN projects p ON p.id = s.project_id
		WHERE b.org_id = $1 AND b.runner = 'platform' AND b.started_at IS NOT NULL AND b.finished_at >= $2 AND b.finished_at < $3
		GROUP BY 1`, orgID, start, end)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var sec int64
		if err := rows.Scan(&k, &sec); err != nil {
			rows.Close()
			return nil, err
		}
		if sec > 0 {
			get(k).BuildSeconds += sec
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ProjectCost, 0, len(byProject))
	for k, u := range byProject {
		out = append(out, ProjectCost{Project: k, Usage: *u, Cents: UsageCents(p, *u)})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Project == "") != (out[j].Project == "") {
			return out[j].Project == ""
		}
		if out[i].Cents != out[j].Cents {
			return out[i].Cents > out[j].Cents
		}
		return out[i].Project < out[j].Project
	})
	return out, nil
}

// Forecast extends a month-to-date amount to the whole month at the same pace.
func Forecast(spent int64, start, end, now time.Time) int64 {
	if !now.After(start) {
		return spent
	}
	if !now.Before(end) {
		return spent
	}
	elapsed := now.Sub(start).Seconds()
	if elapsed < 3600 { // too early in the month to extrapolate
		elapsed = 3600
	}
	return int64(float64(spent) * end.Sub(start).Seconds() / elapsed)
}
