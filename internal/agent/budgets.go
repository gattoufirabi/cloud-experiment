package agent

import (
	"context"
	"fmt"
	"time"

	"platform/internal/billing"
)

// Budgets: every hour (and when one is created or changed), the month-to-date cost of each
// budget's scope is computed with the same prices as the invoices (before tax) and compared
// with the limit. Crossing a threshold for the first time in the month opens a "budget"
// incident and notifies the alert channels; so does a forecast above the limit.

type budget struct {
	ID, OrgID, Project string // Project "" = whole organization
	Limit              int64
	Thresholds         []int32
	Forecast           bool
	Month              *time.Time
	Level              int
	ForecastSent       bool
}

func (a *Agent) budgetLoop(ctx context.Context) {
	if !a.isPrimary() {
		return
	}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		a.evaluateBudgets(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.budgetWake:
		}
	}
}

func (a *Agent) pokeBudgets() {
	select {
	case a.budgetWake <- struct{}{}:
	default:
	}
}

// budgetLevel is the highest threshold (percent of the limit) that spent has reached.
func budgetLevel(spent, limit int64, thresholds []int32) int {
	level := 0
	for _, t := range thresholds {
		if spent*100 >= limit*int64(t) && int(t) > level {
			level = int(t)
		}
	}
	return level
}

func (a *Agent) evaluateBudgets(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT b.id, b.org_id, coalesce(p.slug, ''), b.limit_cents, b.thresholds, b.forecast, b.month, b.level, b.forecast_sent
		FROM budgets b LEFT JOIN projects p ON p.id = b.project_id JOIN orgs o ON o.id = b.org_id
		ORDER BY b.org_id`)
	if err != nil {
		a.log.Warn("budgets", "err", err)
		return
	}
	var list []budget
	for rows.Next() {
		var b budget
		if rows.Scan(&b.ID, &b.OrgID, &b.Project, &b.Limit, &b.Thresholds, &b.Forecast, &b.Month, &b.Level, &b.ForecastSent) == nil {
			list = append(list, b)
		}
	}
	rows.Close()
	now := time.Now().UTC()
	start, end := billing.MonthBounds(now)
	type orgCost struct {
		byProject                 map[string]int64
		total, feeSoFar, feeMonth int64
	}
	costs := map[string]*orgCost{}
	for _, b := range list {
		oc := costs[b.OrgID]
		if oc == nil {
			var p billing.Prices
			var created time.Time
			if err := a.pool.QueryRow(ctx, `
				SELECT p.price_monthly_cents, p.price_vcpu_hour_cents::float8, p.price_gb_hour_cents::float8, p.price_gb_out_cents::float8,
				       p.price_build_minute_cents::float8, p.price_storage_gb_month_cents::float8,
				       p.price_fn_gb_second_cents::float8, p.price_fn_million_requests_cents::float8, p.price_queue_million_requests_cents::float8,
				       p.price_email_thousand_cents::float8, o.created_at
				FROM orgs o JOIN plans p ON p.id = o.plan_id WHERE o.id = $1`, b.OrgID).
				Scan(&p.Monthly, &p.VCPUHour, &p.GBHour, &p.GBOut, &p.BuildMin, &p.StorageGBMonth, &p.FnGBSecond, &p.FnMRequests, &p.QueueMRequests, &p.EmailThousand, &created); err != nil {
				continue
			}
			pcs, err := billing.CostByProject(ctx, a.pool, b.OrgID, p, start, end)
			if err != nil {
				a.log.Warn("budget costs", "org", b.OrgID, "err", err)
				continue
			}
			oc = &orgCost{byProject: map[string]int64{},
				feeSoFar: int64(float64(p.Monthly) * billing.Fraction(start, end, created, now)),
				feeMonth: int64(float64(p.Monthly) * billing.Fraction(start, end, created, end))}
			for _, pc := range pcs {
				oc.byProject[pc.Project] = pc.Cents
				oc.total += pc.Cents
			}
			costs[b.OrgID] = oc
		}
		spent := oc.byProject[b.Project]
		forecast := billing.Forecast(spent, start, end, now)
		if b.Project == "" {
			spent = oc.total + oc.feeSoFar
			forecast = billing.Forecast(oc.total, start, end, now) + oc.feeMonth
		}
		a.checkBudget(ctx, b, start, spent, forecast)
	}
}

func (a *Agent) checkBudget(ctx context.Context, b budget, month time.Time, spent, forecast int64) {
	if b.Month == nil || !b.Month.Equal(month) { // a new month starts from zero
		b.Level, b.ForecastSent = 0, false
		_, _ = a.pool.Exec(ctx, `UPDATE alert_incidents SET status = 'resolved', resolved_at = now() WHERE budget_id = $1 AND status = 'firing'`, b.ID)
	}
	level := budgetLevel(spent, b.Limit, b.Thresholds)
	scope := "the organization"
	if b.Project != "" {
		scope = "project " + b.Project
	}
	cur := a.cfg.BillingCurrency
	switch {
	case level > b.Level:
		b.Level = level
		a.raiseBudget(ctx, b, fmt.Sprintf("Budget of %s: %d%% reached (%s spent of %s this month; forecast %s)",
			scope, level, billing.Money(spent, cur), billing.Money(b.Limit, cur), billing.Money(forecast, cur)))
	case b.Forecast && !b.ForecastSent && level < 100 && forecast > b.Limit:
		b.ForecastSent = true
		a.raiseBudget(ctx, b, fmt.Sprintf("Budget of %s: the forecast for the month (%s) is above the limit of %s (%s spent so far)",
			scope, billing.Money(forecast, cur), billing.Money(b.Limit, cur), billing.Money(spent, cur)))
	}
	if _, err := a.pool.Exec(ctx, `
		UPDATE budgets SET month = $2, level = $3, forecast_sent = $4, spent_cents = $5, forecast_cents = $6, checked_at = now() WHERE id = $1`,
		b.ID, month, b.Level, b.ForecastSent, spent, forecast); err != nil {
		a.log.Warn("budget update", "budget", b.ID, "err", err)
	}
}

// raiseBudget replaces the budget's open incident (if any) with a new one and notifies.
func (a *Agent) raiseBudget(ctx context.Context, b budget, summary string) {
	_, _ = a.pool.Exec(ctx, `UPDATE alert_incidents SET status = 'resolved', resolved_at = now() WHERE budget_id = $1 AND status = 'firing'`, b.ID)
	var id string
	if err := a.pool.QueryRow(ctx, `
		INSERT INTO alert_incidents (org_id, budget_id, kind, status, summary) VALUES ($1, $2, 'budget', 'firing', $3)
		ON CONFLICT (budget_id) WHERE status = 'firing' DO NOTHING RETURNING id`, b.OrgID, b.ID, truncate(summary, 500)).Scan(&id); err != nil {
		return
	}
	a.log.Info("budget alert", "budget", b.ID)
	a.queueNotifications(ctx, b.OrgID, id, "budget", "firing")
}
