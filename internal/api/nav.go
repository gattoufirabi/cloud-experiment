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
	Databases  []navService
	Functions  []navService
}

func (s *Server) loadNav(r *http.Request, o *orgInfo) []navProject {
	var out []navProject
	_ = db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT p.slug, p.name, coalesce(s.slug, ''),
			       coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'none')
			FROM projects p LEFT JOIN services s ON s.project_id = p.id AND s.preview_of IS NULL
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
		if err := rows.Err(); err != nil {
			return err
		}
		drows, err := tx.Query(r.Context(), `
			SELECT p.slug, d.slug, CASE d.status WHEN 'running' THEN 'live' WHEN 'failed' THEN 'failed' ELSE 'pending' END
			FROM databases d JOIN projects p ON p.id = d.project_id ORDER BY d.slug`)
		if err != nil {
			return err
		}
		defer drows.Close()
		for drows.Next() {
			var ps, ds, st string
			if err := drows.Scan(&ps, &ds, &st); err != nil {
				return err
			}
			for i := range out {
				if out[i].Slug == ps {
					out[i].Databases = append(out[i].Databases, navService{Slug: ds, Status: st})
				}
			}
		}
		if err := drows.Err(); err != nil {
			return err
		}
		frows, err := tx.Query(r.Context(), `SELECT p.slug, f.slug FROM functions f JOIN projects p ON p.id = f.project_id ORDER BY f.slug`)
		if err != nil {
			return err
		}
		defer frows.Close()
		for frows.Next() {
			var ps, fs string
			if err := frows.Scan(&ps, &fs); err != nil {
				return err
			}
			for i := range out {
				if out[i].Slug == ps {
					out[i].Functions = append(out[i].Functions, navService{Slug: fs})
				}
			}
		}
		return frows.Err()
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
	d["CurDB"] = r.PathValue("db")
	d["CurFn"] = r.PathValue("fn")
	d["RegistryHostNav"] = s.cfg.RegistryHost != ""
	d["StorageNav"] = s.cfg.StorageEnabled()
	s.addConsole(r, d, o)
	d["Crumbs"] = crumbsFor(d)
}
