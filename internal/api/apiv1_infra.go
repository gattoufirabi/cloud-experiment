package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
)

// API v1 for infrastructure as code (the Terraform provider, scripts): create, read,
// change and delete projects, applications, managed databases and custom domains. Each
// write goes through the same checks as the console (shared functions), and each read
// returns every field a write accepts, so a tool can compare and detect drift.

func (s *Server) apiInfraRoutes(m *http.ServeMux) {
	const proj = "/api/v1/projects/{project}"
	const svc = proj + "/services/{service}"
	const dbp = proj + "/databases/{db}"
	m.HandleFunc("POST /api/v1/projects", s.apiAuth("admin", s.apiCreateProject))
	m.HandleFunc("GET "+proj, s.apiAuth("viewer", s.apiProject1))
	m.HandleFunc("PATCH "+proj, s.apiAuth("admin", s.apiUpdateProject))
	m.HandleFunc("DELETE "+proj, s.apiAuth("admin", s.apiDeleteProject))
	m.HandleFunc("POST "+proj+"/services", s.apiAuth("developer", s.apiCreateService))
	m.HandleFunc("PATCH "+svc, s.apiAuth("developer", s.apiUpdateService))
	m.HandleFunc("DELETE "+svc, s.apiAuth("developer", s.apiDeleteService))
	m.HandleFunc("GET "+svc+"/secrets", s.apiAuth("viewer", s.apiServiceSecrets))
	m.HandleFunc("DELETE "+svc+"/secrets/{name}", s.apiAuth("developer", s.apiDeleteServiceSecret))
	m.HandleFunc("GET "+svc+"/domains", s.apiAuth("viewer", s.apiDomains))
	m.HandleFunc("POST "+svc+"/domains", s.apiAuth("developer", s.apiAddDomain))
	m.HandleFunc("GET "+svc+"/domains/{hostname}", s.apiAuth("viewer", s.apiDomain))
	m.HandleFunc("DELETE "+svc+"/domains/{hostname}", s.apiAuth("developer", s.apiDeleteDomain))
	m.HandleFunc("GET "+proj+"/databases", s.apiAuth("viewer", s.apiDatabases))
	m.HandleFunc("POST "+proj+"/databases", s.apiAuth("developer", s.apiCreateDatabase))
	m.HandleFunc("GET "+dbp, s.apiAuth("viewer", s.apiDatabase))
	m.HandleFunc("PATCH "+dbp, s.apiAuth("developer", s.apiUpdateDatabase))
	m.HandleFunc("DELETE "+dbp, s.apiAuth("admin", s.apiDeleteDatabase))
}

// apiTx runs fn in the organization's transaction and writes the error, if any:
// not found (404), a user error (apiErr) or an internal error (500).
func (s *Server) apiTx(w http.ResponseWriter, r *http.Request, o *orgInfo, notFound string, fn func(tx pgx.Tx) error) bool {
	err := db.WithOrg(r.Context(), s.pool, o.ID, fn)
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		apiError(w, ae.status, ae.msg)
	case isNoRows(err):
		apiError(w, http.StatusNotFound, notFound)
	case err != nil:
		s.log.Error("api", "path", r.URL.Path, "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	default:
		return true
	}
	return false
}

// decodeObject reads a JSON object and refuses fields not in allowed, so a typo is an
// error instead of being ignored.
func decodeObject(r *http.Request, allowed ...string) (map[string]json.RawMessage, error) {
	var in map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil && err != io.EOF {
		return nil, apiErr{http.StatusBadRequest, "the body must be a JSON object"}
	}
	if in == nil {
		in = map[string]json.RawMessage{}
	}
	for k := range in {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return nil, apiErr{http.StatusBadRequest, "unknown field " + strconv.Quote(k) + "; accepted: " + strings.Join(allowed, ", ")}
		}
	}
	return in, nil
}

