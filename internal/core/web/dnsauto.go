package web

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
func ZoneFor(ctx context.Context, tx pgx.Tx, host string) (*DNSZoneView, string, error) {
	var z DNSZoneView
	err := tx.QueryRow(ctx, `
		SELECT z.id, z.domain, z.status, z.checked_at, z.active_at, 0, z.created_at, z.delegation FROM dns_zones z
		WHERE $1 = z.domain OR $1 LIKE '%.' || z.domain ORDER BY length(z.domain) DESC LIMIT 1`, host).
		Scan(&z.ID, &z.Domain, &z.Status, &z.Checked, &z.Active, &z.Records, &z.Created, &z.Delegation)
	if IsNoRows(err) {
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
func Under(label, rel string) string {
	if rel == "@" {
		return label
	}
	return label + "." + rel
}

// setManagedTx replaces the records tagged tag with recs; it returns the records it left
// out because a person's record already uses that name (or, for SPF and DMARC, already
// says what they say).
func (s *Server) SetManagedTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *OrgInfo, z *DNSZoneView, tag string, recs []DNSRecordInput) ([]string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM dns_records WHERE zone_id = $1 AND managed_by = $2`, z.ID, tag); err != nil {
		return nil, err
	}
	var skipped []string
	for _, in := range recs {
		in, err := NormalizeRecord(z.Domain, in)
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
		case in.Type == "MX": // mail goes to one place: MX records of yours (elsewhere) are kept
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_records WHERE zone_id = $1 AND name = $2 AND type IN ('MX', 'CNAME'))`, z.ID, in.Name).Scan(&conflict)
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
	return skipped, s.ZoneChanged(ctx, r, tx, o, z, "dns.records_automatic", map[string]any{"for": tag, "records": len(recs) - len(skipped), "skipped": skipped})
}

// removeManagedTx deletes the records of a domain that is removed.
func (s *Server) RemoveManagedTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *OrgInfo, tag string) error {
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
		var z DNSZoneView
		if err := tx.QueryRow(ctx, `SELECT id, domain FROM dns_zones WHERE id = $1`, id).Scan(&z.ID, &z.Domain); err == nil {
			if err := s.ZoneChanged(ctx, r, tx, o, &z, "dns.records_removed", map[string]any{"for": tag}); err != nil {
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
func (s *Server) AutoServiceDomainTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *OrgInfo, domainID, host, token, target string) (bool, error) {
	z, rel, err := ZoneFor(ctx, tx, host)
	if err != nil || z == nil {
		return false, err
	}
	recs := []DNSRecordInput{{Name: Under("_platform-challenge", rel), Type: "TXT", Value: "platform-verify=" + token}}
	switch {
	case rel != "@":
		recs = append(recs, DNSRecordInput{Name: rel, Type: "CNAME", Value: target})
	case s.Cfg.PublicIP != "":
		recs = append(recs, DNSRecordInput{Name: "@", Type: "A", Value: s.Cfg.PublicIP})
	}
	if _, err := s.SetManagedTx(ctx, r, tx, o, z, "domain:"+domainID, recs); err != nil {
		return false, err
	}
	if z.Status == "active" {
		if _, err := tx.Exec(ctx, `UPDATE domains SET verified_at = coalesce(verified_at, now()) WHERE id = $1`, domainID); err != nil {
			return false, err
		}
		s.Audit(r, tx, o.ID, "domain.verified", "domain:"+host, map[string]string{"by": "hosted zone " + z.Domain})
		_, err = tx.Exec(ctx, `SELECT pg_notify('deployments', '')`)
	}
	return true, err
}

// autoEmailDomainTx writes an email sending domain's records (ownership, DKIM, SPF and
// DMARC unless the domain has its own) when its zone is hosted here.
func (s *Server) AutoEmailDomainTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *OrgInfo, d *SendDomain) (bool, error) {
	z, rel, err := ZoneFor(ctx, tx, d.Domain)
	if err != nil || z == nil {
		return false, err
	}
	recs := []DNSRecordInput{
		{Name: Under("_platform-verify", rel), Type: "TXT", Value: "platform-verify=" + d.Token},
		{Name: Under(d.Selector+"._domainkey", rel), Type: "TXT", Value: d.TXT},
		{Name: Under("_dmarc", rel), Type: "TXT", Value: "v=DMARC1; p=none"},
	}
	if spf := s.SpfMechanism(); spf != "" {
		recs = append(recs, DNSRecordInput{Name: rel, Type: "TXT", Value: "v=spf1 " + spf + " ~all"})
	}
	if _, err := s.SetManagedTx(ctx, r, tx, o, z, "email:"+d.ID, recs); err != nil {
		return false, err
	}
	if z.Status == "active" {
		if _, err := tx.Exec(ctx, `UPDATE email_domains SET status = 'verified', message = '', checked_at = now(), verified_at = coalesce(verified_at, now()) WHERE id = $1`, d.ID); err != nil {
			return false, err
		}
		s.Audit(r, tx, o.ID, "email.domain_verified", "email-domain:"+d.Domain, map[string]string{"by": "hosted zone " + z.Domain})
	}
	return true, nil
}
