package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Automatic DNS for domains in a zone the organization hosts on the platform: the
// records a service's custom domain or an email sending domain needs are written by the
// platform (tagged with managed_by), the domain is verified at once when the zone is
// active (the platform answers for it), and the records go away with the domain. A
// record a person wrote at the same name is never replaced: the automatic one is skipped.

// zoneFor returns the organization's hosted zone holding host (the longest match) and
// host's name inside it ("@" for the zone itself), or nil.
func zoneFor(ctx context.Context, tx pgx.Tx, host string) (*dnsZoneView, string, error) {
	var z dnsZoneView
	err := tx.QueryRow(ctx, `
		SELECT z.id, z.domain, z.status, z.checked_at, z.active_at, 0, z.created_at FROM dns_zones z
		WHERE $1 = z.domain OR $1 LIKE '%.' || z.domain ORDER BY length(z.domain) DESC LIMIT 1`, host).
		Scan(&z.ID, &z.Domain, &z.Status, &z.Checked, &z.Active, &z.Records, &z.Created)
	if isNoRows(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	rel := "@"
	if host != z.Domain {
		rel = strings.TrimSuffix(host, "."+z.Domain)
	}
	return &z, rel, nil
}

// under prefixes a label to a relative name ("_dmarc" under "@" is "_dmarc").
func under(label, rel string) string {
	if rel == "@" {
		return label
	}
	return label + "." + rel
}

// setManagedTx replaces the records tagged tag with recs; it returns the records it left
// out because a person's record already uses that name (or, for SPF and DMARC, already
// says what they say).
func (s *Server) setManagedTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, z *dnsZoneView, tag string, recs []dnsRecordInput) ([]string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM dns_records WHERE zone_id = $1 AND managed_by = $2`, z.ID, tag); err != nil {
		return nil, err
	}
	var skipped []string
	for _, in := range recs {
		in, err := normalizeRecord(z.Domain, in)
		if err != nil {
			skipped = append(skipped, in.Name+" "+in.Type)
			continue
		}
		var conflict bool
		switch {
		case in.Type == "TXT" && (strings.HasPrefix(in.Value, "v=spf1") || strings.HasPrefix(in.Value, "v=DMARC1")):
			// one SPF and one DMARC policy per name: keep the person's own
			prefix := in.Value[:strings.Index(in.Value, " ")]
			if strings.HasPrefix(in.Value, "v=DMARC1") {
				prefix = "v=DMARC1"
			}
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_records WHERE zone_id = $1 AND name = $2 AND type = 'TXT' AND value LIKE $3 || '%')`,
				z.ID, in.Name, strings.TrimSuffix(prefix, ";")).Scan(&conflict)
		case in.Type == "CNAME":
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_records WHERE zone_id = $1 AND name = $2)`, z.ID, in.Name).Scan(&conflict)
		default:
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_records WHERE zone_id = $1 AND name = $2 AND (type = 'CNAME' OR (type = $3 AND $3 IN ('A', 'AAAA'))))`,
				z.ID, in.Name, in.Type).Scan(&conflict)
		}
		if err != nil {
			return nil, err
		}
		if conflict {
			skipped = append(skipped, in.Name+" "+in.Type)
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dns_records (org_id, zone_id, name, type, value, priority, ttl, managed_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			o.ID, z.ID, in.Name, in.Type, in.Value, in.Priority, in.TTL, tag); err != nil {
			return nil, err
		}
	}
	return skipped, s.zoneChanged(ctx, r, tx, o, z, "dns.records_automatic", map[string]any{"for": tag, "records": len(recs) - len(skipped), "skipped": skipped})
}

// removeManagedTx deletes the records of a domain that is removed.
func (s *Server) removeManagedTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, tag string) error {
	rows, err := tx.Query(ctx, `DELETE FROM dns_records WHERE managed_by = $1 RETURNING zone_id::text`, tag)
	if err != nil {
		return err
	}
	zones := map[string]bool{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			zones[id] = true
		}
	}
	rows.Close()
	for id := range zones {
		var z dnsZoneView
		if err := tx.QueryRow(ctx, `SELECT id, domain FROM dns_zones WHERE id = $1`, id).Scan(&z.ID, &z.Domain); err == nil {
			if err := s.zoneChanged(ctx, r, tx, o, &z, "dns.records_removed", map[string]any{"for": tag}); err != nil {
				return err
			}
		}
	}
	return nil
}

