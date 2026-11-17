package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Admin panel pages of the operator area: platform settings, plans (limits and
// prices), every resource of every organization (metadata only), platform health,
// and invoice generation. All changes are audited.

// ---------- settings ----------

func (s *Server) operatorSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.dropSettingsCache()
	s.render(w, r, "operator_settings", http.StatusOK, data{"S": s.settings(r.Context()), "OpSection": "settings",
		"Env": map[string]any{"SignupEnabled": s.cfg.SignupEnabled, "SignupDailyLimit": s.cfg.SignupDailyLimit, "SignupTrialDays": s.cfg.SignupTrialDays,
			"SignupAllowed": strings.Join(s.cfg.SignupAllowedDomains, ", "), "SignupBlocked": strings.Join(s.cfg.SignupBlockedDomains, ", ")}})
}

var domainListRe = regexp.MustCompile(`^[a-z0-9.-]+$`)

func parseDomainList(v string) ([]string, bool) {
	out := []string{}
	for _, d := range strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' }) {
		if !domainListRe.MatchString(d) || len(d) > 253 {
			return nil, false
		}
		out = append(out, d)
	}
	return out, true
}

func (s *Server) operatorSaveSettings(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) { back(w, r, "/operator/settings", "", msg) }
	// Each setting: "env" keeps the environment value, otherwise the given value.
	var enabled *bool
	switch r.PostFormValue("signup_enabled") {
	case "on":
		t := true
		enabled = &t
	case "off":
		f := false
		enabled = &f
	}
	optInt := func(name string, lo, hi int) (*int, bool) {
		v := strings.TrimSpace(r.PostFormValue(name))
		if v == "" {
			return nil, true
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return nil, false
		}
		return &n, true
	}
	limit, ok1 := optInt("signup_daily_limit", 0, 100000)
	days, ok2 := optInt("signup_trial_days", 1, 365)
	if !ok1 || !ok2 {
		fail("Daily signups: 0 to 100000; trial length: 1 to 365 days (leave empty to use the server's value).")
		return
	}
	optList := func(name string) ([]string, bool) {
		if r.PostFormValue(name+"_env") == "1" {
			return nil, true
		}
		return parseDomainList(r.PostFormValue(name))
	}
	allowed, ok3 := optList("signup_allowed_domains")
	blocked, ok4 := optList("signup_blocked_domains")
	if !ok3 || !ok4 {
		fail("Email domains: names such as example.com, separated by commas.")
		return
	}
	banner := strings.TrimSpace(r.PostFormValue("banner_message"))
	level := r.PostFormValue("banner_level")
	if len(banner) > 300 || (level != "info" && level != "warning") {
		fail("Announcement: at most 300 characters, level info or warning.")
		return
	}
	paused := r.PostFormValue("deploys_paused") == "1"
	before := s.settings(r.Context())
	if _, err := s.pool.Exec(r.Context(), `
		UPDATE platform_settings SET signup_enabled = $1, signup_daily_limit = $2, signup_trial_days = $3,
		       signup_allowed_domains = $4, signup_blocked_domains = $5, banner_message = $6, banner_level = $7,
		       deploys_paused = $8, updated_at = now() WHERE id = 1`,
		enabled, limit, days, allowed, blocked, banner, level, paused); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.dropSettingsCache()
	if !paused && before.DeploysPaused {
		_, _ = s.pool.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	}
	opAudit(r, s, "", "platform.settings_changed", "platform", map[string]any{"signup_enabled": enabled, "banner": banner != "", "deploys_paused": paused})
	back(w, r, "/operator/settings", "Settings saved.", "")
}

// ---------- plans ----------

type planField struct {
	Col, Label, Kind, Group string // Kind: int, cents (integer cents), price (fractional cents), bool
}