func field[T any](in map[string]json.RawMessage, name string, dst *T) (bool, error) {
	raw, ok := in[name]
	if !ok || string(raw) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return false, apiErr{http.StatusBadRequest, "field " + strconv.Quote(name) + " has the wrong type"}
	}
	return true, nil
}

// idField reads "id" (or "slug", the console form's name, kept for older scripts).
func idField(in map[string]json.RawMessage) (string, error) {
	var id string
	if _, err := field(in, "id", &id); err != nil || id != "" {
		return id, err
	}
	_, err := field(in, "slug", &id)
	return id, err
}

func created(w http.ResponseWriter, location string, v any) {
	w.Header().Set("Location", location)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- projects ----------

type apiProjectView struct {
	Slug     string `json:"id"`
	Name     string `json:"name"`
	Services int    `json:"services"`
}

func loadAPIProject(r *http.Request, tx pgx.Tx, slug string) (apiProjectView, error) {
	var v apiProjectView
	err := tx.QueryRow(r.Context(), `
		SELECT p.slug, p.name, (SELECT count(*) FROM services s WHERE s.project_id = p.id AND s.preview_of IS NULL)
		FROM projects p WHERE p.slug = $1`, slug).Scan(&v.Slug, &v.Name, &v.Services)
	return v, err
}

func (s *Server) apiCreateProject(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var slug, name string
	var view apiProjectView
	if !s.apiTx(w, r, o, "", func(tx pgx.Tx) error {
		in, err := decodeObject(r, "id", "slug", "name")
		if err != nil {
			return err
		}
		if slug, err = idField(in); err != nil {
			return err
		}
		if _, err := field(in, "name", &name); err != nil {
			return err
		}
		if name = strings.TrimSpace(name); name == "" {
			name = slug
		}
		if !model.ValidSlug(slug) || len(name) > 100 {
			return apiErr{http.StatusUnprocessableEntity, "id: 2 to 20 characters, lowercase letters, digits and dashes; name: up to 100 characters"}
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $3)`, o.ID, slug, name); err != nil {
			if isUniqueViolation(err) {
				return apiErr{http.StatusConflict, "a project with this ID already exists"}
			}
			return err
		}
		s.audit(r, tx, o.ID, "project.created", "project:"+slug, nil)
		view, err = loadAPIProject(r, tx, slug)
		return err
	}) {
		return
	}
	created(w, "/api/v1/projects/"+slug, struct {
		apiProjectView
		Message string `json:"message"`
	}{view, "Project created."})
}

func (s *Server) apiProject1(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var view apiProjectView
	if s.apiTx(w, r, o, "project not found", func(tx pgx.Tx) (err error) {
		view, err = loadAPIProject(r, tx, r.PathValue("project"))
		return err
	}) {
		writeJSON(w, view)
	}
}

func (s *Server) apiUpdateProject(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var view apiProjectView
	if s.apiTx(w, r, o, "project not found", func(tx pgx.Tx) error {
		in, err := decodeObject(r, "name")
		if err != nil {
			return err
		}
		var name string
		if ok, err := field(in, "name", &name); err != nil || !ok {
			if err == nil {
				err = apiErr{http.StatusBadRequest, `give the new name: {"name": "..."}`}
			}
			return err
		}
		if name = strings.TrimSpace(name); name == "" || len(name) > 100 {
			return apiErr{http.StatusUnprocessableEntity, "name: 1 to 100 characters"}
		}
		tag, err := tx.Exec(r.Context(), `UPDATE projects SET name = $2 WHERE slug = $1`, r.PathValue("project"), name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		s.audit(r, tx, o.ID, "project.renamed", "project:"+r.PathValue("project"), map[string]string{"name": name})
		view, err = loadAPIProject(r, tx, r.PathValue("project"))
		return err
	}) {
		writeJSON(w, view)
	}
}

// apiDeleteProject deletes a project with everything in it (applications, databases,
// domains). It is refused while the project still has applications or databases unless
// ?force=true is given, so a mistyped ID cannot wipe a project by accident.
func (s *Server) apiDeleteProject(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	slug := r.PathValue("project")
	if s.apiTx(w, r, o, "project not found", func(tx pgx.Tx) error {
		var id string
		var n int
		if err := tx.QueryRow(r.Context(), `
			SELECT p.id, (SELECT count(*) FROM services s WHERE s.project_id = p.id) + (SELECT count(*) FROM databases d WHERE d.project_id = p.id)
			FROM projects p WHERE p.slug = $1`, slug).Scan(&id, &n); err != nil {
			return err
		}
		if n > 0 && r.URL.Query().Get("force") != "true" {
			return apiErr{http.StatusConflict, "the project still has " + itoa(n) + " applications or databases; delete them first, or add ?force=true to delete everything"}
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM projects WHERE id = $1`, id); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "project.deleted", "project:"+slug, nil)
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- applications (services) ----------

type apiAutoscaling struct {
	Min        int `json:"min"`
	Max        int `json:"max"`
	CPUPercent int `json:"cpu_percent"`
}

type apiServiceView struct {
	Slug           string              `json:"id"`
	Project        string              `json:"project"`
	Image          string              `json:"image"`
	Port           int                 `json:"port"`
	HealthPath     string              `json:"health_path"`
	CPUMillis      int                 `json:"cpu_millis"`
	MemoryMB       int                 `json:"memory_mb"`
	VolumePath     string              `json:"volume_path"`
	Instances      int                 `json:"instances"`
	Worker         bool                `json:"worker"`
	Env            map[string]string   `json:"env"`
	PublicPorts    []string            `json:"public_ports"`
	Autoscaling    *apiAutoscaling     `json:"autoscaling"`
	MetricsPath    string              `json:"metrics_path"`
	MetricsPort    int                 `json:"metrics_port"`
	EdgeCache      bool                `json:"edge_cache"`
	Public         bool                `json:"public"`
	DefaultAddress bool                `json:"default_address"`
	InternalHost   string              `json:"internal_host"`
	URL            string              `json:"url"`
	Status         string              `json:"status"`
	Revision       int                 `json:"revision"`
	Deployments    []apiDeploymentView `json:"deployments,omitempty"`
}

func (s *Server) apiServiceViewOf(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) (apiServiceView, error) {
	v := apiServiceView{
		Slug: sv.Slug, Project: p.Slug, Image: sv.Image, Port: sv.Port, HealthPath: sv.HealthPath,
		CPUMillis: sv.CPUMillis, MemoryMB: sv.MemoryMB, VolumePath: sv.VolumePath, Instances: sv.Instances,
		Worker: sv.Worker, Env: sv.Env, PublicPorts: []string{}, MetricsPath: sv.MetricsPath, MetricsPort: sv.MetricsPort,
		EdgeCache: sv.EdgeCache, Public: sv.Public, DefaultAddress: sv.DefaultAddress,
		InternalHost: sv.Slug + "." + p.Slug + ".internal", Status: "not deployed",
	}
	for _, pp := range sv.Ports {
		v.PublicPorts = append(v.PublicPorts, pp.Key())
	}
	sort.Strings(v.PublicPorts)
	if sv.AutoMin > 0 {
		v.Autoscaling = &apiAutoscaling{sv.AutoMin, sv.AutoMax, sv.AutoCPU}
	}
	if !sv.Worker {
		v.URL = s.appURL(s.serviceHost(o, p, sv))
		if !sv.DefaultAddress && !sv.isPreview() {
			var host string
			if tx.QueryRow(r.Context(), `SELECT hostname FROM domains WHERE service_id = $1 AND verified_at IS NOT NULL ORDER BY hostname LIMIT 1`, sv.ID).Scan(&host) == nil {
				v.URL = "https://" + host
			}
		}
	}
	err := tx.QueryRow(r.Context(), `SELECT revision, status FROM deployments WHERE service_id = $1 ORDER BY revision DESC LIMIT 1`, sv.ID).Scan(&v.Revision, &v.Status)
	if isNoRows(err) {
		err = nil
	}
	return v, err
}

// serviceForm turns a service's current settings into the console form's fields, so
// a partial API change keeps every setting it does not name.
func serviceForm(sv *serviceInfo) url.Values {
	f := url.Values{}
	f.Set("image", sv.Image)
	f.Set("port", itoa(sv.Port))
	f.Set("cpu_millis", itoa(sv.CPUMillis))
	f.Set("memory_mb", itoa(sv.MemoryMB))
	f.Set("health_path", sv.HealthPath)
	f.Set("volume_path", sv.VolumePath)
	f.Set("env", model.FormatEnv(sv.Env))
	f.Set("ports", model.PortsText(sv.Ports))
	f.Set("instances", itoa(max(sv.Instances, 1)))
	if sv.Worker {
		f.Set("kind", "worker")
	}
	if sv.AutoMin > 0 {
		f.Set("autoscale", "1")
		f.Set("autoscale_min", itoa(sv.AutoMin))
		f.Set("autoscale_max", itoa(sv.AutoMax))
	}
	f.Set("autoscale_cpu", itoa(sv.AutoCPU))
	f.Set("metrics_path", sv.MetricsPath)
	if sv.MetricsPort > 0 {
		f.Set("metrics_port", itoa(sv.MetricsPort))
	}
	if sv.EdgeCache {
		f.Set("edge_cache", "1")
	}
	if !sv.Public {
		f.Set("private", "1")
	}
	return f
}

var serviceFields = []string{"image", "port", "cpu_millis", "memory_mb", "health_path", "volume_path", "instances", "worker",
	"env", "public_ports", "autoscaling", "metrics_path", "metrics_port", "edge_cache", "public", "deploy"}

// applyServiceJSON lays the JSON fields given over the form values f.
func applyServiceJSON(in map[string]json.RawMessage, f url.Values) (deploy bool, err error) {
	for _, k := range []string{"image", "health_path", "volume_path", "metrics_path"} {
		var v string
		if _, err := field(in, k, &v); err != nil {
			return false, err
		}
		if _, ok := in[k]; ok {
			f.Set(k, v)
		}
	}
	for _, k := range []string{"port", "cpu_millis", "memory_mb", "instances", "metrics_port"} {
		var v int
		ok, err := field(in, k, &v)
		if err != nil {
			return false, err
		}
		if ok {
			f.Set(k, itoa(v))
		}
	}
	if raw, ok := in["metrics_port"]; ok && (string(raw) == "null" || string(raw) == "0") {
		f.Del("metrics_port")
	}
	bools := map[string]func(bool){
		"worker": func(b bool) {
			if b {
				f.Set("kind", "worker")
			} else {
				f.Del("kind")
			}
		},
		"edge_cache": func(b bool) { setFlag(f, "edge_cache", b) },
		"public":     func(b bool) { setFlag(f, "private", !b) },
		"deploy":     func(b bool) { deploy = b },
	}
	for k, set := range bools {
		var b bool
		ok, err := field(in, k, &b)
		if err != nil {
			return false, err
		}
		if ok {
			set(b)
		}
	}
	if _, ok := in["env"]; ok {
		env := map[string]string{}
		if _, err := field(in, "env", &env); err != nil {
			return false, err
		}
		for k, v := range env {
			if strings.ContainsAny(k, "=\n\r") || strings.TrimSpace(k) != k || k == "" || strings.ContainsAny(v, "\n\r") {
				return false, apiErr{http.StatusUnprocessableEntity, "env: names without '=', spaces or line breaks, values on one line (" + strconv.Quote(k) + ")"}
			}
		}
		f.Set("env", model.FormatEnv(env))
	}
	if _, ok := in["public_ports"]; ok {
		var ports []string
		if _, err := field(in, "public_ports", &ports); err != nil {
			return false, err
		}
		f.Set("ports", strings.Join(ports, "\n"))
	}
	if raw, ok := in["autoscaling"]; ok {
		if string(raw) == "null" {
			f.Del("autoscale")
		} else {
			var a apiAutoscaling
			if err := json.Unmarshal(raw, &a); err != nil {
				return false, apiErr{http.StatusBadRequest, `autoscaling: {"min": 1, "max": 5, "cpu_percent": 70}, or null to turn it off`}
			}
			if a.CPUPercent == 0 {
				a.CPUPercent = 70
			}
			f.Set("autoscale", "1")
			f.Set("autoscale_min", itoa(a.Min))
			f.Set("autoscale_max", itoa(a.Max))
			f.Set("autoscale_cpu", itoa(a.CPUPercent))
		}
	}
	return deploy, nil
}

func setFlag(f url.Values, k string, on bool) {
	if on {
		f.Set(k, "1")
	} else {
		f.Del(k)
	}
}

// saveServiceJSON applies a JSON change to a service and returns its new view.
func (s *Server) saveServiceJSON(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, in map[string]json.RawMessage) (apiServiceView, error) {
	f := serviceForm(sv)
	deploy, err := applyServiceJSON(in, f)
	if err != nil {
		return apiServiceView{}, err
	}
	if deploy && f.Get("image") == "" {
		return apiServiceView{}, apiErr{http.StatusUnprocessableEntity, "give an image to deploy"}
	}
	formErr, _, err := s.applyServiceSettings(r, tx, o, p, sv, f, deploy)
	if err != nil {
		return apiServiceView{}, err
	}
	if formErr != "" {
		return apiServiceView{}, apiErr{http.StatusUnprocessableEntity, formErr}
	}
	if sv, err = s.loadService(r, tx, p.ID); err != nil {
		return apiServiceView{}, err
	}
	return s.apiServiceViewOf(r, tx, o, p, sv)
}

// apiCreateService creates an application with its settings, and deploys it when
// "deploy" is true: POST {"id": "web", "image": "nginx:1.27-alpine", "port": 80, "deploy": true}
func (s *Server) apiCreateService(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var view apiServiceView
	var slug string
	if !s.apiTx(w, r, o, "project not found", func(tx pgx.Tx) error {
		in, err := decodeObject(r, append([]string{"id", "slug"}, serviceFields...)...)
		if err != nil {
			return err
		}
		if slug, err = idField(in); err != nil {
			return err
		}
		if !model.ValidSlug(slug) {
			return apiErr{http.StatusUnprocessableEntity, "id: 2 to 20 characters, lowercase letters, digits and dashes"}
		}
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		if err := s.checkQuota(r, tx, o, "", 250, 256, true); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				return apiErr{http.StatusUnprocessableEntity, qe.Error()}
			}
			return err
		}
		var image string
		_, _ = field(in, "image", &image)
		if _, err := tx.Exec(r.Context(), `INSERT INTO services (org_id, project_id, slug, image) VALUES ($1, $2, $3, $4)`,
			o.ID, p.ID, slug, strings.TrimSpace(image)); err != nil {
			if isUniqueViolation(err) {
				return apiErr{http.StatusConflict, "an application with this ID already exists in the project"}
			}
			return err
		}
		s.audit(r, tx, o.ID, "service.created", "service:"+p.Slug+"/"+slug, nil)
		r.SetPathValue("service", slug)
		sv, err := s.loadService(r, tx, p.ID)
		if err != nil {
			return err
		}
		delete(in, "id")
		delete(in, "slug")
		delete(in, "image")
		if len(in) == 0 { // only an ID and an image: settings come later (as in the console)
			view, err = s.apiServiceViewOf(r, tx, o, p, sv)
			return err
		}
		in["image"], _ = json.Marshal(image)
		view, err = s.saveServiceJSON(r, tx, o, p, sv, in)
		return err
	}) {
		return
	}
	created(w, "/api/v1/projects/"+view.Project+"/services/"+slug, struct {
		apiServiceView
		Message string `json:"message"`
	}{view, "Service created."})
}

