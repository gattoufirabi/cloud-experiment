package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/miekg/dns"

	"platform/internal/db"
	"platform/internal/model"
)

// DNS hosting: zones delegated to the platform's name servers and their records. The
// agent answers for active zones; a zone becomes active when its delegation is seen.

var (
	dnsDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	dnsNameRe   = regexp.MustCompile(`^(\*\.)?([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)*[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?$`)
	dnsHostRe   = regexp.MustCompile(`^([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{1,63}\.?$`)
	dnsTypes    = []string{"A", "AAAA", "CNAME", "TXT", "MX", "CAA", "NS", "SRV"}
)

type dnsZoneView struct {
	ID, Domain, Status string
	Checked, Active    *time.Time
	Records            int
	Created            time.Time
	Delegation         string // what is wrong with the delegation ("" when it is complete)
}

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

func loadDNSZones(ctx context.Context, tx pgx.Tx) ([]dnsZoneView, error) {
	rows, err := tx.Query(ctx, `SELECT z.id, z.domain, z.status, z.checked_at, z.active_at, (SELECT count(*) FROM dns_records r WHERE r.zone_id = z.id)::int, z.created_at, z.delegation
		FROM dns_zones z ORDER BY z.domain`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[dnsZoneView])
}

func loadDNSZone(ctx context.Context, tx pgx.Tx, domain string) (*dnsZoneView, []dnsRecordView, error) {
	var z dnsZoneView
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

func (s *Server) dnsEnabled() bool { return len(s.cfg.DNSNameservers) > 0 }

func (s *Server) addZoneTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, domain string) (string, error) {
	if !s.dnsEnabled() {
		return "", apiErr{http.StatusServiceUnavailable, "DNS hosting is not enabled on this platform"}
	}
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if len(domain) > 253 || !dnsDomainRe.MatchString(domain) {
		return "", apiErr{http.StatusUnprocessableEntity, "enter a domain such as example.com"}
	}
	for _, own := range []string{s.cfg.AppsDomain, hostOnly(s.cfg.ConsoleHost)} { // the platform's own names are not for tenants
		if own != "" && (domain == own || strings.HasSuffix(domain, "."+own) || strings.HasSuffix(own, "."+domain)) {
			return "", apiErr{http.StatusUnprocessableEntity, "this domain belongs to the platform"}
		}
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT dns_zone_taken($1)`, domain).Scan(&taken); err != nil {
		return "", err
	}
	if taken {
		return "", apiErr{http.StatusConflict, "this domain is already hosted here"}
	}
	var n, max int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM dns_zones), max_dns_zones FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return "", err
	}
	if n >= max {
		return "", apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d DNS zones", max)}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dns_zones (org_id, domain) VALUES ($1, $2)`, o.ID, domain); err != nil {
		if isUniqueViolation(err) {
			return "", apiErr{http.StatusConflict, "this domain is already hosted here"}
		}
		return "", err
	}
	s.audit(r, tx, o.ID, "dns.zone_added", "zone:"+domain, nil)
	// the organization's domains in this zone get their records at once
	if err := s.autoZoneTx(ctx, r, tx, o, domain); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'dns')`)
	return domain, err
}

type dnsRecordInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Priority int    `json:"priority"`
	TTL      int    `json:"ttl"`
}

// normalizeRecord checks a record and returns its stored form.
func normalizeRecord(zone string, in dnsRecordInput) (dnsRecordInput, error) {
	bad := func(msg string) (dnsRecordInput, error) { return in, apiErr{http.StatusUnprocessableEntity, msg} }
	in.Name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(in.Name)), ".")
	in.Name = strings.TrimSuffix(in.Name, "."+zone)
	if in.Name == "" || in.Name == zone {
		in.Name = "@"
	}
	if in.Name != "@" && in.Name != "*" && !dnsNameRe.MatchString(in.Name) {
		return bad("name: @ for the domain itself, or a name such as www or api.v2")
	}
	in.Type = strings.ToUpper(strings.TrimSpace(in.Type))
	in.Value = strings.TrimSpace(in.Value)
	if in.TTL == 0 {
		in.TTL = 300
	}
	if in.TTL < 60 || in.TTL > 86400 {
		return bad("TTL: 60 to 86400 seconds")
	}
	switch in.Type {
	case "A":
		if ip := net.ParseIP(in.Value); ip == nil || ip.To4() == nil {
			return bad("A: an IPv4 address such as 203.0.113.10")
		}
	case "AAAA":
		if ip := net.ParseIP(in.Value); ip == nil || ip.To4() != nil {
			return bad("AAAA: an IPv6 address such as 2001:db8::1")
		}
	case "CNAME", "NS", "MX":
		in.Value = strings.TrimSuffix(strings.ToLower(in.Value), ".")
		if !dnsHostRe.MatchString(in.Value + ".") {
			return bad(in.Type + ": a host name such as target.example.com")
		}
		if in.Type == "CNAME" && in.Name == "@" {
			return bad("CNAME: not allowed for the domain itself (@); use an A record, or the service shortcut")
		}
		if in.Type == "NS" && in.Name == "@" {
			return bad("NS: the domain's own name servers are the platform's; NS records delegate a sub-domain")
		}
		if in.Type == "MX" && (in.Priority < 0 || in.Priority > 65535) {
			return bad("MX: a priority from 0 to 65535")
		}
	case "TXT":
		if len(in.Value) > 1000 {
			return bad("TXT: at most 1000 characters")
		}
		in.Value = strings.Trim(in.Value, `"`)
	case "CAA":
		if _, err := dns.NewRR("x.test. 300 IN CAA " + in.Value); err != nil {
			return bad(`CAA: such as 0 issue "letsencrypt.org"`)
		}
	case "SRV": // value "weight port target"; the priority is the SRV priority
		f := strings.Fields(in.Value)
		if len(f) != 3 || in.Priority < 0 || in.Priority > 65535 {
			return bad("SRV: a value such as \"0 443 target.example.com\" (weight, port, target) and a priority from 0 to 65535")
		}
		w, werr := strconv.Atoi(f[0])
		port, perr := strconv.Atoi(f[1])
		target := strings.TrimSuffix(strings.ToLower(f[2]), ".")
		if werr != nil || perr != nil || w < 0 || w > 65535 || port < 0 || port > 65535 || (target != "" && !dnsHostRe.MatchString(target+".")) {
			return bad("SRV: a value such as \"0 443 target.example.com\" (weight, port, target)")
		}
		in.Value = fmt.Sprintf("%d %d %s", w, port, target)
	default:
		return bad("type: one of " + strings.Join(dnsTypes, ", "))
	}
	return in, nil
}

