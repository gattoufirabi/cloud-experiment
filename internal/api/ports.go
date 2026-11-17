package api

import (
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"platform/internal/model"
)

// allocatePorts gives each wanted container port (TCP/UDP) a public port: the one the
// service already has for it, or the first free one of the platform's range. Ports the
// service no longer exposes are released. The plan limits how many an organization has.
func (s *Server) allocatePorts(r *http.Request, tx pgx.Tx, o *orgInfo, serviceID string, want []model.PublicPort) ([]model.PublicPort, error) {
	ctx := r.Context()
	if len(want) > 0 {
		var used, max int
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM public_ports WHERE service_id <> $2), p.max_public_ports FROM plans p WHERE p.id = $1`,
			o.PlanID, serviceID).Scan(&used, &max); err != nil {
			return nil, err
		}
		if used+len(want) > max {
			return nil, quotaError(fmt.Sprintf("Your plan allows %d public TCP/UDP ports (%d used by other services).", max, used))
		}
	}
	have := map[string]int{}
	rows, err := tx.Query(ctx, `SELECT container_port, protocol, public_port FROM public_ports WHERE service_id = $1`, serviceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p model.PublicPort
		if err := rows.Scan(&p.ContainerPort, &p.Protocol, &p.PublicPort); err != nil {
			rows.Close()
			return nil, err
		}
		have[p.Key()] = p.PublicPort
	}
	rows.Close()
	keep := map[string]bool{}
	out := make([]model.PublicPort, 0, len(want))
	for _, p := range want {
		keep[p.Key()] = true
		if pub, ok := have[p.Key()]; ok {
			p.PublicPort = pub
			out = append(out, p)
			continue
		}
		assigned := 0
		for cand := s.cfg.PublicPortMin; cand <= s.cfg.PublicPortMax && assigned == 0; cand++ {
			var taken bool
			if err := tx.QueryRow(ctx, `SELECT public_port_taken($1, $2)`, cand, p.Protocol).Scan(&taken); err != nil {
				return nil, err
			}
			if taken {
				continue
			}
			tag, err := tx.Exec(ctx, `INSERT INTO public_ports (public_port, protocol, org_id, service_id, container_port) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING`, cand, p.Protocol, o.ID, serviceID, p.ContainerPort)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() == 1 {
				assigned = cand
			}
		}
		if assigned == 0 {
			return nil, quotaError("No public port is free on this platform; ask the operator.")
		}
		p.PublicPort = assigned
		out = append(out, p)
	}
	for key, pub := range have {
		if !keep[key] {
			if _, err := tx.Exec(ctx, `DELETE FROM public_ports WHERE service_id = $1 AND public_port = $2`, serviceID, pub); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
