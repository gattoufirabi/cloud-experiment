package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
)

// importZoneTx adds the records of a zone file; records already in the zone are kept as
// they are (same name, type and value), and records that would clash are reported.
func (s *API) importZoneTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, z *web.DNSZoneView, text string) (added int, skipped []string, err error) {
	recs, skipped, err := web.ParseZoneFile(z.Domain, text)
	if err != nil {
		return 0, nil, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM dns_records WHERE zone_id = $1`, z.ID).Scan(&count); err != nil {
		return 0, nil, err
	}
	for _, in := range recs {
		in, nerr := web.NormalizeRecord(z.Domain, in)
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
	return added, skipped, s.ZoneChanged(ctx, r, tx, o, z, "dns.zone_imported", map[string]any{"added": added, "skipped": len(skipped)})
}

// apiImportZone: POST /api/v1/dns/zones/{zone}/import with the zone file as the body
// (text/plain), or JSON {"zone_file": "..."}.
func (s *API) apiImportZone(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, web.MaxZoneFile+1))
	if err != nil {
		web.APIError(w, http.StatusBadRequest, "cannot read the body")
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
	err = db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		z, _, err := loadDNSZone(r.Context(), tx, r.PathValue("zone"))
		if err != nil {
			return err
		}
		added, skipped, err = s.importZoneTx(r.Context(), r, tx, o, z, text)
		return err
	})
	if web.APITx(w, s.Server, err) {
		if skipped == nil {
			skipped = []string{}
		}
		web.WriteJSON(w, map[string]any{"added": added, "skipped": skipped})
	}
}
