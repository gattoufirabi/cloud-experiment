// Package billing turns metered usage into invoice lines. It is used by the agent
// (monthly invoices), the operator CLI (manual runs) and the console (estimates), so
// the three always agree on the numbers.
//
// Pricing model, per organization and calendar month (UTC):
//   - the plan's monthly fee, prorated from the day the organization was created;
//   - allocated CPU (vCPU-hours) and memory (GB-hours) of live services and running
//     databases, metered every minute;
//   - network out of services (GB, 10^9 bytes);
//   - object storage: bytes stored in buckets, sampled hourly (GB-month = 730 hours).
//
// All prices are in cents of one currency per installation (BILLING_CURRENCY).
package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"platform/internal/config"
)

type Prices struct {
	Monthly        int64   // cents per month
	VCPUHour       float64 // cents per vCPU-hour
	GBHour         float64 // cents per GB-hour of memory
	GBOut          float64 // cents per GB of network out
	BuildMin       float64 // cents per build minute
	StorageGBMonth float64 // cents per GB-month of object storage
	FnGBSecond     float64 // cents per GB-second of function run time
	FnMRequests    float64 // cents per million function requests
	QueueMRequests float64 // cents per million queue requests
	EmailThousand  float64 // cents per thousand emails (per recipient)
}

type Usage struct {
	CPUMilliMinutes  int64
	MemMBMinutes     int64
	EgressBytes      int64
	BuildSeconds     int64
	StorageByteHours int64 // sum of hourly samples of stored bytes
	FnRequests       int64
	FnMBms           int64 // memory (MB) x run time (ms) of function invocations
	QueueRequests    int64
	Emails           int64
}

func (u Usage) VCPUHours() float64       { return float64(u.CPUMilliMinutes) / 1000 / 60 }
func (u Usage) GBHours() float64         { return float64(u.MemMBMinutes) / 1024 / 60 }
func (u Usage) GBOut() float64           { return float64(u.EgressBytes) / 1e9 }
func (u Usage) BuildMinutes() float64    { return float64(u.BuildSeconds) / 60 }
func (u Usage) StorageGBMonths() float64 { return float64(u.StorageByteHours) / 1e9 / 730 }
func (u Usage) FnGBSeconds() float64     { return float64(u.FnMBms) / 1024 / 1000 }
func (u Usage) FnMRequests() float64     { return float64(u.FnRequests) / 1e6 }

type Line struct {
	Description string
	Quantity    float64
	Unit        string
	UnitPrice   float64 // cents
	Amount      int64   // cents
}

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

// Lines prices one month of usage. fraction is the share of the month the plan fee
// applies to (1 for a full month).
func Lines(planName string, p Prices, u Usage, fraction float64) []Line {
	var out []Line
	if p.Monthly > 0 && fraction > 0 {
		q := round4(fraction)
		out = append(out, Line{"Plan " + planName + " (monthly fee)", q, "month", float64(p.Monthly), int64(math.Round(float64(p.Monthly) * q))})
	}
	add := func(desc, unit string, qty, price float64) {
		qty = round4(qty)
		if qty > 0 {
			out = append(out, Line{desc, qty, unit, price, int64(math.Round(qty * price))})
		}
	}
	add("CPU allocated to services and databases", "vCPU-hour", u.VCPUHours(), p.VCPUHour)
	add("Memory allocated to services and databases", "GB-hour", u.GBHours(), p.GBHour)
	add("Network out", "GB", u.GBOut(), p.GBOut)
	add("Builds from git", "build minute", u.BuildMinutes(), p.BuildMin)
	add("Object storage", "GB-month", u.StorageGBMonths(), p.StorageGBMonth)
	add("Function requests", "million requests", u.FnMRequests(), p.FnMRequests)
	add("Function run time", "GB-second", u.FnGBSeconds(), p.FnGBSecond)
	add("Queue requests", "million requests", float64(u.QueueRequests)/1e6, p.QueueMRequests)
	add("Emails sent", "thousand emails", float64(u.Emails)/1e3, p.EmailThousand)
	return out
}

func Totals(lines []Line, taxPercent float64) (subtotal, tax, total int64) {
	for _, l := range lines {
		subtotal += l.Amount
	}
	tax = int64(math.Round(float64(subtotal) * taxPercent / 100))
	return subtotal, tax, subtotal + tax
}

// MonthBounds returns [start, end) of the UTC calendar month containing t.
func MonthBounds(t time.Time) (time.Time, time.Time) {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// Fraction is the share of [start, end) during which an organization created at
// `created` existed, up to `until` (end for a finished month, now for an estimate).
func Fraction(start, end, created, until time.Time) float64 {
	from := start
	if created.After(from) {
		from = created
	}
	if until.After(end) {
		until = end
	}
	if !until.After(from) {
		return 0
	}
	return until.Sub(from).Seconds() / end.Sub(start).Seconds()
}

// Money formats cents as "12.34 EUR".
func Money(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, cents/100, cents%100, currency)
}

// ErrMonthNotOver is returned when asked to invoice a month that has not ended yet.
var ErrMonthNotOver = errors.New("the month is not over yet")

