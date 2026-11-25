package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
)

type dnsRecordView struct {
	ID, Name, Type, Value string
	Priority, TTL         int
	ManagedBy             string // "" for records people added; "domain:<id>" or "email:<id>"
}

// Automatic is the kind of resource an automatic record belongs to, for the page.
func (r dnsRecordView) Automatic() string {
	switch {
	case strings.HasPrefix(r.ManagedBy, "domain:"):
		return "custom domain"
	case strings.HasPrefix(r.ManagedBy, "email:"):
		return "email"
	}
	return ""
}

func loadDNSZones(ctx context.Context, tx pgx.Tx) ([]web.DNSZoneView, error) {
	rows, err := tx.Query(ctx, `SELECT z.id, z.domain, z.status, z.checked_at, z.active_at, (SELECT count(*) FROM dns_records r WHERE r.zone_id = z.id)::int, z.created_at, z.delegation
		FROM dns_zones z ORDER BY z.domain`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[web.DNSZoneView])
}

func loadDNSZone(ctx context.Context, tx pgx.Tx, domain string) (*web.DNSZoneView, []dnsRecordView, error) {
	var z web.DNSZoneView
	if err := tx.QueryRow(ctx, `SELECT z.id, z.domain, z.status, z.checked_at, z.active_at, (SELECT count(*) FROM dns_records r WHERE r.zone_id = z.id)::int, z.created_at, z.delegation
		FROM dns_zones z WHERE z.domain = $1`, domain).Scan(&z.ID, &z.Domain, &z.Status, &z.Checked, &z.Active, &z.Records, &z.Created, &z.Delegation); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id, name, type, value, priority, ttl, managed_by FROM dns_records WHERE zone_id = $1
		ORDER BY CASE WHEN name = '@' THEN '' ELSE name END, type, priority, value`, z.ID)
	if err != nil {
		return nil, nil, err
	}
	recs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dnsRecordView])
	return &z, recs, err
}

func (s *API) addZoneTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, domain string) (string, error) {
	if !s.DNSEnabled() {
		return "", web.APIErr{Status: http.StatusServiceUnavailable, Msg: "DNS hosting is not enabled on this platform"}
	}
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if len(domain) > 253 || !web.DNSDomainRe.MatchString(domain) {
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "enter a domain such as example.com"}
	}
	for _, own := range []string{s.Cfg.AppsDomain, web.HostOnly(s.Cfg.ConsoleHost)} { // the platform's own names are not for tenants
		if own != "" && (domain == own || strings.HasSuffix(domain, "."+own) || strings.HasSuffix(own, "."+domain)) {
			return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "this domain belongs to the platform"}
		}
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT dns_zone_taken($1)`, domain).Scan(&taken); err != nil {
		return "", err
	}
	if taken {
		return "", web.APIErr{Status: http.StatusConflict, Msg: "this domain is already hosted here"}
	}
	var n, max int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM dns_zones), max_dns_zones FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return "", err
	}
	if n >= max {
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: fmt.Sprintf("your plan allows %d DNS zones", max)}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dns_zones (org_id, domain) VALUES ($1, $2)`, o.ID, domain); err != nil {
		if web.IsUniqueViolation(err) {
			return "", web.APIErr{Status: http.StatusConflict, Msg: "this domain is already hosted here"}
		}
		return "", err
	}
	s.Audit(r, tx, o.ID, "dns.zone_added", "zone:"+domain, nil)
	// the organization's domains in this zone get their records at once
	if err := s.autoZoneTx(ctx, r, tx, o, domain); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'dns')`)
	return domain, err
}

func (s *API) addRecordTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, z *web.DNSZoneView, in web.DNSRecordInput) error {
	_, err := s.addRecordIDTx(ctx, r, tx, o, z, in)
	return err
}

// addRecordIDTx adds a record and returns its ID.
func (s *API) addRecordIDTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, z *web.DNSZoneView, in web.DNSRecordInput) (string, error) {
	in, err := web.NormalizeRecord(z.Domain, in)
	if err != nil {
		return "", err
	}
	// A CNAME stands alone at its name.
	var others, cnames int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type <> 'CNAME'), count(*) FILTER (WHERE type = 'CNAME') FROM dns_records WHERE zone_id = $1 AND name = $2`,
		z.ID, in.Name).Scan(&others, &cnames); err != nil {
		return "", err
	}
	if in.Type == "CNAME" && others+cnames > 0 || in.Type != "CNAME" && cnames > 0 {
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a CNAME cannot share its name with other records"}
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM dns_records WHERE zone_id = $1`, z.ID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 500 {
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a zone holds at most 500 records"}
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO dns_records (org_id, zone_id, name, type, value, priority, ttl) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
		o.ID, z.ID, in.Name, in.Type, in.Value, in.Priority, in.TTL).Scan(&id); err != nil {
		return "", err
	}
	return id, s.ZoneChanged(ctx, r, tx, o, z, "dns.record_added", map[string]any{"name": in.Name, "type": in.Type})
}

