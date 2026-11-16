package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// Operator console (/operator): the whole platform at a glance, organizations (plan,
// suspend/restore), invoices (paid/void) and the global audit log. Only users with
// is_operator and a full two-step session; everyone else gets "not found". Data comes
// from the operator_* SECURITY DEFINER functions (migration 018); every change is audited.

func (s *Server) operatorOnly(h http.HandlerFunc) http.HandlerFunc {
	return s.auth(authFull, func(w http.ResponseWriter, r *http.Request) {
		if !sessionFrom(r).IsOperator {
			s.notFound(w, r)
			return
		}
		h(w, r)
	})
}

type opOverview struct {
	Orgs, Users, LiveServices, Trials, Suspended, OpenIncidents, OpenInvoices, BuildsToday, DiskUsedMB int64
}

type opOrg struct {
	Slug, Name, PlanID, Owner string
	Services, Databases       int64
	DiskUsedMB, MaxDiskMB     int
	Suspended                 bool
	SuspendReason             string
	TrialEnds                 *time.Time
	Created                   time.Time
}

type opInvoice struct {
	Number, OrgSlug string
	PeriodStart     time.Time
	Total           int64
	Currency        string
	Status          string
	Issued          time.Time
}

type opEvent struct {
	At                               time.Time
	OrgSlug, Who, Action, Target, IP string
}

func (s *Server) operatorPage(w http.ResponseWriter, r *http.Request) {
	var ov opOverview
	if err := s.pool.QueryRow(r.Context(), `SELECT * FROM operator_overview()`).
		Scan(&ov.Orgs, &ov.Users, &ov.LiveServices, &ov.Trials, &ov.Suspended, &ov.OpenIncidents, &ov.OpenInvoices, &ov.BuildsToday, &ov.DiskUsedMB); err != nil {
		s.serverError(w, r, err)
		return
	}
	collect := func(sql string, fn func(pgx.CollectableRow) error) error {
		rows, err := s.pool.Query(r.Context(), sql)
		if err != nil {
			return err
		}
		_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) { return struct{}{}, fn(row) })
		return err
	}
	var orgs []opOrg
	var invoices []opInvoice
	var events []opEvent
	err := collect(`SELECT * FROM operator_orgs()`, func(row pgx.CollectableRow) error {
		var o opOrg
		err := row.Scan(&o.Slug, &o.Name, &o.PlanID, &o.Owner, &o.Services, &o.Databases, &o.DiskUsedMB, &o.MaxDiskMB,
			&o.Suspended, &o.SuspendReason, &o.TrialEnds, &o.Created)
		orgs = append(orgs, o)
		return err
	})
	if err == nil {
		err = collect(`SELECT * FROM operator_invoices()`, func(row pgx.CollectableRow) error {
			var i opInvoice
			err := row.Scan(&i.Number, &i.OrgSlug, &i.PeriodStart, &i.Total, &i.Currency, &i.Status, &i.Issued)
			invoices = append(invoices, i)
			return err
		})
	}
	if err == nil {
		err = collect(`SELECT * FROM operator_audit()`, func(row pgx.CollectableRow) error {
			var e opEvent
			err := row.Scan(&e.At, &e.OrgSlug, &e.Who, &e.Action, &e.Target, &e.IP)
			events = append(events, e)
			return err
		})
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var plans []string
	err = collect(`SELECT id FROM plans ORDER BY max_cpu_millis, id`, func(row pgx.CollectableRow) error {
		var id string
		err := row.Scan(&id)
		plans = append(plans, id)
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "operator", http.StatusOK, data{"Overview": ov, "Orgs": orgs, "Invoices": invoices, "Events": events, "Plans": plans})
}

func (s *Server) operatorSetPlan(w http.ResponseWriter, r *http.Request) {
	slug, plan := r.PathValue("slug"), r.PostFormValue("plan")
	var ok bool
	if err := s.pool.QueryRow(r.Context(), `SELECT operator_set_plan($1, $2)`, slug, plan).Scan(&ok); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		back(w, r, "/operator#op-orgs", "", "Unknown organization or plan.")
		return
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.set_plan", "org:"+slug, map[string]string{"plan": plan}, clientIP(r))
	back(w, r, "/operator#op-orgs", "Plan of "+slug+" set to "+plan+".", "")
}

func (s *Server) operatorSuspend(w http.ResponseWriter, r *http.Request) {
	slug, on := r.PathValue("slug"), r.PostFormValue("on") == "1"
	var ok bool
	if err := s.pool.QueryRow(r.Context(), `SELECT operator_suspend($1, $2)`, slug, on).Scan(&ok); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		back(w, r, "/operator#op-orgs", "", "Unknown organization.")
		return
	}
	action, msg := "operator.restore", slug+" restored: its applications start again."
	if on {
		action, msg = "operator.suspend", slug+" suspended: its applications stop within seconds."
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, action, "org:"+slug, nil, clientIP(r))
	back(w, r, "/operator#op-orgs", msg, "")
}

func (s *Server) operatorInvoice(w http.ResponseWriter, r *http.Request) {
	number, status := r.PathValue("number"), r.PostFormValue("status")
	var orgID *string
	err := s.pool.QueryRow(r.Context(), `SELECT operator_invoice_set($1, $2)`, number, status).Scan(&orgID)
	if err != nil || orgID == nil {
		back(w, r, "/operator#op-invoices", "", "No open invoice "+number+".")
		return
	}
	_ = db.Audit(r.Context(), s.pool, *orgID, sessionFrom(r).UserID, "invoice."+status, "invoice:"+number, map[string]bool{"by_operator": true}, clientIP(r))
	back(w, r, "/operator#op-invoices", "Invoice "+number+" marked "+status+".", "")
}