var planFields = []planField{
	{"name", "Display name", "text", "Plan"},
	{"price_monthly_cents", "Monthly price (cents)", "cents", "Prices"},
	{"price_vcpu_hour_cents", "vCPU-hour (cents)", "price", "Prices"},
	{"price_gb_hour_cents", "GB-hour of memory (cents)", "price", "Prices"},
	{"price_gb_out_cents", "GB out (cents)", "price", "Prices"},
	{"price_build_minute_cents", "Build minute (cents)", "price", "Prices"},
	{"price_storage_gb_month_cents", "Object storage GB-month (cents)", "price", "Prices"},
	{"price_fn_gb_second_cents", "Function GB-second (cents)", "price", "Prices"},
	{"price_fn_million_requests_cents", "Function million requests (cents)", "price", "Prices"},
	{"price_queue_million_requests_cents", "Queue million requests (cents)", "price", "Prices"},
	{"price_email_thousand_cents", "Thousand emails (cents)", "price", "Prices"},
	{"max_services", "Services", "int", "Limits"},
	{"max_instances", "Instances per service", "int", "Limits"},
	{"max_cpu_millis", "CPU (millicores, total)", "int", "Limits"},
	{"max_memory_mb", "Memory (MB, total)", "int", "Limits"},
	{"max_disk_mb", "Disk (MB)", "int", "Limits"},
	{"max_databases", "Databases", "int", "Limits"},
	{"max_domains", "Custom domains", "int", "Limits"},
	{"max_buckets", "Buckets", "int", "Limits"},
	{"max_storage_mb", "Object storage (MB)", "int", "Limits"},
	{"max_functions", "Functions", "int", "Limits"},
	{"max_queues", "Queues", "int", "Limits"},
	{"max_secrets", "Secrets", "int", "Limits"},
	{"max_public_ports", "Public TCP/UDP ports", "int", "Limits"},
	{"max_email_domains", "Email sending domains", "int", "Limits"},
	{"max_emails_day", "Emails a day", "int", "Limits"},
	{"log_retention_days", "Log retention (days)", "int", "Limits"},
	{"allow_builds", "Builds from git allowed", "bool", "Limits"},
}

type planRow struct {
	ID     string
	Values map[string]string
	Orgs   int
}

func planSelect() string {
	cols := make([]string, len(planFields))
	for i, f := range planFields {
		cols[i] = f.Col + "::text"
	}
	return `SELECT id, ` + strings.Join(cols, ", ") + `, (SELECT count(*) FROM orgs o WHERE o.plan_id = plans.id)::int FROM plans`
}

func (s *Server) operatorPlansPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), planSelect()+` ORDER BY max_cpu_millis, id`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (planRow, error) {
		vals := make([]string, len(planFields))
		dest := []any{new(string)}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var orgs int
		dest = append(dest, &orgs)
		if err := row.Scan(dest...); err != nil {
			return planRow{}, err
		}
		p := planRow{ID: *dest[0].(*string), Values: map[string]string{}, Orgs: orgs}
		for i, f := range planFields {
			p.Values[f.Col] = vals[i]
		}
		return p, nil
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "operator_plans", http.StatusOK, data{"Plans": plans, "Fields": planFields, "OpSection": "plans", "Currency": s.cfg.BillingCurrency})
}

var planIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,29}$`)

func (s *Server) operatorCreatePlan(w http.ResponseWriter, r *http.Request) {
	id, from := strings.TrimSpace(r.PostFormValue("id")), r.PostFormValue("from")
	if !planIDRe.MatchString(id) {
		back(w, r, "/operator/plans#create", "", "Plan ID: 2 to 30 lowercase letters, digits and dashes.")
		return
	}
	cols := make([]string, 0, len(planFields))
	for _, f := range planFields {
		if f.Col != "name" {
			cols = append(cols, f.Col)
		}
	}
	tag, err := s.pool.Exec(r.Context(), `INSERT INTO plans (id, name, `+strings.Join(cols, ", ")+`)
		SELECT $1, $2, `+strings.Join(cols, ", ")+` FROM plans WHERE id = $3`, id, strings.ToUpper(id[:1])+id[1:], from)
	if isUniqueViolation(err) {
		back(w, r, "/operator/plans#create", "", "A plan with this ID already exists.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		back(w, r, "/operator/plans#create", "", "Choose a plan to copy.")
		return
	}
	opAudit(r, s, "", "plan.created", "plan:"+id, map[string]string{"copied_from": from})
	back(w, r, "/operator/plans#plan-"+id, "Plan "+id+" created from "+from+": adjust its limits and prices.", "")
}

func (s *Server) operatorUpdatePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sets := []string{}
	args := []any{id}
	changed := map[string]string{}
	for _, f := range planFields {
		v := strings.TrimSpace(r.PostFormValue(f.Col))
		switch f.Kind {
		case "bool":
			v = strconv.FormatBool(v == "1")
		case "text":
			if v == "" || len(v) > 60 {
				back(w, r, "/operator/plans#plan-"+id, "", f.Label+": 1 to 60 characters.")
				return
			}
		case "int", "cents":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				back(w, r, "/operator/plans#plan-"+id, "", f.Label+": a whole number, 0 or more.")
				return
			}
		case "price":
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || n < 0 || n > 1e6 {
				back(w, r, "/operator/plans#plan-"+id, "", f.Label+": a number, 0 or more.")
				return
			}
		}
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d::text::%s", f.Col, len(args), map[string]string{"bool": "boolean", "text": "text", "int": "int", "cents": "int", "price": "numeric"}[f.Kind]))
		changed[f.Col] = v
	}
	tag, err := s.pool.Exec(r.Context(), `UPDATE plans SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...)
	if err != nil {
		back(w, r, "/operator/plans#plan-"+id, "", "The database refused a value: "+truncateStr(err.Error(), 120))
		return
	}
	if tag.RowsAffected() == 0 {
		s.notFound(w, r)
		return
	}
	opAudit(r, s, "", "plan.updated", "plan:"+id, changed)
	back(w, r, "/operator/plans#plan-"+id, "Plan "+id+" saved. Limits apply to the next change each organization makes; prices to the next invoice.", "")
}

