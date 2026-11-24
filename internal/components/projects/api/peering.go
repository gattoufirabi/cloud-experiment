package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
)

// connectProjectsTx connects (or, with remove, disconnects) p and the project `other`, and
// redeploys both projects' live services. It returns how many are redeploying.
func (s *API) connectProjectsTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, other string, remove bool) (int, error) {
	other = strings.TrimSpace(other)
	if other == "" || other == p.Slug {
		return 0, web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "choose another project of the organization"}
	}
	var q web.ProjectInfo
	if err := tx.QueryRow(r.Context(), `SELECT id, slug FROM projects WHERE slug = $1`, other).Scan(&q.ID, &q.Slug); err != nil {
		return 0, web.APIErr{Status: http.StatusNotFound, Msg: "project " + other + " not found"}
	}
	a, b := p.ID, q.ID
	if b < a {
		a, b = b, a
	}
	if remove {
		tag, err := tx.Exec(r.Context(), `DELETE FROM project_peerings WHERE project_a = $1 AND project_b = $2`, a, b)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, web.APIErr{Status: http.StatusNotFound, Msg: "these projects are not connected"}
		}
	} else {
		tag, err := tx.Exec(r.Context(), `INSERT INTO project_peerings (org_id, project_a, project_b) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, o.ID, a, b)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, web.APIErr{Status: http.StatusConflict, Msg: "these projects are already connected"}
		}
	}
	action := "project.connected"
	if remove {
		action = "project.disconnected"
	}
	s.Audit(r, tx, o.ID, action, "project:"+p.Slug, map[string]string{"with": q.Slug})
	n1, err := s.redeployProjectServices(r, tx, o, p)
	if err != nil {
		return 0, err
	}
	n2, err := s.redeployProjectServices(r, tx, o, &q)
	return n1 + n2, err
}

// ---------- console ----------

func (s *API) projectConnection(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	remove := r.PostFormValue("remove") == "1"
	other := r.PostFormValue("project")
	var p *web.ProjectInfo
	var n int
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.LoadProject(r, tx); err != nil {
			return err
		}
		n, err = s.connectProjectsTx(r, tx, o, p, other, remove)
		return err
	})
	path := o.Path() + "/p/" + r.PathValue("project") + "#private"
	var ae web.APIErr
	switch {
	case web.IsNoRows(err):
		s.NotFound(w, r)
	case errors.As(err, &ae):
		web.Back(w, r, path, "", ae.Msg+".")
	case err != nil:
		s.ServerError(w, r, err)
	case remove:
		web.Back(w, r, path, fmt.Sprintf("Disconnected from %s; %d service(s) redeploying without the other project's names.", other, n), "")
	default:
		web.Back(w, r, path, fmt.Sprintf("Connected to %s: services of both projects reach each other at <service>.<project>.internal; %d service(s) redeploying to learn the names.", other, n), "")
	}
}

// ---------- REST ----------

func (s *API) apiPeers(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []web.PeerView
	if s.apiProject(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo) error {
		var err error
		out, err = web.LoadPeers(r.Context(), tx, p.ID)
		return err
	}) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiConnect(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"project": "other-project"}`)
		return
	}
	var n int
	if s.apiProject(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo) error {
		var err error
		n, err = s.connectProjectsTx(r, tx, o, p, in.Project, false)
		return err
	}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]int{"redeploying": n})
	}
}

func (s *API) apiDisconnect(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if s.apiProject(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo) error {
		_, err := s.connectProjectsTx(r, tx, o, p, r.PathValue("other"), true)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
