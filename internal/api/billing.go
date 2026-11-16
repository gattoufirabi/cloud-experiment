package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/billing"
	"platform/internal/db"
)

// Billing page (admins and owners): this month's metered usage per resource, the
// estimated bill so far, past invoices and the billing details printed on them.

type usageRow struct {
	Kind, Name                        string
	VCPUHours, GBHours, GB, StorageGB float64
}

type invoiceRow struct {
	Number      string
	PeriodStart time.Time
	Total       int64
	Currency    string
	Status      string
	Issued      time.Time
}

func (s *Server) billingPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	now := time.Now().UTC()
	start, end := billing.MonthBounds(now)
	var rows []usageRow
	var total billing.Usage
	var invoices []invoiceRow
	var planName string
	var prices billing.Prices
	var created time.Time
	var billName, billAddr, billTax string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `
			SELECT p.name, p.price_monthly_cents, p.price_vcpu_hour_cents::float8, p.price_gb_hour_cents::float8, p.price_gb_out_cents::float8,
			       p.price_build_minute_cents::float8, p.price_storage_gb_month_cents::float8, o.created_at, o.billing_name, o.billing_address, o.billing_tax_id
			FROM orgs o JOIN plans p ON p.id = o.plan_id WHERE o.id = $1`, o.ID).
			Scan(&planName, &prices.Monthly, &prices.VCPUHour, &prices.GBHour, &prices.GBOut, &prices.BuildMin, &prices.StorageGBMonth, &created, &billName, &billAddr, &billTax); err != nil {
			return err
		}
		var err error
		if total.BuildSeconds, err = billing.BuildSeconds(r.Context(), tx, o.ID, start, end); err != nil {
			return err
		}
		q, err := tx.Query(r.Context(), `
			SELECT kind, (array_agg(name ORDER BY hour DESC))[1],
			       sum(cpu_milli_minutes)::bigint, sum(mem_mb_minutes)::bigint, sum(egress_bytes)::bigint, sum(storage_bytes)::bigint
			FROM usage_hourly WHERE hour >= $1 AND hour < $2
			GROUP BY resource_id, kind ORDER BY 2`, start, end)
		if err != nil {
			return err
		}
		for q.Next() {
			var u billing.Usage
			var row usageRow
			if err := q.Scan(&row.Kind, &row.Name, &u.CPUMilliMinutes, &u.MemMBMinutes, &u.EgressBytes, &u.StorageByteHours); err != nil {
				q.Close()
				return err
			}
			row.VCPUHours, row.GBHours, row.GB, row.StorageGB = u.VCPUHours(), u.GBHours(), u.GBOut(), u.StorageGBMonths()
			total.StorageByteHours += u.StorageByteHours
			total.CPUMilliMinutes += u.CPUMilliMinutes
			total.MemMBMinutes += u.MemMBMinutes
			total.EgressBytes += u.EgressBytes
			rows = append(rows, row)
		}
		q.Close()
		if err := q.Err(); err != nil {
			return err
		}
		iq, err := tx.Query(r.Context(), `
			SELECT number, period_start, total_cents, currency, status, issued_at FROM invoices ORDER BY period_start DESC LIMIT 36`)
		if err != nil {
			return err
		}
		invoices, err = pgx.CollectRows(iq, pgx.RowToStructByPos[invoiceRow])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	lines := billing.Lines(planName, prices, total, billing.Fraction(start, end, created, now))
	sub, tax, sum := billing.Totals(lines, s.cfg.BillingTaxPercent)
	s.render(w, r, "billing", http.StatusOK, data{
		"Org": o, "Usage": rows, "Lines": lines, "Subtotal": sub, "Tax": tax, "Total": sum,
		"Currency": s.cfg.BillingCurrency, "TaxPercent": s.cfg.BillingTaxPercent, "PlanName": planName, "Prices": prices,
		"Month": start, "Invoices": invoices, "BillingEnabled": s.cfg.BillingEnabled,
		"BillName": billName, "BillAddr": billAddr, "BillTax": billTax,
	})
}

func (s *Server) billingDetails(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("billing_name"))
	addr := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("billing_address"), "\r", ""))
	taxID := strings.TrimSpace(r.PostFormValue("billing_tax_id"))
	if len(name) > 200 || len(addr) > 500 || len(taxID) > 50 {
		back(w, r, o.Path()+"/billing", "", "Billing details are too long (name 200, address 500, tax ID 50 characters).")
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `UPDATE orgs SET billing_name = $2, billing_address = $3, billing_tax_id = $4 WHERE id = $1`,
			o.ID, name, addr, taxID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "billing.details", "org:"+o.Slug, nil)
		return nil
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/billing", "Billing details saved. They appear on the next invoices.", "")
}

type invoiceLine struct {
	Description string
	Quantity    float64
	Unit        string
	UnitPrice   float64
	Amount      int64
}

func (s *Server) invoicePage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	var inv struct {
		ID, Number, Currency, Seller, Buyer, Plan, Status string
		Start, End                                        time.Time
		Sub, Tax, Total                                   int64
		TaxPct                                            float64
		Issued                                            time.Time
		Paid                                              *time.Time
	}
	var lines []invoiceLine
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `
			SELECT id, number, currency, seller, buyer, plan_name, status, period_start, period_end,
			       subtotal_cents, tax_cents, total_cents, tax_percent::float8, issued_at, paid_at
			FROM invoices WHERE number = $1`, r.PathValue("number")).
			Scan(&inv.ID, &inv.Number, &inv.Currency, &inv.Seller, &inv.Buyer, &inv.Plan, &inv.Status, &inv.Start, &inv.End,
				&inv.Sub, &inv.Tax, &inv.Total, &inv.TaxPct, &inv.Issued, &inv.Paid); err != nil {
			return err
		}
		q, err := tx.Query(r.Context(), `
			SELECT description, quantity::float8, unit, unit_price_cents::float8, amount_cents
			FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, inv.ID)
		if err != nil {
			return err
		}
		lines, err = pgx.CollectRows(q, pgx.RowToStructByPos[invoiceLine])
		return err
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "invoice", http.StatusOK, data{"Org": o, "Invoice": inv, "Lines": lines, "LastDay": inv.End.AddDate(0, 0, -1)})
}
