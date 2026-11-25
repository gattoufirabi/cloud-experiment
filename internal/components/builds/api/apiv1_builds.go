package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
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

func (s *API) apiSource(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out sourceJSON
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return tx.QueryRow(r.Context(), `
			SELECT source_repo, source_branch, source_dir, pr_previews, build_on, build_labels, source_token_enc IS NOT NULL
			FROM services WHERE id = $1`, sv.ID).Scan(&out.Repo, &out.Branch, &out.Dir, &out.PRPreviews, &out.BuildOn, &out.BuildLabels, &out.HasToken)
	}) {
		return
	}
	web.WriteJSON(w, out)
}

// apiPutSource replaces the source settings (fields left out take their defaults; the
// token is kept unless given).
func (s *API) apiPutSource(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var body sourceJSON
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON, for example {"repo": "https://github.com/acme/web", "branch": "main", "build_on": "servers", "build_labels": ["gpu"]}`)
		return
	}
	labels, err := web.ParseLabels(strings.Join(body.BuildLabels, ","))
	if err != nil {
		web.APIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in := sourceInput{Repo: body.Repo, Branch: body.Branch, Dir: body.Dir, PRPreviews: body.PRPreviews, BuildOn: body.BuildOn, BuildLabels: labels}
	if body.Token != nil {
		in.Token, in.ClearToken = *body.Token, *body.Token == ""
	}
	if msg := s.checkSource(&in); msg != "" {
		web.APIError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		msg, err := s.storeSource(r, tx, o, p, sv, in)
		if msg != "" {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: msg}
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
func organizationWide(w http.ResponseWriter, o *web.OrgInfo) bool {
	if o.Scoped {
		web.APIError(w, http.StatusForbidden, "this token is limited to a project; servers belong to the organization")
		return false
	}
	return true
}

func (s *API) loadServersJSON(r *http.Request, o *web.OrgInfo, id string) ([]serverJSON, error) {
	out := []serverJSON{}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT n.id, n.name, `+web.ServerOnlineSQL+`, n.os, n.arch, n.builds, n.labels, n.max_builds, n.clean_builds,
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
			v.AutoLabels = web.ServerView{OS: v.OS, Arch: v.Arch}.AutoLabels()
			if v.AutoLabels == nil {
				v.AutoLabels = []string{}
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

func (s *API) apiServers(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if !organizationWide(w, o) {
		return
	}
	list, err := s.loadServersJSON(r, o, "")
	if err != nil {
		s.Log.Error("api", "path", r.URL.Path, "err", err)
		web.APIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	web.WriteJSON(w, map[string]any{"servers": list})
}

func (s *API) apiServer(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if !organizationWide(w, o) {
		return
	}
	list, err := s.loadServersJSON(r, o, r.PathValue("id"))
	switch {
	case err != nil:
		s.Log.Error("api", "path", r.URL.Path, "err", err)
		web.APIError(w, http.StatusInternalServerError, "internal error")
	case len(list) == 0:
		web.APIError(w, http.StatusNotFound, "server not found")
	default:
		web.WriteJSON(w, list[0])
	}
}

// apiPutServerBuilds sets what a server does with builds.
func (s *API) apiPutServerBuilds(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if !organizationWide(w, o) {
		return
	}
	body := web.ServerBuilds{MaxBuilds: 1}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON, for example {"enabled": true, "labels": ["gpu"], "max_builds": 2, "clean": false}`)
		return
	}
	labels, err := web.ParseLabels(strings.Join(body.Labels, ","))
	if err != nil {
		web.APIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	body.Labels = labels
	if body.MaxBuilds < 1 || body.MaxBuilds > 8 {
		web.APIError(w, http.StatusUnprocessableEntity, web.MaxBuildsErr)
		return
	}
	err = db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		_, err := s.SaveServerBuilds(r, tx, o, r.PathValue("id"), body)
		return err
	})
	switch {
	case web.IsNoRows(err):
		web.APIError(w, http.StatusNotFound, "server not found")
	case err != nil:
		s.Log.Error("api", "path", r.URL.Path, "err", err)
		web.APIError(w, http.StatusInternalServerError, "internal error")
	default:
		s.apiServer(w, r, o)
	}
}