func (s *Server) addRecordTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, z *dnsZoneView, in dnsRecordInput) error {
	_, err := s.addRecordIDTx(ctx, r, tx, o, z, in)
	return err
}

// addRecordIDTx adds a record and returns its ID.
func (s *Server) addRecordIDTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, z *dnsZoneView, in dnsRecordInput) (string, error) {
	in, err := normalizeRecord(z.Domain, in)
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
		return "", apiErr{http.StatusUnprocessableEntity, "a CNAME cannot share its name with other records"}
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM dns_records WHERE zone_id = $1`, z.ID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 500 {
		return "", apiErr{http.StatusUnprocessableEntity, "a zone holds at most 500 records"}
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO dns_records (org_id, zone_id, name, type, value, priority, ttl) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
		o.ID, z.ID, in.Name, in.Type, in.Value, in.Priority, in.TTL).Scan(&id); err != nil {
		return "", err
	}
	return id, s.zoneChanged(ctx, r, tx, o, z, "dns.record_added", map[string]any{"name": in.Name, "type": in.Type})
}

func (s *Server) zoneChanged(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, z *dnsZoneView, action string, detail any) error {
	if _, err := tx.Exec(ctx, `UPDATE dns_zones SET serial = greatest(serial + 1, extract(epoch FROM now())::bigint) WHERE id = $1`, z.ID); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, action, "zone:"+z.Domain, detail)
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'dns')`)
	return err
}

// ---------- console ----------

func (s *Server) dnsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	var zones []dnsZoneView
	if err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		zones, err = loadDNSZones(r.Context(), tx)
		return err
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "dns", http.StatusOK, data{"Org": o, "Zones": zones, "Nameservers": s.cfg.DNSNameservers, "Enabled": s.dnsEnabled(),
		"CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *Server) addZone(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	var domain string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		domain, err = s.addZoneTx(r.Context(), r, tx, o, r.PostFormValue("domain"))
		return err
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/dns#zones", "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/dns/"+domain, "Zone added. Set its name servers at your registrar to the ones below.", "")
	}
}

func (s *Server) withZone(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *orgInfo, z *dnsZoneView, recs []dnsRecordView) error) (*orgInfo, *dnsZoneView, []dnsRecordView, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, nil, false
	}
	var z *dnsZoneView
	var recs []dnsRecordView
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if z, recs, err = loadDNSZone(r.Context(), tx, r.PathValue("zone")); err != nil {
			return err
		}
		return fn(tx, o, z, recs)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, nil, false
	}
	return o, z, recs, err, true
}

type dnsServiceOption struct{ Label, Host string }