func (s *Server) apiUpdateService(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var view apiServiceView
	in, err := decodeObject(r, serviceFields...)
	if err != nil {
		var ae apiErr
		errors.As(err, &ae)
		apiError(w, ae.status, ae.msg)
		return
	}
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) (err error) {
		if sv.isPreview() {
			return apiErr{http.StatusUnprocessableEntity, "previews follow their application's settings"}
		}
		view, err = s.saveServiceJSON(r, tx, o, p, sv, in)
		return err
	}) {
		writeJSON(w, view)
	}
}

func (s *Server) apiDeleteService(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		if _, err := tx.Exec(r.Context(), `DELETE FROM services WHERE id = $1`, sv.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "service.deleted", "service:"+p.Slug+"/"+sv.Slug, nil)
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiServiceSecrets lists the names of an application's secrets (never their values).
func (s *Server) apiServiceSecrets(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	type secret struct {
		Name    string `json:"name"`
		Source  string `json:"source"`
		Updated string `json:"updated_at"`
	}
	out := []secret{}
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		rows, err := tx.Query(r.Context(), `SELECT name, source, to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM secrets WHERE service_id = $1 ORDER BY name`, sv.ID)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[secret])
		out = append(out, list...)
		return err
	}) {
		writeJSON(w, out)
	}
}

