package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
)

// projectMapFragment serves the map alone (htmx refreshes it in place).
func (s *API) projectMapFragment(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, o.Path()+"/p/"+r.PathValue("project")+"#map", http.StatusSeeOther)
		return
	}
	var pm *web.ProjectMap
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.LoadProject(r, tx)
		if err != nil {
			return err
		}
		pm, err = s.LoadProjectMap(r.Context(), tx, o, p)
		return err
	})
	switch {
	case web.IsNoRows(err):
		s.NotFound(w, r)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		s.RenderPartial(w, r, "projectmap", pm)
	}
}
