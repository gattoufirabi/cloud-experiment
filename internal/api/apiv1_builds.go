package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// Public API for where builds run: a service's git source and build settings, and the
// organization's build servers (migrations 069, 070). Used by the Terraform provider.

type sourceJSON struct {
	Repo        string   `json:"repo"`
	Branch      string   `json:"branch"`
	Dir         string   `json:"dir"`
	PRPreviews  bool     `json:"pr_previews"`
	BuildOn     string   `json:"build_on"`
	BuildLabels []string `json:"build_labels"`
	HasToken    bool     `json:"has_token"`
	Token       *string  `json:"token,omitempty"` // write only: "" removes it, absent keeps it
}

func (s *Server) apiSource(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out sourceJSON
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		return tx.QueryRow(r.Context(), `
			SELECT source_repo, source_branch, source_dir, pr_previews, build_on, build_labels, source_token_enc IS NOT NULL
			FROM services WHERE id = $1`, sv.ID).Scan(&out.Repo, &out.Branch, &out.Dir, &out.PRPreviews, &out.BuildOn, &out.BuildLabels, &out.HasToken)
	}) {
		return
	}
	writeJSON(w, out)
}

// apiPutSource replaces the source settings (fields left out take their defaults; the
// token is kept unless given).
func (s *Server) apiPutSource(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body sourceJSON
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"repo": "https://github.com/acme/web", "branch": "main", "build_on": "servers", "build_labels": ["gpu"]}`)
		return
	}
	labels, err := parseLabels(strings.Join(body.BuildLabels, ","))
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in := sourceInput{Repo: body.Repo, Branch: body.Branch, Dir: body.Dir, PRPreviews: body.PRPreviews, BuildOn: body.BuildOn, BuildLabels: labels}
	if body.Token != nil {
		in.Token, in.ClearToken = *body.Token, *body.Token == ""
	}
	if msg := s.checkSource(&in); msg != "" {
		apiError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		msg, err := s.storeSource(r, tx, o, p, sv, in)
		if msg != "" {
			return apiErr{http.StatusUnprocessableEntity, msg}
		}
		return err
	}) {
		return
	}
	s.apiSource(w, r, o)
}

type serverJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Online     bool     `json:"online"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	Builds     bool     `json:"builds"`
	Labels     []string `json:"labels"`
	AutoLabels []string `json:"auto_labels"`
	MaxBuilds  int      `json:"max_builds"`
	Clean      bool     `json:"clean_builds"`
	Running    []int    `json:"running_builds"`
}

// organizationWide refuses tokens limited to some projects.
func organizationWide(w http.ResponseWriter, o *orgInfo) bool {
	if o.Scoped {
		apiError(w, http.StatusForbidden, "this token is limited to a project; servers belong to the organization")
		return false
	}
	return true
}

func (s *Server) loadServersJSON(r *http.Request, o *orgInfo, id string) ([]serverJSON, error) {
	out := []serverJSON{}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT n.id, n.name, `+serverOnlineSQL+`, n.os, n.arch, n.builds, n.labels, n.max_builds, n.clean_builds,
			       coalesce((SELECT array_agg(b.number ORDER BY b.number) FROM builds b WHERE b.node_id = n.id AND b.status = 'running'), '{}')
			FROM nodes n WHERE n.role = 'customer' AND ($1 = '' OR n.id = $1) ORDER BY n.name, n.id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v serverJSON
			if err := rows.Scan(&v.ID, &v.Name, &v.Online, &v.OS, &v.Arch, &v.Builds, &v.Labels, &v.MaxBuilds, &v.Clean, &v.Running); err != nil {
				return err
			}
			v.AutoLabels = serverView{OS: v.OS, Arch: v.Arch}.AutoLabels()
			if v.AutoLabels == nil {
				v.AutoLabels = []string{}
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Server) apiServers(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if !organizationWide(w, o) {
		return
	}
	list, err := s.loadServersJSON(r, o, "")
	if err != nil {
		s.log.Error("api", "path", r.URL.Path, "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, map[string]any{"servers": list})
}

func (s *Server) apiServer(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if !organizationWide(w, o) {
		return
	}
	list, err := s.loadServersJSON(r, o, r.PathValue("id"))
	switch {
	case err != nil:
		s.log.Error("api", "path", r.URL.Path, "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	case len(list) == 0:
		apiError(w, http.StatusNotFound, "server not found")
	default:
		writeJSON(w, list[0])
	}
}

// apiPutServerBuilds sets what a server does with builds.
func (s *Server) apiPutServerBuilds(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if !organizationWide(w, o) {
		return
	}
	body := serverBuilds{MaxBuilds: 1}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"enabled": true, "labels": ["gpu"], "max_builds": 2, "clean": false}`)
		return
	}
	labels, err := parseLabels(strings.Join(body.Labels, ","))
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	body.Labels = labels
	if body.MaxBuilds < 1 || body.MaxBuilds > 8 {
		apiError(w, http.StatusUnprocessableEntity, maxBuildsErr)
		return
	}
	err = db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		_, err := s.saveServerBuilds(r, tx, o, r.PathValue("id"), body)
		return err
	})
	switch {
	case isNoRows(err):
		apiError(w, http.StatusNotFound, "server not found")
	case err != nil:
		s.log.Error("api", "path", r.URL.Path, "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	default:
		s.apiServer(w, r, o)
	}
}