func (s *Server) apiDeleteServiceSecret(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	name := r.PathValue("name")
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM secrets WHERE service_id = $1 AND name = $2`, sv.ID, name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apiErr{http.StatusNotFound, "secret not found"}
		}
		s.audit(r, tx, o.ID, "secret.deleted", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"name": name})
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- custom domains ----------

type apiDNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type apiDomainView struct {
	Hostname string         `json:"hostname"`
	Verified bool           `json:"verified"`
	Records  []apiDNSRecord `json:"dns_records"`
}

func (s *Server) loadAPIDomains(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, host string) ([]apiDomainView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT hostname, verified_at IS NOT NULL, verify_token FROM domains
		WHERE service_id = $1 AND ($2 = '' OR hostname = $2) ORDER BY hostname`, sv.ID, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	target := s.serviceHost(o, p, sv)
	out := []apiDomainView{}
	for rows.Next() {
		var d apiDomainView
		var token string
		if err := rows.Scan(&d.Hostname, &d.Verified, &token); err != nil {
			return nil, err
		}
		d.Records = []apiDNSRecord{
			{"CNAME", d.Hostname, target},
			{"TXT", challengePrefix + d.Hostname, "platform-verify=" + token},
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) apiDomains(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []apiDomainView
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) (err error) {
		out, err = s.loadAPIDomains(r, tx, o, p, sv, "")
		return err
	}) {
		writeJSON(w, out)
	}
}

