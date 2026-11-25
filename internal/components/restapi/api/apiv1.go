package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/registry"
	"platform/internal/lib/secure"
)

func (s *API) apiMe(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	tok, _ := r.Context().Value(web.APITokenKey).(string)
	web.WriteJSON(w, map[string]string{"organization": o.Slug, "name": o.Name, "role": o.Role, "token": tok})
}

func (s *API) apiProjects(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	type project struct {
		Slug     string `json:"id"`
		Name     string `json:"name"`
		Services int    `json:"services"`
	}
	var out []project
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT p.slug, p.name, (SELECT count(*) FROM services s WHERE s.project_id = p.id AND s.preview_of IS NULL) FROM projects p ORDER BY p.slug`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[project])
		return err
	})
	if err != nil {
		web.APIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if out == nil {
		out = []project{}
	}
	web.WriteJSON(w, out)
}

func (s *API) apiServices(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	type service struct {
		Slug   string `json:"id"`
		Image  string `json:"image"`
		Status string `json:"status"`
	}
	var out []service
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.LoadProject(r, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `
			SELECT s.slug, s.image,
			       coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'not deployed')
			FROM services s WHERE s.project_id = $1 AND s.preview_of IS NULL ORDER BY s.slug`, p.ID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[service])
		return err
	})
	if web.IsNoRows(err) {
		web.APIError(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		web.APIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if out == nil {
		out = []service{}
	}
	web.WriteJSON(w, out)
}

type apiDeploymentView struct {
	Revision  int       `json:"revision"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	Image     string    `json:"image"`
	By        string    `json:"by"`
	CreatedAt time.Time `json:"created_at"`
}

func toAPIDeployments(in []web.DeploymentView) []apiDeploymentView {
	out := make([]apiDeploymentView, 0, len(in))
	for _, d := range in {
		out = append(out, apiDeploymentView{d.Revision, d.Status, d.Message, d.Image, d.By, d.CreatedAt})
	}
	return out
}

func (s *API) apiService1(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var view apiServiceView
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		deps, err := web.RecentDeployments(r, tx, sv.ID, 10)
		if err != nil {
			return err
		}
		if view, err = s.apiServiceViewOf(r, tx, o, p, sv); err != nil {
			return err
		}
		view.Deployments = toAPIDeployments(deps)
		return nil
	}) {
		return
	}
	web.WriteJSON(w, view)
}

// apiScale changes the number of instances and deploys: POST {"instances": 3}
func (s *API) apiScale(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var body struct {
		Instances int `json:"instances"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON, for example {"instances": 3}`)
		return
	}
	var rev int
	var path string
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if sv.IsPreview() {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a preview runs a single instance"}
		}
		spec := sv.Spec()
		spec.Instances = body.Instances
		if body.Instances < 1 {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "instances must be between 1 and 10"}
		}
		if err := spec.Validate(); err != nil {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
		}
		for _, check := range []func() error{
			func() error { return s.CheckInstances(r, tx, o, spec.Count()) },
			func() error {
				return s.CheckQuota(r, tx, o, sv.ID, spec.CPUMillis*spec.Count(), spec.MemoryMB*spec.Count(), false)
			},
		} {
			if err := check(); err != nil {
				var qe web.QuotaError
				if errors.As(err, &qe) {
					return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: qe.Error()}
				}
				return err
			}
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET instances = $2 WHERE id = $1`, sv.ID, spec.Count()); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "service.scaled", "service:"+p.Slug+"/"+sv.Slug, map[string]int{"instances": spec.Count()})
		var err error
		rev, err = s.CreateDeployment(r, tx, o, sv.ID, spec)
		path = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/deployments/" + web.Itoa(rev)
		return err
	}) {
		return
	}
	w.Header().Set("Location", path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"instances": body.Instances, "revision": rev, "status": "pending", "status_url": path})
}

// apiDeploy deploys the current settings, optionally with a new image first:
// POST {"image": "registry.example.com/acme/web:1.4.2"}
func (s *API) apiDeploy(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var body struct {
		Image string `json:"image"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
			web.APIError(w, http.StatusBadRequest, "body must be JSON, for example {\"image\": \"nginx:1.27-alpine\"}")
			return
		}
	}
	var rev int
	var path string
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		spec := sv.Spec()
		if img := strings.TrimSpace(body.Image); img != "" {
			spec.Image = img
		}
		if err := spec.Validate(); err != nil {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
		}
		if err := registry.CheckNamespace(spec.Image, s.Cfg.RegistryHost, o.Slug); err != nil {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
		}
		if err := s.CheckDisk(r, tx, o); err != nil {
			var qe web.QuotaError
			if errors.As(err, &qe) {
				return web.APIErr{Status: http.StatusInsufficientStorage, Msg: qe.Error()}
			}
			return err
		}
		if spec.Image != sv.Image {
			if _, err := tx.Exec(r.Context(), `UPDATE services SET image = $2 WHERE id = $1`, sv.ID, spec.Image); err != nil {
				return err
			}
		}
		var err error
		rev, err = s.CreateDeployment(r, tx, o, sv.ID, spec)
		path = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/deployments/" + web.Itoa(rev)
		return err
	}) {
		return
	}
	w.Header().Set("Location", path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"revision": rev, "status": "pending", "status_url": path})
}