// ---------- console ----------

func (s *API) dnsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	var zones []web.DNSZoneView
	if err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		zones, err = loadDNSZones(r.Context(), tx)
		return err
	}); err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "dns", http.StatusOK, web.Data{"Org": o, "Zones": zones, "Nameservers": s.Cfg.DNSNameservers, "Enabled": s.DNSEnabled(),
		"CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *API) addZone(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	var domain string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		domain, err = s.addZoneTx(r.Context(), r, tx, o, r.PostFormValue("domain"))
		return err
	})
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/dns#zones", "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, o.Path()+"/dns/"+domain, "Zone added. Set its name servers at your registrar to the ones below.", "")
	}
}

func (s *API) withZone(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *web.OrgInfo, z *web.DNSZoneView, recs []dnsRecordView) error) (*web.OrgInfo, *web.DNSZoneView, []dnsRecordView, error, bool) {
	o, ok := s.Org(w, r, minRole)
	if !ok {
		return nil, nil, nil, nil, false
	}
	var z *web.DNSZoneView
	var recs []dnsRecordView
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if z, recs, err = loadDNSZone(r.Context(), tx, r.PathValue("zone")); err != nil {
			return err
		}
		return fn(tx, o, z, recs)
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return nil, nil, nil, nil, false
	}
	return o, z, recs, err, true
}

type dnsServiceOption struct{ Label, Host string }