func (s *Server) apiDomain(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []apiDomainView
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) (err error) {
		out, err = s.loadAPIDomains(r, tx, o, p, sv, strings.ToLower(r.PathValue("hostname")))
		return err
	}) {
		return
	}
	if len(out) == 0 {
		apiError(w, http.StatusNotFound, "domain not found")
		return
	}
	writeJSON(w, out[0])
}

// apiAddDomain: POST {"hostname": "shop.example.com"}. The answer lists the DNS records
// to create; in a zone hosted on the platform they are created, and the domain verified.
func (s *Server) apiAddDomain(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"hostname": "shop.example.com"}`)
		return
	}
	var out []apiDomainView
	var path string
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		formErr, _, _, err := s.addDomainTx(r, tx, o, p, sv, body.Hostname)
		if err != nil {
			return err
		}
		if formErr != "" {
			status := http.StatusUnprocessableEntity
			if strings.Contains(formErr, "already registered") {
				status = http.StatusConflict
			}
			return apiErr{status, formErr}
		}
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(body.Hostname)), ".")
		path = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/domains/" + host
		out, err = s.loadAPIDomains(r, tx, o, p, sv, host)
		return err
	}) {
		return
	}
	if len(out) == 0 {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	created(w, path, struct {
		apiDomainView
		Message string `json:"message"`
	}{out[0], "Domain added."})
}

func (s *Server) apiDeleteDomain(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		var id string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM domains WHERE service_id = $1 AND hostname = $2`,
			sv.ID, strings.ToLower(r.PathValue("hostname"))).Scan(&id); err != nil {
			if isNoRows(err) {
				return apiErr{http.StatusNotFound, "domain not found"}
			}
			return err
		}
		return s.deleteDomainTx(r, tx, o, sv, id)
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- managed databases ----------

