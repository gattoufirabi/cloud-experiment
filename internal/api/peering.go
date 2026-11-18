package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// Private connections between projects: each web service is <service>.<project>.internal
// on the private network, reachable from its own project and from the projects connected
// to it. Connecting or disconnecting redeploys both projects' services so their
// containers learn (or forget) the names. Admins manage connections.

type peerView struct {
	Project string   `json:"project"`
	Names   []string `json:"names"` // the private names its services answer on
}

// loadPeers lists the projects connected to projectID and the names they expose.
func loadPeers(ctx context.Context, tx pgx.Tx, projectID string) ([]peerView, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.slug, coalesce(array_agg(s.slug || '.' || p.slug || '.internal' ORDER BY s.slug) FILTER (WHERE s.id IS NOT NULL), '{}')
		FROM project_peerings g
		JOIN projects p ON p.id = CASE WHEN g.project_a = $1 THEN g.project_b ELSE g.project_a END
		LEFT JOIN services s ON s.project_id = p.id AND s.preview_of IS NULL
		WHERE $1 IN (g.project_a, g.project_b)
		GROUP BY p.slug ORDER BY p.slug`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[peerView])
	if out == nil {
		out = []peerView{}
	}
	return out, err
}

// connectProjectsTx connects (or, with remove, disconnects) p and the project `other`, and
// redeploys both projects' live services. It returns how many are redeploying.
func (s *Server) connectProjectsTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, other string, remove bool) (int, error) {
	other = strings.TrimSpace(other)
	if other == "" || other == p.Slug {
		return 0, apiErr{http.StatusUnprocessableEntity, "choose another project of the organization"}
	}
	var q projectInfo
	if err := tx.QueryRow(r.Context(), `SELECT id, slug FROM projects WHERE slug = $1`, other).Scan(&q.ID, &q.Slug); err != nil {
		return 0, apiErr{http.StatusNotFound, "project " + other + " not found"}
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
			return 0, apiErr{http.StatusNotFound, "these projects are not connected"}
		}
	} else {
		tag, err := tx.Exec(r.Context(), `INSERT INTO project_peerings (org_id, project_a, project_b) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, o.ID, a, b)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, apiErr{http.StatusConflict, "these projects are already connected"}
		}
	}
	action := "project.connected"
	if remove {
		action = "project.disconnected"
	}
	s.audit(r, tx, o.ID, action, "project:"+p.Slug, map[string]string{"with": q.Slug})
	n1, err := s.redeployProjectServices(r, tx, o, p)
	if err != nil {
		return 0, err
	}
	n2, err := s.redeployProjectServices(r, tx, o, &q)
	return n1 + n2, err
}

// ---------- console ----------

func (s *Server) projectConnection(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	remove := r.PostFormValue("remove") == "1"
	other := r.PostFormValue("project")
	var p *projectInfo
	var n int
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.loadProject(r, tx); err != nil {
			return err
		}
		n, err = s.connectProjectsTx(r, tx, o, p, other, remove)
		return err
	})
	path := o.Path() + "/p/" + r.PathValue("project") + "#private"
	var ae apiErr
	switch {
	case isNoRows(err):
		s.notFound(w, r)
	case errors.As(err, &ae):
		back(w, r, path, "", ae.msg+".")
	case err != nil:
		s.serverError(w, r, err)
	case remove:
		back(w, r, path, fmt.Sprintf("Disconnected from %s; %d service(s) redeploying without the other project's names.", other, n), "")
	default:
		back(w, r, path, fmt.Sprintf("Connected to %s: services of both projects reach each other at <service>.<project>.internal; %d service(s) redeploying to learn the names.", other, n), "")
	}
}

// ---------- REST ----------

func (s *Server) apiPeers(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []peerView
	if s.apiProject(w, r, o, func(tx pgx.Tx, p *projectInfo) error {
		var err error
		out, err = loadPeers(r.Context(), tx, p.ID)
		return err
	}) {
		writeJSON(w, out)
	}
}

func (s *Server) apiConnect(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var in struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"project": "other-project"}`)
		return
	}
	var n int
	if s.apiProject(w, r, o, func(tx pgx.Tx, p *projectInfo) error {
		var err error
		n, err = s.connectProjectsTx(r, tx, o, p, in.Project, false)
		return err
	}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]int{"redeploying": n})
	}
}

func (s *Server) apiDisconnect(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiProject(w, r, o, func(tx pgx.Tx, p *projectInfo) error {
		_, err := s.connectProjectsTx(r, tx, o, p, r.PathValue("other"), true)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
