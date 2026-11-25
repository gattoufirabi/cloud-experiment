package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/billing"
	"platform/internal/lib/db"
)

// Cost reports (cost per project this month, its forecast and last month) and budgets
// (a monthly limit for the organization or one project; the agent notifies the alert
// channels when a threshold is crossed). Admins only, like billing.

type costRow struct {
	Project   string `json:"project"` // "" = organization-wide (object storage, queues, email, plan fee)
	Cents     int64  `json:"month_to_date_cents"`
	Forecast  int64  `json:"forecast_cents"`
	LastMonth int64  `json:"last_month_cents"`
}

type costReport struct {
	Month      string    `json:"month"` // YYYY-MM
	Currency   string    `json:"currency"`
	Projects   []costRow `json:"projects"`
	Total      int64     `json:"month_to_date_cents"`
	Forecast   int64     `json:"forecast_cents"`
	LastMonth  int64     `json:"last_month_cents"`
	PlanFee    int64     `json:"plan_fee_cents"` // part of the month's plan fee so far (in the organization-wide row)
	BeforeTax  bool      `json:"before_tax"`
	Generated  time.Time `json:"generated_at"`
	monthStart time.Time
}

type budgetView struct {
	ID         string     `json:"id"`
	Project    string     `json:"project"` // "" = whole organization
	Limit      int64      `json:"limit_cents"`
	Thresholds []int32    `json:"thresholds"`
	Forecast   bool       `json:"forecast_alert"`
	Spent      int64      `json:"spent_cents"`
	Projected  int64      `json:"forecast_cents"`
	Checked    *time.Time `json:"checked_at"`
}

// Percent is the share of the limit spent so far (for the page).
func (b budgetView) Percent() int64 {
	if b.Limit <= 0 {
		return 0
	}
	return b.Spent * 100 / b.Limit
}

func orgPrices(ctx context.Context, tx pgx.Tx, orgID string) (billing.Prices, time.Time, error) {
	var p billing.Prices
	var created time.Time
	err := tx.QueryRow(ctx, `
		SELECT p.price_monthly_cents, p.price_vcpu_hour_cents::float8, p.price_gb_hour_cents::float8, p.price_gb_out_cents::float8,
		       p.price_build_minute_cents::float8, p.price_storage_gb_month_cents::float8,
		       p.price_fn_gb_second_cents::float8, p.price_fn_million_requests_cents::float8, p.price_queue_million_requests_cents::float8,
		       p.price_email_thousand_cents::float8, o.created_at
		FROM orgs o JOIN plans p ON p.id = o.plan_id WHERE o.id = $1`, orgID).
		Scan(&p.Monthly, &p.VCPUHour, &p.GBHour, &p.GBOut, &p.BuildMin, &p.StorageGBMonth, &p.FnGBSecond, &p.FnMRequests, &p.QueueMRequests, &p.EmailThousand, &created)
	return p, created, err
}