type apiDatabaseView struct {
	Slug          string `json:"id"`
	Project       string `json:"project"`
	Engine        string `json:"engine"`
	Version       string `json:"version"`
	Status        string `json:"status"`
	CPUMillis     int    `json:"cpu_millis"`
	MemoryMB      int    `json:"memory_mb"`
	BackupEnabled bool   `json:"backup_enabled"`
	BackupHour    int    `json:"backup_hour"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Database      string `json:"database"`
	Username      string `json:"username"`
	SecretName    string `json:"secret_name"`
	Password      string `json:"password,omitempty"`
	URL           string `json:"url,omitempty"`
}

// apiDatabaseViewOf: the password and connection URL only go to developers and admins,
// as in the console.
func (s *Server) apiDatabaseViewOf(o *orgInfo, p *projectInfo, d *databaseInfo) apiDatabaseView {
	v := apiDatabaseView{
		Slug: d.Slug, Project: p.Slug, Engine: d.Engine, Version: d.Version, Status: d.Status,
		CPUMillis: d.CPUMillis, MemoryMB: d.MemoryMB, BackupEnabled: d.BackupEnabled, BackupHour: d.BackupHour,
		Host: d.Host(), Port: d.Port(), Database: d.DBName, Username: d.Username, SecretName: d.DefaultSecret(),
	}
	if model.RoleAtLeast(o.Role, "developer") {
		v.Password, v.URL = s.dbPassword(d), s.dbURL(d)
	}
	return v
}

func (s *Server) apiDatabases(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	out := []apiDatabaseView{}
	if s.apiTx(w, r, o, "project not found", func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT slug FROM databases WHERE project_id = $1 ORDER BY slug`, p.ID)
		if err != nil {
			return err
		}
		slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, slug := range slugs {
			r.SetPathValue("db", slug)
			d, err := s.loadDatabase(r, tx, p.ID)
			if err != nil {
				return err
			}
			out = append(out, s.apiDatabaseViewOf(o, p, d))
		}
		return nil
	}) {
		writeJSON(w, out)
	}
}

