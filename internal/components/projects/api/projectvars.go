package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
)

// checkProjectVar validates a name and value; the message is shown to the user.
func checkProjectVar(name, value string) string {
	if !model.SecretNameRe.MatchString(name) || strings.HasPrefix(name, "PLATFORM_") || name == "PORT" {
		return "Variable names use uppercase letters, digits and underscores (for example API_BASE_URL); PORT and PLATFORM_* are reserved."
	}
	if value == "" || len(value) > 8192 {
		return "The value must be between 1 and 8192 characters."
	}
	return ""
}

func (s *API) saveProjectVar(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, name, value string, secret bool) error {
	enc, err := s.Box.Seal([]byte(value), web.ProjVarAAD(p.ID, name))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO project_variables (org_id, project_id, name, value_enc, secret) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, secret = EXCLUDED.secret, updated_at = now()`,
		o.ID, p.ID, name, enc, secret); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "project_variable.set", "project:"+p.Slug, map[string]any{"name": name, "secret": secret})
	return nil
}

func (s *API) removeProjectVar(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, name string) (bool, error) {
	tag, err := tx.Exec(r.Context(), `DELETE FROM project_variables WHERE project_id = $1 AND name = $2`, p.ID, name)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	s.Audit(r, tx, o.ID, "project_variable.deleted", "project:"+p.Slug, map[string]string{"name": name})
	return true, nil
}

// redeployProjectServices starts a new deployment, with the same settings, of every
// service in the project (previews included) whose current deployment is live, so they
// pick up changed project variables. Returns how many were started.
func (s *API) redeployProjectServices(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo) (int, error) {
	if err := s.CheckDisk(r, tx, o); err != nil {
		return 0, err
	}
	rows, err := tx.Query(r.Context(), `
		SELECT d.service_id, d.spec FROM deployments d JOIN services s ON s.id = d.service_id
		WHERE s.project_id = $1 AND d.status = 'live'
		  AND NOT EXISTS (SELECT 1 FROM deployments x WHERE x.service_id = d.service_id AND x.status IN ('pending', 'deploying'))`, p.ID)
	if err != nil {
		return 0, err
	}
	type live struct {
		serviceID string
		spec      []byte
	}
	var lives []live
	for rows.Next() {
		var l live
		if err := rows.Scan(&l.serviceID, &l.spec); err != nil {
			rows.Close()
			return 0, err
		}
		lives = append(lives, l)
	}
	rows.Close()
	n := 0
	for _, l := range lives {
		var spec model.Spec
		if err := json.Unmarshal(l.spec, &spec); err != nil {
			return n, err
		}
		if _, err := s.CreateDeployment(r, tx, o, l.serviceID, spec); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		s.Audit(r, tx, o.ID, "project.redeployed", "project:"+p.Slug, map[string]int{"services": n})
	}
	return n, nil
}

// ---------- console ----------

// withProject runs fn in the organization's tenant context with the project loaded.
func (s *API) withProject(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo) error) (*web.OrgInfo, *web.ProjectInfo, error, bool) {
	o, ok := s.Org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var p *web.ProjectInfo
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.LoadProject(r, tx); err != nil {
			return err
		}
		return fn(tx, o, p)
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return nil, nil, nil, false
	}
	return o, p, err, true
}

func (s *API) setProjectVar(w http.ResponseWriter, r *http.Request) {
	var formErr string
	redeployed := -1
	o, p, err, ok := s.withProject(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo) error {
		name := strings.TrimSpace(r.PostFormValue("name"))
		value := r.PostFormValue("value")
		if formErr = checkProjectVar(name, value); formErr != "" {
			return nil
		}
		if err := s.saveProjectVar(r, tx, o, p, name, value, r.PostFormValue("secret") != ""); err != nil {
			return err
		}
		if r.PostFormValue("redeploy") != "" {
			n, err := s.redeployProjectServices(r, tx, o, p)
			var qe web.QuotaError
			if errors.As(err, &qe) {
				formErr = "Variable saved, but services were not redeployed: " + qe.Error()
				return nil
			}
			redeployed = n
			return err
		}
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	path := o.Path() + "/p/" + p.Slug + "#variables"
	switch {
	case formErr != "":
		web.Back(w, r, path, "", formErr)
	case redeployed >= 0:
		web.Back(w, r, path, "Variable saved. Redeploying "+web.Itoa(redeployed)+" service(s).", "")
	default:
		web.Back(w, r, path, "Variable saved. Redeploy the services to apply it.", "")
	}
}

func (s *API) deleteProjectVar(w http.ResponseWriter, r *http.Request) {
	o, p, err, ok := s.withProject(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo) error {
		_, err := s.removeProjectVar(r, tx, o, p, r.PathValue("name"))
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/p/"+p.Slug+"#variables", "Variable deleted. Redeploy the services to apply it.", "")
}

func (s *API) redeployProject(w http.ResponseWriter, r *http.Request) {
	var formErr string
	n := 0
	o, p, err, ok := s.withProject(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo) error {
		var err error
		n, err = s.redeployProjectServices(r, tx, o, p)
		var qe web.QuotaError
		if errors.As(err, &qe) {
			formErr = qe.Error()
			return nil
		}
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	path := o.Path() + "/p/" + p.Slug + "#variables"
	switch {
	case formErr != "":
		web.Back(w, r, path, "", formErr)
	case n == 0:
		web.Back(w, r, path, "No running service to redeploy.", "")
	default:
		web.Back(w, r, path, "Redeploying "+web.Itoa(n)+" service(s).", "")
	}
}

// ---------- API ----------

func (s *API) apiProject(w http.ResponseWriter, r *http.Request, o *web.OrgInfo, fn func(tx pgx.Tx, p *web.ProjectInfo) error) bool {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.LoadProject(r, tx)
		if err != nil {
			return err
		}
		return fn(tx, p)
	})
	var ae web.APIErr
	switch {
	case web.IsNoRows(err):
		web.APIError(w, http.StatusNotFound, "project not found")
	case errors.As(err, &ae):
		web.APIError(w, ae.Status, ae.Msg)
	case err != nil:
		s.Log.Error("api", "path", r.URL.Path, "err", err)
		web.APIError(w, http.StatusInternalServerError, "internal error")
	default:
		return true
	}
	return false
}

func (s *API) apiProjectVars(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var vars []web.ProjectVar
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo) error {
		var err error
		vars, err = s.LoadProjectVars(r, tx, p.ID)
		return err
	}) {
		return
	}
	if vars == nil {
		vars = []web.ProjectVar{}
	}
	web.WriteJSON(w, vars)
}

func (s *API) apiSetProjectVar(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	name := r.PathValue("name")
	var body struct {
		Value  string `json:"value"`
		Secret *bool  `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		web.APIError(w, http.StatusBadRequest, "body must be JSON: {\"value\": \"...\", \"secret\": true}")
		return
	}
	if msg := checkProjectVar(name, body.Value); msg != "" {
		web.APIError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	secret := body.Secret == nil || *body.Secret // secret unless said otherwise
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo) error {
		return s.saveProjectVar(r, tx, o, p, name, body.Value, secret)
	}) {
		return
	}
	web.WriteJSON(w, map[string]any{"name": name, "secret": secret, "status": "saved; redeploy the services to apply it"})
}

func (s *API) apiDeleteProjectVar(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo) error {
		found, err := s.removeProjectVar(r, tx, o, p, r.PathValue("name"))
		if err == nil && !found {
			return web.APIErr{Status: http.StatusNotFound, Msg: "variable not found"}
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *API) apiRedeployProject(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	n := 0
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo) error {
		var err error
		n, err = s.redeployProjectServices(r, tx, o, p)
		var qe web.QuotaError
		if errors.As(err, &qe) {
			return web.APIErr{Status: http.StatusInsufficientStorage, Msg: qe.Error()}
		}
		return err
	}) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]int{"redeploying": n})
}