func (s *Server) zonePage(w http.ResponseWriter, r *http.Request) {
	var svcs []dnsServiceOption
	o, z, recs, err, ok := s.withZone(w, r, "viewer", func(tx pgx.Tx, o *orgInfo, z *dnsZoneView, recs []dnsRecordView) error {
		rows, err := tx.Query(r.Context(), `SELECT p.slug, s.slug FROM services s JOIN projects p ON p.id = s.project_id WHERE s.preview_of IS NULL AND NOT s.worker ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ps, ss string
			if rows.Scan(&ps, &ss) == nil {
				svcs = append(svcs, dnsServiceOption{ps + "/" + ss, model.DefaultHost(ss, ps, o.Slug, s.cfg.AppsDomain)})
			}
		}
		return rows.Err()
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "dns_zone", http.StatusOK, data{"Org": o, "Zone": z, "Records": recs, "Nameservers": s.cfg.DNSNameservers, "Types": dnsTypes,
		"Services": svcs, "PublicIP": s.cfg.PublicIP, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *Server) zoneAction(w http.ResponseWriter, r *http.Request) {
	action := r.PostFormValue("action")
	var msg string
	o, z, _, err, ok := s.withZone(w, r, "developer", func(tx pgx.Tx, o *orgInfo, z *dnsZoneView, recs []dnsRecordView) error {
		switch action {
		case "add":
			msg = "Record added."
			return s.addRecordTx(r.Context(), r, tx, o, z, dnsRecordInput{Name: r.PostFormValue("name"), Type: r.PostFormValue("type"), Value: r.PostFormValue("value"),
				Priority: atoi(r.PostFormValue("priority")), TTL: atoiDefault(r.PostFormValue("ttl"), 300)})
		case "service": // point a name at one of the organization's services
			name, host := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("host")
			if !strings.HasSuffix(host, "."+s.cfg.AppsDomain) {
				return apiErr{http.StatusUnprocessableEntity, "Choose a service."}
			}
			in := dnsRecordInput{Name: name, Type: "CNAME", Value: host}
			if name == "" || name == "@" {
				if s.cfg.PublicIP == "" {
					return apiErr{http.StatusUnprocessableEntity, "The domain itself can only point to a service with an A record, and the platform's address is not configured: use a name such as www."}
				}
				in = dnsRecordInput{Name: "@", Type: "A", Value: s.cfg.PublicIP}
			}
			msg = "Record added. Now add the domain to the service (Domains tab) so it is served with HTTPS."
			return s.addRecordTx(r.Context(), r, tx, o, z, in)
		case "delete":
			tag, err := tx.Exec(r.Context(), `DELETE FROM dns_records WHERE id::text = $1 AND zone_id = $2`, r.PostFormValue("record"), z.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return apiErr{http.StatusNotFound, "No such record."}
			}
			msg = "Record deleted."
			return s.zoneChanged(r.Context(), r, tx, o, z, "dns.record_deleted", nil)
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
				return apiErr{http.StatusUnprocessableEntity, "Type the domain to delete the zone."}
			}
			if _, err := tx.Exec(r.Context(), `DELETE FROM dns_zones WHERE id = $1`, z.ID); err != nil {
				return err
			}
			s.audit(r, tx, o.ID, "dns.zone_deleted", "zone:"+z.Domain, nil)
			_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'dns')`)
			return err
		}
		return apiErr{http.StatusBadRequest, "unknown action"}
	})
	if !ok {
		return
	}
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/dns/"+z.Domain+"#records", "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	case action == "remove":
		back(w, r, o.Path()+"/dns", "Zone "+z.Domain+" deleted: the platform no longer answers for it.", "")
	default:
		back(w, r, o.Path()+"/dns/"+z.Domain+"#records", msg, "")
	}
}

// ---------- REST API ----------

func (s *Server) apiDNSZones(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var zones []dnsZoneView
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		zones, err = loadDNSZones(r.Context(), tx)
		return err
	})
	if apiTx(w, s, err) {
		if zones == nil {
			zones = []dnsZoneView{}
		}
		writeJSON(w, map[string]any{"zones": zones, "nameservers": s.cfg.DNSNameservers})
	}
}

func (s *Server) apiAddDNSZone(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Domain string `json:"domain"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"domain": "example.com"}`)
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		_, err := s.addZoneTx(r.Context(), r, tx, o, body.Domain)
		return err
	})
	if apiTx(w, s, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"nameservers": s.cfg.DNSNameservers, "status": "pending"})
	}
}

func (s *Server) apiDNSRecords(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []dnsRecordView
	var zone *dnsZoneView
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		zone, out, err = loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		return err
	})
	if apiTx(w, s, err) {
		if out == nil {
			out = []dnsRecordView{}
		}
		writeJSON(w, map[string]any{"zone": zone.Domain, "status": zone.Status, "records": out})
	}
}

func (s *Server) apiAddDNSRecord(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var in dnsRecordInput
	var out dnsRecordView
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in) != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"name": "www", "type": "CNAME", "value": "target.example.com"}`)
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
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
	if apiTx(w, s, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	}
}

func (s *Server) apiDeleteDNSRecord(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
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
		return s.zoneChanged(r.Context(), r, tx, o, z, "dns.record_deleted", nil)
	})
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiDeleteDNSZone deletes a hosted zone with its records: DELETE /api/v1/dns/zones/{zone}
func (s *Server) apiDeleteDNSZone(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		z, _, err := loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM dns_zones WHERE id = $1`, z.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "dns.zone_deleted", "zone:"+z.Domain, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'dns')`)
		return err
	})
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}