// loadCosts reports the month containing `at`: month to date (or the whole month when it
// is over), its forecast and the month before.
func (s *API) loadCosts(ctx context.Context, tx pgx.Tx, orgID string, at time.Time) (costReport, error) {
	now := time.Now().UTC()
	start, end := billing.MonthBounds(at)
	prevStart, prevEnd := billing.MonthBounds(start.AddDate(0, 0, -1))
	rep := costReport{Month: start.Format("2006-01"), Currency: s.Cfg.BillingCurrency, BeforeTax: true, Generated: now, monthStart: start}
	p, created, err := orgPrices(ctx, tx, orgID)
	if err != nil {
		return rep, err
	}
	cur, err := billing.CostByProject(ctx, tx, orgID, p, start, end)
	if err != nil {
		return rep, err
	}
	prev, err := billing.CostByProject(ctx, tx, orgID, p, prevStart, prevEnd)
	if err != nil {
		return rep, err
	}
	until := now
	if until.After(end) {
		until = end
	}
	rows := map[string]*costRow{}
	get := func(k string) *costRow {
		if rows[k] == nil {
			rows[k] = &costRow{Project: k}
		}
		return rows[k]
	}
	for _, c := range cur {
		r := get(c.Project)
		r.Cents = c.Cents
		r.Forecast = billing.Forecast(c.Cents, start, end, until)
	}
	for _, c := range prev {
		get(c.Project).LastMonth = c.Cents
	}
	// the plan fee belongs to the organization-wide row
	rep.PlanFee = int64(math.Round(float64(p.Monthly) * billing.Fraction(start, end, created, until)))
	if p.Monthly > 0 {
		org := get("")
		org.Cents += rep.PlanFee
		org.Forecast += int64(math.Round(float64(p.Monthly) * billing.Fraction(start, end, created, end)))
		org.LastMonth += int64(math.Round(float64(p.Monthly) * billing.Fraction(prevStart, prevEnd, created, prevEnd)))
	}
	for _, r := range rows {
		rep.Projects = append(rep.Projects, *r)
		rep.Total += r.Cents
		rep.Forecast += r.Forecast
		rep.LastMonth += r.LastMonth
	}
	sort.Slice(rep.Projects, func(i, j int) bool {
		a, b := rep.Projects[i], rep.Projects[j]
		if (a.Project == "") != (b.Project == "") {
			return b.Project == ""
		}
		if a.Cents != b.Cents {
			return a.Cents > b.Cents
		}
		return a.Project < b.Project
	})
	if rep.Projects == nil {
		rep.Projects = []costRow{}
	}
	return rep, nil
}

func loadBudgets(ctx context.Context, tx pgx.Tx) ([]budgetView, error) {
	rows, err := tx.Query(ctx, `
		SELECT b.id, coalesce(p.slug, ''), b.limit_cents, b.thresholds, b.forecast, b.spent_cents, b.forecast_cents, b.checked_at
		FROM budgets b LEFT JOIN projects p ON p.id = b.project_id ORDER BY p.slug NULLS FIRST`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[budgetView])
	if out == nil {
		out = []budgetView{}
	}
	return out, err
}

type budgetInput struct {
	Project    string `json:"project"` // "" = whole organization
	Limit      int64  `json:"limit_cents"`
	Thresholds []int  `json:"thresholds"`
	Forecast   *bool  `json:"forecast_alert"`
}

// parseMoney turns "25", "25.5" or "25,50" into cents.
func parseMoney(v string) (int64, bool) {
	v = strings.ReplaceAll(strings.TrimSpace(v), ",", ".")
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 || f > 1e9 || math.IsNaN(f) {
		return 0, false
	}
	return int64(math.Round(f * 100)), true
}

func parseThresholds(v string) ([]int, bool) {
	var out []int
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '%' }) {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// saveBudgetTx creates the budget of a scope or replaces its settings (one per scope).
func (s *API) saveBudgetTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, in budgetInput) error {
	in.Project = strings.TrimSpace(in.Project)
	if len(in.Thresholds) == 0 {
		in.Thresholds = []int{50, 80, 100}
	}
	sort.Ints(in.Thresholds)
	switch {
	case in.Limit <= 0 || in.Limit > 1e11:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "limit: a positive amount"}
	case len(in.Thresholds) > 5:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "thresholds: at most 5"}
	case in.Thresholds[0] < 1 || in.Thresholds[len(in.Thresholds)-1] > 500:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "thresholds: percentages of the limit, 1 to 500"}
	}
	var projectID *string
	if in.Project != "" {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1`, in.Project).Scan(&id); err != nil {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "project " + in.Project + " not found"}
		}
		projectID = &id
	}
	forecast := in.Forecast == nil || *in.Forecast
	th := make([]int32, len(in.Thresholds))
	for i, t := range in.Thresholds {
		th[i] = int32(t)
	}
	// changing a budget starts its notifications over for the month
	if _, err := tx.Exec(ctx, `
		INSERT INTO budgets (org_id, project_id, limit_cents, thresholds, forecast) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, project_id) DO UPDATE SET limit_cents = EXCLUDED.limit_cents, thresholds = EXCLUDED.thresholds,
		    forecast = EXCLUDED.forecast, level = 0, forecast_sent = false`,
		o.ID, projectID, in.Limit, th, forecast); err != nil {
		return err
	}
	target := "budget:organization"
	if in.Project != "" {
		target = "budget:project/" + in.Project
	}
	s.Audit(r, tx, o.ID, "budget.saved", target, map[string]any{"limit_cents": in.Limit, "thresholds": in.Thresholds, "forecast_alert": forecast})
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'budgets')`)
	return err
}