func (s *Server) apiDatabaseTx(w http.ResponseWriter, r *http.Request, o *orgInfo, fn func(tx pgx.Tx, p *projectInfo, d *databaseInfo) error) bool {
	return s.apiTx(w, r, o, "project or database not found", func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		d, err := s.loadDatabase(r, tx, p.ID)
		if err != nil {
			return err
		}
		return fn(tx, p, d)
	})
}

func (s *Server) apiDatabase(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var view apiDatabaseView
	if s.apiDatabaseTx(w, r, o, func(tx pgx.Tx, p *projectInfo, d *databaseInfo) error {
		view = s.apiDatabaseViewOf(o, p, d)
		return nil
	}) {
		writeJSON(w, view)
	}
}

// updateDatabaseJSON applies the resource and backup fields given, keeping the others.
func (s *Server) updateDatabaseJSON(r *http.Request, tx pgx.Tx, o *orgInfo, d *databaseInfo, in map[string]json.RawMessage) error {
	cpu, mem, hour, enabled := d.CPUMillis, d.MemoryMB, d.BackupHour, d.BackupEnabled
	for k, dst := range map[string]*int{"cpu_millis": &cpu, "memory_mb": &mem, "backup_hour": &hour} {
		if _, err := field(in, k, dst); err != nil {
			return err
		}
	}
	if _, err := field(in, "backup_enabled", &enabled); err != nil {
		return err
	}
	if hour < 0 || hour > 23 {
		return apiErr{http.StatusUnprocessableEntity, "backup_hour: from 0 to 23 (UTC)"}
	}
	if cpu == d.CPUMillis && mem == d.MemoryMB && hour == d.BackupHour && enabled == d.BackupEnabled {
		return nil
	}
	formErr, err := s.databaseSettingsTx(r, tx, o, d, cpu, mem, hour, enabled)
	if err == nil && formErr != "" {
		err = apiErr{http.StatusUnprocessableEntity, formErr}
	}
	return err
}