func (s *Server) operatorDeletePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.pool.Exec(r.Context(), `DELETE FROM plans WHERE id = $1 AND id NOT IN ('trial', 'starter', 'pro') AND NOT EXISTS (SELECT 1 FROM orgs WHERE plan_id = $1)`, id)
	if err != nil || tag.RowsAffected() == 0 {
		back(w, r, "/operator/plans#plan-"+id, "", "Only an unused plan that is not one of the built-in plans (trial, starter, pro) can be deleted.")
		return
	}
	opAudit(r, s, "", "plan.deleted", "plan:"+id, nil)
	back(w, r, "/operator/plans", "Plan "+id+" deleted.", "")
}

// ---------- resources ----------

type opResource struct {
	Kind, OrgSlug, Project, Name, Detail, Status string
	Created                                      time.Time
}

func (s *Server) operatorResourcesPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `SELECT * FROM operator_resources()`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[opResource])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	kind, q := r.URL.Query().Get("kind"), strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	counts := map[string]int{}
	var shown []opResource
	for _, x := range all {
		counts[x.Kind]++
		if kind != "" && x.Kind != kind {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(x.OrgSlug+" "+x.Project+" "+x.Name+" "+x.Detail), q) {
			continue
		}
		shown = append(shown, x)
	}
	kinds := []string{"service", "database", "function", "bucket", "domain", "queue", "email domain"}
	s.render(w, r, "operator_resources", http.StatusOK, data{"Resources": shown, "Counts": counts, "Kinds": kinds, "Kind": kind, "Q": q, "Total": len(all), "OpSection": "resources"})
}

// ---------- health ----------

func (s *Server) operatorHealthPage(w http.ResponseWriter, r *http.Request) {
	type metric struct {
		Name  string
		Value int64
		Bad   bool
	}
	type failure struct {
		At                           time.Time
		Kind, OrgSlug, What, Message string
	}
	rows, err := s.pool.Query(r.Context(), `SELECT * FROM operator_health()`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	metrics, err := pgx.CollectRows(rows, pgx.RowToStructByPos[metric])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err = s.pool.Query(r.Context(), `SELECT * FROM operator_failures()`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	failures, err := pgx.CollectRows(rows, pgx.RowToStructByPos[failure])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	nodes, err := s.loadNodes(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var dbSize int64
	_ = s.pool.QueryRow(r.Context(), `SELECT pg_database_size(current_database())`).Scan(&dbSize)
	s.render(w, r, "operator_health", http.StatusOK, data{"Metrics": metrics, "Failures": failures, "Nodes": nodes, "DBSize": dbSize,
		"Paused": s.settings(r.Context()).DeploysPaused, "OpSection": "health"})
}

// ---------- invoices ----------

func (s *Server) operatorGenerateInvoices(w http.ResponseWriter, r *http.Request) {
	month, err := time.Parse("2006-01", r.PostFormValue("month"))
	if err != nil || !month.Before(time.Now().UTC().AddDate(0, 0, -time.Now().UTC().Day()+1)) {
		back(w, r, "/operator#op-invoices", "", "Choose a past month, such as 2026-10.")
		return
	}
	if _, err := s.pool.Exec(r.Context(), `SELECT pg_notify('deployments', $1)`, "invoices:"+month.Format("2006-01")); err != nil {
		s.serverError(w, r, err)
		return
	}
	opAudit(r, s, "", "invoices.generate_requested", "month:"+month.Format("2006-01"), nil)
	back(w, r, "/operator#op-invoices", "Generating the invoices of "+month.Format("January 2006")+" (organizations already invoiced are skipped); reload in a few seconds.", "")
}
