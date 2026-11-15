package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// The sidebar pane: every project of the organization with its services and the status
// of each service's latest deployment. Loaded for every page that belongs to an org.

type navService struct{ Slug, Status string }

type navProject struct {
	Slug, Name string
	Services   []navService
}

func (s *Server) loadNav(r *http.Request, o *orgInfo) []navProject {
	var out []navProject
	_ = db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT p.slug, p.name, coalesce(s.slug, ''),
			       coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'none')
			FROM projects p LEFT JOIN services s ON s.project_id = p.id
			ORDER BY p.name, p.slug, s.slug`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ps, pn, ss, st string
			if err := rows.Scan(&ps, &pn, &ss, &st); err != nil {
				return err
			}
			if len(out) == 0 || out[len(out)-1].Slug != ps {
				out = append(out, navProject{Slug: ps, Name: pn})
			}
			if ss != "" {
				out[len(out)-1].Services = append(out[len(out)-1].Services, navService{Slug: ss, Status: st})
			}
		}
		return rows.Err()
	})
	return out
}

// addNav fills the sidebar data when the page belongs to an organization.
func (s *Server) addNav(r *http.Request, d data) {
	o, ok := d["Org"].(*orgInfo)
	if !ok || o == nil {
		return
	}
	d["HasNav"] = true
	d["Nav"] = s.loadNav(r, o)
	d["CurProject"] = r.PathValue("project")
	d["CurService"] = r.PathValue("service")
	d["RegistryHostNav"] = s.cfg.RegistryHost != ""
}