// autoServiceDomainTx writes a custom domain's records when its zone is hosted here: the
// name points at the service (a CNAME, or an A record at the zone's apex) and the
// verification TXT; the domain is verified at once if the zone is active. It reports
// whether the platform took care of it.
func (s *Server) autoServiceDomainTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, domainID, host, token, target string) (bool, error) {
	z, rel, err := zoneFor(ctx, tx, host)
	if err != nil || z == nil {
		return false, err
	}
	recs := []dnsRecordInput{{Name: under("_platform-challenge", rel), Type: "TXT", Value: "platform-verify=" + token}}
	switch {
	case rel != "@":
		recs = append(recs, dnsRecordInput{Name: rel, Type: "CNAME", Value: target})
	case s.cfg.PublicIP != "":
		recs = append(recs, dnsRecordInput{Name: "@", Type: "A", Value: s.cfg.PublicIP})
	}
	if _, err := s.setManagedTx(ctx, r, tx, o, z, "domain:"+domainID, recs); err != nil {
		return false, err
	}
	if z.Status == "active" {
		if _, err := tx.Exec(ctx, `UPDATE domains SET verified_at = coalesce(verified_at, now()) WHERE id = $1`, domainID); err != nil {
			return false, err
		}
		s.audit(r, tx, o.ID, "domain.verified", "domain:"+host, map[string]string{"by": "hosted zone " + z.Domain})
		_, err = tx.Exec(ctx, `SELECT pg_notify('deployments', '')`)
	}
	return true, err
}

// autoEmailDomainTx writes an email sending domain's records (ownership, DKIM, SPF and
// DMARC unless the domain has its own) when its zone is hosted here.
func (s *Server) autoEmailDomainTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, d *sendDomain) (bool, error) {
	z, rel, err := zoneFor(ctx, tx, d.Domain)
	if err != nil || z == nil {
		return false, err
	}
	recs := []dnsRecordInput{
		{Name: under("_platform-verify", rel), Type: "TXT", Value: "platform-verify=" + d.Token},
		{Name: under(d.Selector+"._domainkey", rel), Type: "TXT", Value: d.TXT},
		{Name: under("_dmarc", rel), Type: "TXT", Value: "v=DMARC1; p=none"},
	}
	if spf := s.spfMechanism(); spf != "" {
		recs = append(recs, dnsRecordInput{Name: rel, Type: "TXT", Value: "v=spf1 " + spf + " ~all"})
	}
	if _, err := s.setManagedTx(ctx, r, tx, o, z, "email:"+d.ID, recs); err != nil {
		return false, err
	}
	if z.Status == "active" {
		if _, err := tx.Exec(ctx, `UPDATE email_domains SET status = 'verified', message = '', checked_at = now(), verified_at = coalesce(verified_at, now()) WHERE id = $1`, d.ID); err != nil {
			return false, err
		}
		s.audit(r, tx, o.ID, "email.domain_verified", "email-domain:"+d.Domain, map[string]string{"by": "hosted zone " + z.Domain})
	}
	return true, nil
}

// autoZoneTx gives a new zone the records of the organization's domains it holds.
func (s *Server) autoZoneTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, zone string) error {
	type dom struct{ ID, Host, Token, Target string }
	var doms []dom
	rows, err := tx.Query(ctx, `
		SELECT dm.id::text, dm.hostname, dm.verify_token, s.slug || '-' || p.slug
		FROM domains dm JOIN services s ON s.id = dm.service_id JOIN projects p ON p.id = s.project_id
		WHERE dm.hostname = $1 OR dm.hostname LIKE '%.' || $1`, zone)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d dom
		if rows.Scan(&d.ID, &d.Host, &d.Token, &d.Target) == nil {
			d.Target += "-" + o.Slug + "." + s.cfg.AppsDomain
			doms = append(doms, d)
		}
	}
	rows.Close()
	for _, d := range doms {
		if _, err := s.autoServiceDomainTx(ctx, r, tx, o, d.ID, d.Host, d.Token, d.Target); err != nil {
			return err
		}
	}
	eds, err := loadSendDomains(ctx, tx)
	if err != nil {
		return err
	}
	for _, d := range eds {
		if d.Domain == zone || strings.HasSuffix(d.Domain, "."+zone) {
			if _, err := s.autoEmailDomainTx(ctx, r, tx, o, d); err != nil {
				return err
			}
		}
	}
	return nil
}