func (s *API) deleteBudgetTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, id string) error {
	var project string
	if err := tx.QueryRow(ctx, `
		DELETE FROM budgets b WHERE b.id::text = $1
		RETURNING coalesce((SELECT slug FROM projects WHERE id = b.project_id), '')`, id).Scan(&project); err != nil {
		return err
	}
	target := "budget:organization"
	if project != "" {
		target = "budget:project/" + project
	}
	s.Audit(r, tx, o.ID, "budget.deleted", target, nil)
	return nil
}

// ---------- console ----------

func (s *API) saveBudget(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	limit, lok := parseMoney(r.PostFormValue("limit"))
	th, tok := parseThresholds(r.PostFormValue("thresholds"))
	f := r.PostFormValue("forecast") == "1"
	in := budgetInput{Project: r.PostFormValue("project"), Limit: limit, Thresholds: th, Forecast: &f}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		switch {
		case !lok:
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "enter the monthly limit as an amount, for example 50 or 49.90"}
		case !tok:
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "thresholds: percentages separated by commas, for example 50, 80, 100"}
		}
		return s.saveBudgetTx(r.Context(), r, tx, o, in)
	})
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/billing#budgets", "", "Budget not saved: "+ae.Msg+".")
	case err != nil:
		s.ServerError(w, r, err)
	default:
		scope := "the organization"
		if in.Project != "" {
			scope = "project " + in.Project
		}
		web.Back(w, r, o.Path()+"/billing#budgets", fmt.Sprintf("Budget of %s saved: %s per month. It is checked every hour.", scope, billing.Money(limit, s.Cfg.BillingCurrency)), "")
	}
}

func (s *API) deleteBudget(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.deleteBudgetTx(r.Context(), r, tx, o, r.PathValue("id")) })
	switch {
	case web.IsNoRows(err):
		web.Back(w, r, o.Path()+"/billing#budgets", "", "Budget not found.")
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, o.Path()+"/billing#budgets", "Budget deleted.", "")
	}
}

// ---------- REST ----------

func (s *API) apiCosts(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	at := time.Now().UTC()
	if m := r.URL.Query().Get("month"); m != "" {
		t, err := time.Parse("2006-01", m)
		if err != nil || t.After(at) {
			web.APIError(w, http.StatusBadRequest, "month: YYYY-MM, not in the future")
			return
		}
		at = t
	}
	var rep costReport
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		rep, err = s.loadCosts(r.Context(), tx, o.ID, at)
		return err
	})
	if web.APITx(w, s.Server, err) {
		web.WriteJSON(w, rep)
	}
}

func (s *API) apiBudgets(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []budgetView
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		out, err = loadBudgets(r.Context(), tx)
		return err
	})
	if web.APITx(w, s.Server, err) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiSaveBudget(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in budgetInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"project": "shop", "limit_cents": 5000, "thresholds": [50, 80, 100], "forecast_alert": true}`)
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.saveBudgetTx(r.Context(), r, tx, o, in) })
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *API) apiDeleteBudget(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.deleteBudgetTx(r.Context(), r, tx, o, r.PathValue("id")) })
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}
