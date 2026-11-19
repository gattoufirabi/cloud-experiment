package api

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"platform/internal/db"
)

// Feature switches: the operator turns a service off for everyone, for a plan or for one
// organization (platform_settings, plans and orgs .features_off). A service that is off
// leaves the console's menus; its pages and its API answer that it is off. Projects,
// access management and billing are always on.

var alwaysOn = map[string]bool{"projects": true, "iam": true, "billing": true}

// switchable lists the services the operator can turn off, in menu order.
func (s *Server) switchable() []*product {
	var out []*product
	for i := range products {
		if p := &products[i]; !alwaysOn[p.ID] && s.productEnabled(p) {
			out = append(out, p)
		}
	}
	return out
}

func validFeature(id string) bool {
	p := productByID(id)
	return p != nil && !alwaysOn[id]
}

// loadFeaturesOff fills o.Off with the services that are off for the organization.
func (s *Server) loadFeaturesOff(ctx context.Context, o *orgInfo) {
	var off []string
	if err := s.pool.QueryRow(ctx, `SELECT org_features_off($1)`, o.ID).Scan(&off); err != nil {
		return
	}
	o.Off = map[string]bool{}
	for _, f := range off {
		o.Off[f] = true
	}
}

// featureOffPage answers a console page of a service that is off.
func (s *Server) featureOffPage(w http.ResponseWriter, r *http.Request, p *product) {
	s.render(w, r, "error", http.StatusForbidden, data{"Title": p.Name + " is turned off",
		"Text": p.Name + " is not available for this organization. Ask the platform's operator if you need it."})
}

// apiAreas maps the first part of an API path (/api/v1/<part>/...) to its service.
var apiAreas = map[string]string{
	"databases": "databases", "storage": "storage", "functions": "functions", "queues": "queues",
	"email": "email", "mail": "mail", "dns": "dns", "domains": "domains", "secrets": "secrets",
	"registry": "registry", "alarms": "monitoring", "incidents": "monitoring", "logs": "monitoring",
	"analytics": "analytics",
}

// apiArea names the service an API request belongs to ("" when none).
func apiArea(path string) string {
	rest := strings.TrimPrefix(path, "/api/v1/")
	parts := strings.Split(rest, "/")
	if parts[0] == "projects" && len(parts) >= 3 { // /api/v1/projects/{p}/<kind>/...
		switch parts[2] {
		case "services", "jobs", "previews":
			return "compute"
		case "databases":
			return "databases"
		case "functions":
			return "functions"
		}
		return ""
	}
	return apiAreas[parts[0]]
}

// ---------- operator ----------

type featureRow struct {
	ID, Name, Group string
	Global          bool            // on for everyone
	Plans           map[string]bool // plan ID -> on
}

func (s *Server) operatorFeaturesPage(w http.ResponseWriter, r *http.Request) {
	var global []string
	if err := s.pool.QueryRow(r.Context(), `SELECT features_off FROM platform_settings WHERE id = 1`).Scan(&global); err != nil {
		s.serverError(w, r, err)
		return
	}
	type plan struct {
		ID, Name string
		Off      []string
	}
	var plans []plan
	rows, err := s.pool.Query(r.Context(), `SELECT id, name, features_off FROM plans ORDER BY price_monthly_cents, id`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for rows.Next() {
		var p plan
		if rows.Scan(&p.ID, &p.Name, &p.Off) == nil {
			plans = append(plans, p)
		}
	}
	rows.Close()
	in := func(list []string, id string) bool {
		for _, x := range list {
			if x == id {
				return true
			}
		}
		return false
	}
	var out []featureRow
	for _, p := range s.switchable() {
		fr := featureRow{ID: p.ID, Name: p.Name, Group: p.Group, Global: !in(global, p.ID), Plans: map[string]bool{}}
		for _, pl := range plans {
			fr.Plans[pl.ID] = !in(pl.Off, p.ID)
		}
		out = append(out, fr)
	}
	s.render(w, r, "operator_features", http.StatusOK, data{"OpSection": "features", "Features": out, "Plans": plans})
}

func (s *Server) operatorSaveFeatures(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		back(w, r, "/operator/features", "", "The form could not be read.")
		return
	}
	var plans []string
	rows, err := s.pool.Query(r.Context(), `SELECT id FROM plans`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			plans = append(plans, id)
		}
	}
	rows.Close()
	globalOff := []string{}
	planOff := map[string][]string{}
	for _, p := range s.switchable() {
		if r.PostForm.Get("on_"+p.ID) != "1" {
			globalOff = append(globalOff, p.ID)
		}
		for _, pl := range plans {
			if r.PostForm.Get("plan_"+pl+"_"+p.ID) != "1" {
				planOff[pl] = append(planOff[pl], p.ID)
			}
		}
	}
	if _, err := s.pool.Exec(r.Context(), `UPDATE platform_settings SET features_off = $1, updated_at = now() WHERE id = 1`, globalOff); err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, pl := range plans {
		off := planOff[pl]
		if off == nil {
			off = []string{}
		}
		if _, err := s.pool.Exec(r.Context(), `UPDATE plans SET features_off = $2 WHERE id = $1`, pl, off); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.dropSettingsCache()
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.features", "platform", map[string]any{"off": globalOff, "plans_off": planOff}, clientIP(r))
	back(w, r, "/operator/features", "Feature switches saved.", "")
}

// operatorOrgFeatures sets the services turned off for one organization.
func (s *Server) operatorOrgFeatures(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := r.ParseForm(); err != nil {
		back(w, r, "/operator/orgs/"+slug, "", "The form could not be read.")
		return
	}
	off := []string{}
	for _, p := range s.switchable() {
		if r.PostForm.Get("on_"+p.ID) != "1" {
			off = append(off, p.ID)
		}
	}
	sort.Strings(off)
	var ok bool
	if err := s.pool.QueryRow(r.Context(), `SELECT operator_set_org_features($1, $2)`, slug, off).Scan(&ok); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r)
		return
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.org_features", "org:"+slug, map[string]any{"off": off}, clientIP(r))
	back(w, r, "/operator/orgs/"+slug+"#features", "Services of "+slug+" saved.", "")
}