func (s *API) zonePage(w http.ResponseWriter, r *http.Request) {
	var svcs []dnsServiceOption
	o, z, recs, err, ok := s.withZone(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, z *web.DNSZoneView, recs []dnsRecordView) error {
		rows, err := tx.Query(r.Context(), `SELECT p.slug, s.slug FROM services s JOIN projects p ON p.id = s.project_id WHERE s.preview_of IS NULL AND NOT s.worker ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ps, ss string
			if rows.Scan(&ps, &ss) == nil {
				svcs = append(svcs, dnsServiceOption{ps + "/" + ss, model.DefaultHost(ss, ps, o.Slug, s.Cfg.AppsDomain)})
			}
		}
		return rows.Err()
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "dns_zone", http.StatusOK, web.Data{"Org": o, "Zone": z, "Records": recs, "Nameservers": s.Cfg.DNSNameservers, "Types": web.DNSTypes,
		"Services": svcs, "PublicIP": s.Cfg.PublicIP, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *API) zoneAction(w http.ResponseWriter, r *http.Request) {
	action := r.PostFormValue("action")
	var msg string
	o, z, _, err, ok := s.withZone(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, z *web.DNSZoneView, recs []dnsRecordView) error {
		switch action {
		case "add":
			msg = "Record added."
			return s.addRecordTx(r.Context(), r, tx, o, z, web.DNSRecordInput{Name: r.PostFormValue("name"), Type: r.PostFormValue("type"), Value: r.PostFormValue("value"),
				Priority: web.Atoi(r.PostFormValue("priority")), TTL: web.AtoiDefault(r.PostFormValue("ttl"), 300)})
		case "service": // point a name at one of the organization's services
			name, host := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("host")
			if !strings.HasSuffix(host, "."+s.Cfg.AppsDomain) {
				return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "Choose a service."}
			}
			in := web.DNSRecordInput{Name: name, Type: "CNAME", Value: host}
			if name == "" || name == "@" {
				if s.Cfg.PublicIP == "" {
					return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "The domain itself can only point to a service with an A record, and the platform's address is not configured: use a name such as www."}
				}
				in = web.DNSRecordInput{Name: "@", Type: "A", Value: s.Cfg.PublicIP}
			}
			msg = "Record added. Now add the domain to the service (Domains tab) so it is served with HTTPS."
			return s.addRecordTx(r.Context(), r, tx, o, z, in)
		case "delete":
			tag, err := tx.Exec(r.Context(), `DELETE FROM dns_records WHERE id::text = $1 AND zone_id = $2`, r.PostFormValue("record"), z.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return web.APIErr{Status: http.StatusNotFound, Msg: "No such record."}
			}
			msg = "Record deleted."
			return s.ZoneChanged(r.Context(), r, tx, o, z, "dns.record_deleted", nil)
		case "import":
			added, skipped, err := s.importZoneTx(r.Context(), r, tx, o, z, r.PostFormValue("zone_file"))
			if err != nil {
				return err
			}
			msg = fmt.Sprintf("%d record(s) imported.", added)
			if len(skipped) > 0 {
				msg += " Left out: " + strings.Join(skipped, "; ") + "."
			}
			return nil
		case "check":
			msg = "Checking the delegation now; reload in a minute."
			if _, err := tx.Exec(r.Context(), `UPDATE dns_zones SET checked_at = NULL WHERE id = $1`, z.ID); err != nil {
				return err
			}
			_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'dns')`)
			return err
		case "remove":
			if r.PostFormValue("confirm") != z.Domain {
				return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "Type the domain to delete the zone."}
			}
			if _, err := tx.Exec(r.Context(), `DELETE FROM dns_zones WHERE id = $1`, z.ID); err != nil {
				return err
			}
			s.Audit(r, tx, o.ID, "dns.zone_deleted", "zone:"+z.Domain, nil)
			_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'dns')`)
			return err
		}
		return web.APIErr{Status: http.StatusBadRequest, Msg: "unknown action"}
	})
	if !ok {
		return
	}
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/dns/"+z.Domain+"#records", "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	case action == "remove":
		web.Back(w, r, o.Path()+"/dns", "Zone "+z.Domain+" deleted: the platform no longer answers for it.", "")
	default:
		web.Back(w, r, o.Path()+"/dns/"+z.Domain+"#records", msg, "")
	}
}

// ---------- REST API ----------

func (s *API) apiDNSZones(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var zones []web.DNSZoneView
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		zones, err = loadDNSZones(r.Context(), tx)
		return err
	})
	if web.APITx(w, s.Server, err) {
		if zones == nil {
			zones = []web.DNSZoneView{}
		}
		web.WriteJSON(w, map[string]any{"zones": zones, "nameservers": s.Cfg.DNSNameservers})
	}
}

func (s *API) apiAddDNSZone(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var body struct {
		Domain string `json:"domain"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"domain": "example.com"}`)
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		_, err := s.addZoneTx(r.Context(), r, tx, o, body.Domain)
		return err
	})
	if web.APITx(w, s.Server, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"nameservers": s.Cfg.DNSNameservers, "status": "pending"})
	}
}

func (s *API) apiDNSRecords(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []dnsRecordView
	var zone *web.DNSZoneView
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		zone, out, err = loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		return err
	})
	if web.APITx(w, s.Server, err) {
		if out == nil {
			out = []dnsRecordView{}
		}
		web.WriteJSON(w, map[string]any{"zone": zone.Domain, "status": zone.Status, "records": out})
	}
}

func (s *API) apiAddDNSRecord(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in web.DNSRecordInput
	var out dnsRecordView
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in) != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"name": "www", "type": "CNAME", "value": "target.example.com"}`)
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		z, _, err := loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		if err != nil {
			return err
		}
		id, err := s.addRecordIDTx(r.Context(), r, tx, o, z, in)
		if err != nil {
			return err
		}
		_, recs, err := loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		for _, rec := range recs {
			if rec.ID == id {
				out = rec
			}
		}
		return err
	})
	if web.APITx(w, s.Server, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	}
}

func (s *API) apiDeleteDNSRecord(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		z, _, err := loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		if err != nil {
			return err
		}
		tag, err := tx.Exec(r.Context(), `DELETE FROM dns_records WHERE id::text = $1 AND zone_id = $2`, r.PathValue("id"), z.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return s.ZoneChanged(r.Context(), r, tx, o, z, "dns.record_deleted", nil)
	})
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiDeleteDNSZone deletes a hosted zone with its records: DELETE /api/v1/dns/zones/{zone}
func (s *API) apiDeleteDNSZone(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		z, _, err := loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM dns_zones WHERE id = $1`, z.ID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "dns.zone_deleted", "zone:"+z.Domain, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'dns')`)
		return err
	})
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}