func (s *API) apiDeployment(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out apiDeploymentView
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return tx.QueryRow(r.Context(), `
			SELECT d.revision, d.status, d.message, d.spec->>'image', coalesce(u.email, ''), d.created_at
			FROM deployments d LEFT JOIN users u ON u.id = d.created_by
			WHERE d.service_id = $1 AND d.revision = $2`, sv.ID, web.Atoi(r.PathValue("rev"))).
			Scan(&out.Revision, &out.Status, &out.Message, &out.Image, &out.By, &out.CreatedAt)
	}) {
		return
	}
	web.WriteJSON(w, out)
}

func (s *API) apiLogs(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var serviceID string
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		serviceID = sv.ID
		return nil
	}) {
		return
	}
	web.WriteJSON(w, s.QueryLogs(o.ID, serviceID, r.URL.Query().Get("q")))
}

// apiSetSecret: PUT .../secrets/{NAME} {"value": "..."}; applies at the next deployment.
func (s *API) apiSetSecret(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	name := r.PathValue("name")
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		web.APIError(w, http.StatusBadRequest, "body must be JSON: {\"value\": \"...\"}")
		return
	}
	if !model.SecretNameRe.MatchString(name) || strings.HasPrefix(name, "PLATFORM_") {
		web.APIError(w, http.StatusUnprocessableEntity, "secret names use uppercase letters, digits and underscores")
		return
	}
	if body.Value == "" || len(body.Value) > 8192 {
		web.APIError(w, http.StatusUnprocessableEntity, "the value must be between 1 and 8192 characters")
		return
	}
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		enc, err := s.Box.Seal([]byte(body.Value), "secret:"+sv.ID+":"+name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO secrets (org_id, service_id, name, value_enc) VALUES ($1, $2, $3, $4)
			ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, source = '', created_at = now()`,
			o.ID, sv.ID, name, enc); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "secret.set", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"name": name})
		return nil
	}) {
		return
	}
	web.WriteJSON(w, map[string]string{"name": name, "status": "saved; deploy to apply it"})
}

// ---------- console: API token management (admins) ----------

func (s *API) createAPIToken(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	role := r.PostFormValue("role")
	project := r.PostFormValue("project") // empty: the whole organization
	if project != "" && role == "admin" {
		web.Back(w, r, o.Path()+"/iam#api", "", "A token limited to a project can be viewer or developer.")
		return
	}
	if name == "" || len(name) > 60 || (role != "viewer" && role != "developer" && role != "admin") {
		web.Back(w, r, o.Path()+"/iam#api", "", "Give the token a name (up to 60 characters) and a role.")
		return
	}
	token := web.APITokenPrefix + secure.NewToken()
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var projectID any
		if project != "" {
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE slug = $1`, project).Scan(&id); err != nil {
				return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "Choose a project."}
			}
			projectID = id
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO api_tokens (org_id, name, token_hash, role, created_by, expires_at, project_id)
			VALUES ($1, $2, $3, $4, $5, now() + interval '1 year', $6)`,
			o.ID, name, secure.HashToken(token), role, web.SessionFrom(r).UserID, projectID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "api_token.created", "token:"+name, map[string]string{"role": role, "project": project})
		return nil
	})
	var ae web.APIErr
	if errors.As(err, &ae) {
		web.Back(w, r, o.Path()+"/iam#api", "", ae.Msg)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.RenderOrg(w, r, o, http.StatusOK, web.Data{"APIToken": token, "APITokenName": name, "Tab": "api", "Page": "iam"})
}

func (s *API) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `DELETE FROM api_tokens WHERE id = $1 RETURNING name`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "api_token.revoked", "token:"+name, nil)
		return nil
	})
	if err != nil && !web.IsNoRows(err) {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/iam#api", "API token revoked. It stops working immediately.", "")
}
