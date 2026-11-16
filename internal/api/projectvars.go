package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
)

// Shared project variables: set once on a project, passed to every service in it (and
// its previews). A service's own variables and secrets override a project variable of
// the same name. Values are encrypted at rest; secret ones are never shown again.

type projectVar struct {
	Name    string    `json:"name"`
	Secret  bool      `json:"secret"`
	Value   string    `json:"value,omitempty"` // plain variables only
	Updated time.Time `json:"updated_at"`
}

func projVarAAD(projectID, name string) string { return "projvar:" + projectID + ":" + name }

// loadProjectVars lists a project's variables; secret values stay encrypted.
func (s *Server) loadProjectVars(r *http.Request, tx pgx.Tx, projectID string) ([]projectVar, error) {
	rows, err := tx.Query(r.Context(), `SELECT name, secret, value_enc, updated_at FROM project_variables WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projectVar
	for rows.Next() {
		var v projectVar
		var enc []byte
		if err := rows.Scan(&v.Name, &v.Secret, &enc, &v.Updated); err != nil {
			return nil, err
		}
		if !v.Secret {
			plain, err := s.box.Open(enc, projVarAAD(projectID, v.Name))
			if err != nil {
				return nil, err
			}
			v.Value = string(plain)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

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

func (s *Server) saveProjectVar(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, name, value string, secret bool) error {
	enc, err := s.box.Seal([]byte(value), projVarAAD(p.ID, name))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO project_variables (org_id, project_id, name, value_enc, secret) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, secret = EXCLUDED.secret, updated_at = now()`,
		o.ID, p.ID, name, enc, secret); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "project_variable.set", "project:"+p.Slug, map[string]any{"name": name, "secret": secret})
	return nil
}

func (s *Server) removeProjectVar(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, name string) (bool, error) {
	tag, err := tx.Exec(r.Context(), `DELETE FROM project_variables WHERE project_id = $1 AND name = $2`, p.ID, name)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	s.audit(r, tx, o.ID, "project_variable.deleted", "project:"+p.Slug, map[string]string{"name": name})
	return true, nil
}

// redeployProjectServices starts a new deployment, with the same settings, of every
// service in the project (previews included) whose current deployment is live, so they
// pick up changed project variables. Returns how many were started.
func (s *Server) redeployProjectServices(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo) (int, error) {
	if err := s.checkDisk(r, tx, o); err != nil {
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
		if _, err := s.createDeployment(r, tx, o, l.serviceID, spec); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		s.audit(r, tx, o.ID, "project.redeployed", "project:"+p.Slug, map[string]int{"services": n})
	}
	return n, nil
}

// ---------- console ----------

// withProject runs fn in the organization's tenant context with the project loaded.
func (s *Server) withProject(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *orgInfo, p *projectInfo) error) (*orgInfo, *projectInfo, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var p *projectInfo
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.loadProject(r, tx); err != nil {
			return err
		}
		return fn(tx, o, p)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, false
	}
	return o, p, err, true
}

func (s *Server) setProjectVar(w http.ResponseWriter, r *http.Request) {
	var formErr string
	redeployed := -1
	o, p, err, ok := s.withProject(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo) error {
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
			var qe quotaError
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
		s.serverError(w, r, err)
		return
	}
	path := o.Path() + "/p/" + p.Slug + "#variables"
	switch {
	case formErr != "":
		back(w, r, path, "", formErr)
	case redeployed >= 0:
		back(w, r, path, "Variable saved. Redeploying "+itoa(redeployed)+" service(s).", "")
	default:
		back(w, r, path, "Variable saved. Redeploy the services to apply it.", "")
	}
}

func (s *Server) deleteProjectVar(w http.ResponseWriter, r *http.Request) {
	o, p, err, ok := s.withProject(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo) error {
		_, err := s.removeProjectVar(r, tx, o, p, r.PathValue("name"))
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/p/"+p.Slug+"#variables", "Variable deleted. Redeploy the services to apply it.", "")
}

func (s *Server) redeployProject(w http.ResponseWriter, r *http.Request) {
	var formErr string
	n := 0
	o, p, err, ok := s.withProject(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo) error {
		var err error
		n, err = s.redeployProjectServices(r, tx, o, p)
		var qe quotaError
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
		s.serverError(w, r, err)
		return
	}
	path := o.Path() + "/p/" + p.Slug + "#variables"
	switch {
	case formErr != "":
		back(w, r, path, "", formErr)
	case n == 0:
		back(w, r, path, "No running service to redeploy.", "")
	default:
		back(w, r, path, "Redeploying "+itoa(n)+" service(s).", "")
	}
}

// ---------- API ----------

func (s *Server) apiProject(w http.ResponseWriter, r *http.Request, o *orgInfo, fn func(tx pgx.Tx, p *projectInfo) error) bool {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		return fn(tx, p)
	})
	var ae apiErr
	switch {
	case isNoRows(err):
		apiError(w, http.StatusNotFound, "project not found")
	case errors.As(err, &ae):
		apiError(w, ae.status, ae.msg)
	case err != nil:
		s.log.Error("api", "path", r.URL.Path, "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	default:
		return true
	}
	return false
}

func (s *Server) apiProjectVars(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var vars []projectVar
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *projectInfo) error {
		var err error
		vars, err = s.loadProjectVars(r, tx, p.ID)
		return err
	}) {
		return
	}
	if vars == nil {
		vars = []projectVar{}
	}
	writeJSON(w, vars)
}

func (s *Server) apiSetProjectVar(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	name := r.PathValue("name")
	var body struct {
		Value  string `json:"value"`
		Secret *bool  `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "body must be JSON: {\"value\": \"...\", \"secret\": true}")
		return
	}
	if msg := checkProjectVar(name, body.Value); msg != "" {
		apiError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	secret := body.Secret == nil || *body.Secret // secret unless said otherwise
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *projectInfo) error {
		return s.saveProjectVar(r, tx, o, p, name, body.Value, secret)
	}) {
		return
	}
	writeJSON(w, map[string]any{"name": name, "secret": secret, "status": "saved; redeploy the services to apply it"})
}

func (s *Server) apiDeleteProjectVar(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *projectInfo) error {
		found, err := s.removeProjectVar(r, tx, o, p, r.PathValue("name"))
		if err == nil && !found {
			return apiErr{http.StatusNotFound, "variable not found"}
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiRedeployProject(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	n := 0
	if !s.apiProject(w, r, o, func(tx pgx.Tx, p *projectInfo) error {
		var err error
		n, err = s.redeployProjectServices(r, tx, o, p)
		var qe quotaError
		if errors.As(err, &qe) {
			return apiErr{http.StatusInsufficientStorage, qe.Error()}
		}
		return err
	}) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]int{"redeploying": n})
}