// Generate issues the invoices for the month containing `month`, once per
// organization (safe to run repeatedly). Organizations with nothing to pay get none.
// It returns how many invoices were created.
func Generate(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, month time.Time) (int, error) {
	start, end := MonthBounds(month)
	if time.Now().Before(end.Add(10 * time.Minute)) { // let the last minutes be metered
		return 0, ErrMonthNotOver
	}
	type orgRow struct {
		ID, Name, BillName, BillAddr, BillTax, Plan string
		Created                                     time.Time
		P                                           Prices
	}
	rows, err := pool.Query(ctx, `
		SELECT o.id, o.name, o.billing_name, o.billing_address, o.billing_tax_id, p.name, o.created_at,
		       p.price_monthly_cents, p.price_vcpu_hour_cents::float8, p.price_gb_hour_cents::float8, p.price_gb_out_cents::float8,
		       p.price_build_minute_cents::float8, p.price_storage_gb_month_cents::float8,
		       p.price_fn_gb_second_cents::float8, p.price_fn_million_requests_cents::float8, p.price_queue_million_requests_cents::float8,
		       p.price_email_thousand_cents::float8
		FROM orgs o JOIN plans p ON p.id = o.plan_id
		WHERE o.created_at < $2
		  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.org_id = o.id AND i.period_start = $1::date)
		ORDER BY o.created_at`, start, end)
	if err != nil {
		return 0, err
	}
	orgs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (orgRow, error) {
		var o orgRow
		err := r.Scan(&o.ID, &o.Name, &o.BillName, &o.BillAddr, &o.BillTax, &o.Plan, &o.Created,
			&o.P.Monthly, &o.P.VCPUHour, &o.P.GBHour, &o.P.GBOut, &o.P.BuildMin, &o.P.StorageGBMonth, &o.P.FnGBSecond, &o.P.FnMRequests, &o.P.QueueMRequests, &o.P.EmailThousand)
		return o, err
	})
	if err != nil {
		return 0, err
	}
	created := 0
	for _, o := range orgs {
		var u Usage
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(sum(cpu_milli_minutes), 0)::bigint, coalesce(sum(mem_mb_minutes), 0)::bigint, coalesce(sum(egress_bytes), 0)::bigint,
			       coalesce(sum(storage_bytes), 0)::bigint, coalesce(sum(fn_requests), 0)::bigint, coalesce(sum(fn_mb_ms), 0)::bigint,
			       coalesce(sum(queue_requests), 0)::bigint, coalesce(sum(emails), 0)::bigint
			FROM usage_hourly WHERE org_id = $1 AND hour >= $2 AND hour < $3`, o.ID, start, end).
			Scan(&u.CPUMilliMinutes, &u.MemMBMinutes, &u.EgressBytes, &u.StorageByteHours, &u.FnRequests, &u.FnMBms, &u.QueueRequests, &u.Emails); err != nil {
			return created, err
		}
		if u.BuildSeconds, err = BuildSeconds(ctx, pool, o.ID, start, end); err != nil {
			return created, err
		}
		lines := Lines(o.Plan, o.P, u, Fraction(start, end, o.Created, end))
		sub, tax, total := Totals(lines, cfg.BillingTaxPercent)
		if total == 0 {
			continue
		}
		buyer := o.BillName
		if buyer == "" {
			buyer = o.Name
		}
		if o.BillAddr != "" {
			buyer += "\n" + o.BillAddr
		}
		if o.BillTax != "" {
			buyer += "\nTax ID: " + o.BillTax
		}
		ok, err := insertInvoice(ctx, pool, cfg, o.ID, start, end, buyer, o.Plan, lines, sub, tax, total)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

func insertInvoice(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, orgID string, start, end time.Time,
	buyer, plan string, lines []Line, sub, tax, total int64) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT nextval('invoice_seq')`).Scan(&seq); err != nil {
		return false, err
	}
	number := fmt.Sprintf("INV-%d-%06d", start.Year(), seq)
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO invoices (org_id, number, period_start, period_end, currency, seller, buyer, plan_name,
		                      subtotal_cents, tax_percent, tax_cents, total_cents)
		VALUES ($1, $2, $3::date, $4::date, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (org_id, period_start) DO NOTHING RETURNING id`,
		orgID, number, start, end, cfg.BillingCurrency, strings.TrimSpace(cfg.BillingSeller), buyer, plan,
		sub, cfg.BillingTaxPercent, tax, total).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // generated concurrently
	}
	if err != nil {
		return false, err
	}
	for i, l := range lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_lines (invoice_id, org_id, position, description, quantity, unit, unit_price_cents, amount_cents)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, orgID, i+1, l.Description, l.Quantity, l.Unit, l.UnitPrice, l.Amount); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT audit_append($1, NULL, 'invoice.issued', $2, jsonb_build_object('total_cents', $3::bigint), NULL)`,
		orgID, "invoice:"+number, total); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Querier is satisfied by a pool and a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BuildSeconds is the time an organization's builds ran in [start, end).
func BuildSeconds(ctx context.Context, q Querier, orgID string, start, end time.Time) (int64, error) {
	var s int64
	err := q.QueryRow(ctx, `
		SELECT coalesce(sum(extract(epoch FROM finished_at - started_at)), 0)::bigint FROM builds
		WHERE org_id = $1 AND runner = 'platform' AND started_at IS NOT NULL AND finished_at >= $2 AND finished_at < $3`, orgID, start, end).Scan(&s)
	return s, err
}