var databaseFields = []string{"cpu_millis", "memory_mb", "backup_enabled", "backup_hour"}

// apiCreateDatabase: POST {"id": "main", "engine": "postgres:17", "memory_mb": 512}.
// engine is one of the console's choices ("postgres", "postgres:17", "redis:7"...).
func (s *Server) apiCreateDatabase(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var view apiDatabaseView
	var path string
	if !s.apiTx(w, r, o, "project not found", func(tx pgx.Tx) error {
		in, err := decodeObject(r, append([]string{"id", "slug", "engine"}, databaseFields...)...)
		if err != nil {
			return err
		}
		var engine string
		slug, err := idField(in)
		if err != nil {
			return err
		}
		if _, err := field(in, "engine", &engine); err != nil {
			return err
		}
		if !model.ValidSlug(slug) {
			return apiErr{http.StatusUnprocessableEntity, "id: 2 to 20 characters, lowercase letters, digits and dashes"}
		}
		eng, found := model.FindEngine(engine)
		for _, e := range model.Engines { // an engine without a version: its newest one
			if !found && e.ID == engine {
				eng, found = e, true
			}
		}
		if !found {
			var names []string
			for _, e := range model.Engines {
				names = append(names, e.ID+":"+e.Version)
			}
			return apiErr{http.StatusUnprocessableEntity, "engine: one of " + strings.Join(names, ", ")}
		}
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		formErr, err := s.createDatabaseTx(r, tx, o, p, slug, eng)
		if formErr != "" {
			status := http.StatusUnprocessableEntity
			if strings.Contains(formErr, "already exists") {
				status = http.StatusConflict
			}
			return apiErr{status, formErr}
		}
		if err != nil {
			return err
		}
		r.SetPathValue("db", slug)
		d, err := s.loadDatabase(r, tx, p.ID)
		if err != nil {
			return err
		}
		if err := s.updateDatabaseJSON(r, tx, o, d, in); err != nil {
			return err
		}
		if d, err = s.loadDatabase(r, tx, p.ID); err != nil {
			return err
		}
		view = s.apiDatabaseViewOf(o, p, d)
		path = "/api/v1/projects/" + p.Slug + "/databases/" + slug
		return nil
	}) {
		return
	}
	created(w, path, struct {
		apiDatabaseView
		Message string `json:"message"`
	}{view, "Database created. It is starting; this takes under a minute."})
}

func (s *Server) apiUpdateDatabase(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var view apiDatabaseView
	in, err := decodeObject(r, databaseFields...)
	if err != nil {
		var ae apiErr
		errors.As(err, &ae)
		apiError(w, ae.status, ae.msg)
		return
	}
	if s.apiDatabaseTx(w, r, o, func(tx pgx.Tx, p *projectInfo, d *databaseInfo) error {
		if err := s.updateDatabaseJSON(r, tx, o, d, in); err != nil {
			return err
		}
		d, err := s.loadDatabase(r, tx, p.ID)
		if err != nil {
			return err
		}
		view = s.apiDatabaseViewOf(o, p, d)
		return nil
	}) {
		writeJSON(w, view)
	}
}

func (s *Server) apiDeleteDatabase(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiDatabaseTx(w, r, o, func(tx pgx.Tx, p *projectInfo, d *databaseInfo) error {
		if _, err := tx.Exec(r.Context(), `DELETE FROM databases WHERE id = $1`, d.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "database.deleted", "database:"+p.Slug+"/"+d.Slug, nil)
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
