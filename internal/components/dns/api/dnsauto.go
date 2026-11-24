package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
)

// autoZoneTx gives a new zone the records of the organization's domains it holds.
func (s *API) autoZoneTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, zone string) error {
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
			d.Target += "-" + o.Slug + "." + s.Cfg.AppsDomain
			doms = append(doms, d)
		}
	}
	rows.Close()
	for _, d := range doms {
		if _, err := s.AutoServiceDomainTx(ctx, r, tx, o, d.ID, d.Host, d.Token, d.Target); err != nil {
			return err
		}
	}
	eds, err := web.LoadSendDomains(ctx, tx)
	if err != nil {
		return err
	}
	for _, d := range eds {
		if d.Domain == zone || strings.HasSuffix(d.Domain, "."+zone) {
			if _, err := s.AutoEmailDomainTx(ctx, r, tx, o, d); err != nil {
				return err
			}
		}
	}
	return nil
}
