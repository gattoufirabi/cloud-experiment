package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/miekg/dns"

	"platform/internal/db"
)

// Importing a zone file (the BIND format registrars and DNS providers export): before
// moving a domain's name servers to the platform, its existing records (websites, mail,
// verification records) are copied into the hosted zone, so nothing stops answering.
// The zone's SOA and its own NS records are the platform's and are left out.

const maxZoneFile = 256 << 10

// parseZoneFile turns a zone file into records of zone; skipped explains what is left out.
func parseZoneFile(zone, text string) (recs []dnsRecordInput, skipped []string, err error) {
	if len(text) > maxZoneFile {
		return nil, nil, apiErr{http.StatusRequestEntityTooLarge, "the zone file is larger than 256 KB"}
	}
	zp := dns.NewZoneParser(strings.NewReader(text), dns.Fqdn(zone), "")
	zp.SetDefaultTTL(3600)
	zp.SetIncludeAllowed(false)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		h := rr.Header()
		name := strings.TrimSuffix(strings.ToLower(h.Name), ".")
		if name != zone && !strings.HasSuffix(name, "."+zone) {
			skipped = append(skipped, name+" (outside "+zone+")")
			continue
		}
		rel := "@"
		if name != zone {
			rel = strings.TrimSuffix(name, "."+zone)
		}
		ttl := int(h.Ttl)
		ttl = max(60, min(ttl, 86400))
		in := dnsRecordInput{Name: rel, TTL: ttl}
		switch v := rr.(type) {
		case *dns.SOA:
			continue // the platform's own
		case *dns.NS:
			if rel == "@" {
				continue // the domain's name servers are the platform's
			}
			in.Type, in.Value = "NS", v.Ns
		case *dns.A:
			in.Type, in.Value = "A", v.A.String()
		case *dns.AAAA:
			in.Type, in.Value = "AAAA", v.AAAA.String()
		case *dns.CNAME:
			in.Type, in.Value = "CNAME", v.Target
		case *dns.MX:
			in.Type, in.Value, in.Priority = "MX", v.Mx, int(v.Preference)
		case *dns.TXT:
			in.Type, in.Value = "TXT", strings.Join(v.Txt, "")
		case *dns.CAA:
			in.Type, in.Value = "CAA", fmt.Sprintf("%d %s %q", v.Flag, v.Tag, v.Value)
		case *dns.SRV:
			in.Type, in.Value, in.Priority = "SRV", fmt.Sprintf("%d %d %s", v.Weight, v.Port, v.Target), int(v.Priority)
		default:
			skipped = append(skipped, rel+" "+dns.TypeToString[h.Rrtype]+" (type not supported)")
			continue
		}
		recs = append(recs, in)
	}
	if err := zp.Err(); err != nil {
		return nil, nil, apiErr{http.StatusUnprocessableEntity, "the zone file does not parse: " + err.Error()}
	}
	return recs, skipped, nil
}

// importZoneTx adds the records of a zone file; records already in the zone are kept as
// they are (same name, type and value), and records that would clash are reported.
func (s *Server) importZoneTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, z *dnsZoneView, text string) (added int, skipped []string, err error) {
	recs, skipped, err := parseZoneFile(z.Domain, text)
	if err != nil {
		return 0, nil, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM dns_records WHERE zone_id = $1`, z.ID).Scan(&count); err != nil {
		return 0, nil, err
	}
	for _, in := range recs {
		in, nerr := normalizeRecord(z.Domain, in)
		if nerr != nil {
			skipped = append(skipped, in.Name+" "+in.Type+" ("+strings.TrimSuffix(nerr.Error(), ".")+")")
			continue
		}
		var exists, clash bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_records WHERE zone_id = $1 AND name = $2 AND type = $3 AND value = $4 AND priority = $5)`,
			z.ID, in.Name, in.Type, in.Value, in.Priority).Scan(&exists); err != nil {
			return 0, nil, err
		}
		if exists {
			continue
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_records WHERE zone_id = $1 AND name = $2 AND (type = 'CNAME' OR $3 = 'CNAME'))`,
			z.ID, in.Name, in.Type).Scan(&clash); err != nil {
			return 0, nil, err
		}
		if clash {
			skipped = append(skipped, in.Name+" "+in.Type+" (a CNAME already uses this name)")
			continue
		}
		if count >= 500 {
			skipped = append(skipped, in.Name+" "+in.Type+" (the zone holds at most 500 records)")
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dns_records (org_id, zone_id, name, type, value, priority, ttl) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			o.ID, z.ID, in.Name, in.Type, in.Value, in.Priority, in.TTL); err != nil {
			return 0, nil, err
		}
		count++
		added++
	}
	return added, skipped, s.zoneChanged(ctx, r, tx, o, z, "dns.zone_imported", map[string]any{"added": added, "skipped": len(skipped)})
}

// apiImportZone: POST /api/v1/dns/zones/{zone}/import with the zone file as the body
// (text/plain), or JSON {"zone_file": "..."}.
func (s *Server) apiImportZone(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxZoneFile+1))
	if err != nil {
		apiError(w, http.StatusBadRequest, "cannot read the body")
		return
	}
	text := string(body)
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		var in struct {
			ZoneFile string `json:"zone_file"`
		}
		if json.Unmarshal(body, &in) == nil {
			text = in.ZoneFile
		}
	}
	var added int
	var skipped []string
	err = db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		z, _, err := loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		if err != nil {
			return err
		}
		added, skipped, err = s.importZoneTx(r.Context(), r, tx, o, z, text)
		return err
	})
	if apiTx(w, s, err) {
		if skipped == nil {
			skipped = []string{}
		}
		writeJSON(w, map[string]any{"added": added, "skipped": skipped})
	}
}
