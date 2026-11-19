package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/registry"
	"platform/internal/secure"
)

// REST API v1, for CI pipelines and the platformctl tool. Authentication is a bearer
// API token ("pla_…") created by an organization admin. Every request is limited to the
// token's organization (database row-level security, as in the console) and to the
// token's role. Bearer tokens are never sent automatically by browsers, so no CSRF check
// is needed. Changes are audited under the token's creator, with the token named.

const apiTokenPrefix = "pla_"

const apiTokenKey ctxKey = 2

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// apiAuth resolves the bearer token and enforces the minimum role.
type queueScopeKeyT struct{}

// apiQueueScopeKey holds the queue a token is limited to ("" for organization tokens).
var apiQueueScopeKey = queueScopeKeyT{}

func (s *Server) apiAuth(minRole string, h func(http.ResponseWriter, *http.Request, *orgInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ip := clientIP(r)
		if !s.limiter.allow("api:"+ip, 600, time.Minute) {
			apiError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || !strings.HasPrefix(tok, apiTokenPrefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="platform"`)
			apiError(w, http.StatusUnauthorized, "missing or malformed API token (Authorization: Bearer pla_...)")
			return
		}
		var o orgInfo
		var name, role string
		var createdBy, queueScope, projectScope *string
		var emailOnly bool
		err := s.pool.QueryRow(r.Context(), `
			SELECT token_name, role, created_by::text, org_id, org_slug, org_name, plan_id, scan_policy, queue_id::text, email_only, project_slug FROM api_token_check($1)`,
			secure.HashToken(tok)).Scan(&name, &role, &createdBy, &o.ID, &o.Slug, &o.Name, &o.PlanID, &o.ScanPolicy, &queueScope, &emailOnly, &projectScope)
		if err != nil {
			if !s.limiter.allow("api-fail:"+ip, 30, 10*time.Minute) {
				apiError(w, http.StatusTooManyRequests, "too many failed attempts")
				return
			}
			apiError(w, http.StatusUnauthorized, "invalid, expired or revoked API token")
			return
		}
		o.Role = role
		if !model.RoleAtLeast(role, minRole) {
			apiError(w, http.StatusForbidden, "this token's role ("+role+") cannot do this; it needs "+minRole)
			return
		}
		s.loadFeaturesOff(r.Context(), &o)
		if area := apiArea(r.URL.Path); o.Off[area] {
			apiError(w, http.StatusForbidden, productByID(area).Name+" is turned off for this organization")
			return
		}
		actor := ""
		if createdBy != nil {
			actor = *createdBy
		}
		ctx := context.WithValue(r.Context(), sessKey, &session{UserID: actor})
		ctx = context.WithValue(ctx, apiTokenKey, name)
		if queueScope != nil { // a queue token only sends, receives and deletes messages of its queue
			p := r.URL.Path
			if !strings.HasPrefix(p, "/api/v1/queues/") || !(strings.HasSuffix(p, "/messages") || strings.HasSuffix(p, "/receive") || strings.Contains(p, "/messages/")) {
				apiError(w, http.StatusForbidden, "this token only works for its queue's messages")
				return
			}
			ctx = context.WithValue(ctx, apiQueueScopeKey, *queueScope)
		}
		if projectScope != nil { // a project token only reaches that project
			p := r.URL.Path
			if p != "/api/v1/me" && p != "/api/v1/projects/"+*projectScope && !strings.HasPrefix(p, "/api/v1/projects/"+*projectScope+"/") {
				apiError(w, http.StatusForbidden, "this token only works for project "+*projectScope)
				return
			}
			o.Scoped, o.Projects = true, map[string]string{*projectScope: role}
		}
		if emailOnly && !(r.Method == http.MethodPost && r.URL.Path == "/api/v1/email/send") &&
			!(r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/email/messages/")) {
			apiError(w, http.StatusForbidden, "this token only sends email")
			return
		}
		h(w, r.WithContext(ctx), &o)
	}
}

// apiService resolves {project} and {service} inside the token's organization.
func (s *Server) apiService(w http.ResponseWriter, r *http.Request, o *orgInfo,
	fn func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error) bool {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		sv, err := s.loadService(r, tx, p.ID)
		if err != nil {
			return err
		}
		return fn(tx, p, sv)
	})
	var ae apiErr
	switch {
	case isNoRows(err):
		apiError(w, http.StatusNotFound, "project or service not found")
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

type apiErr struct {
	status int
	msg    string
}

func (e apiErr) Error() string { return e.msg }

func (s *Server) apiRoutes(m *http.ServeMux) {
	const svc = "/api/v1/projects/{project}/services/{service}"
	m.HandleFunc("GET /api/v1/me", s.apiAuth("viewer", s.apiMe))
	s.apiConsoleRoutes(m)
	m.HandleFunc("GET /api/v1/prometheus/api/v1/{rest...}", s.apiAuth("viewer", s.apiPrometheus))
	m.HandleFunc("POST /api/v1/prometheus/api/v1/{rest...}", s.apiAuth("viewer", s.apiPrometheus))
	m.HandleFunc("GET /api/v1/projects", s.apiAuth("viewer", s.apiProjects))
	m.HandleFunc("GET /api/v1/projects/{project}/services", s.apiAuth("viewer", s.apiServices))
	m.HandleFunc("GET /api/v1/projects/{project}/variables", s.apiAuth("viewer", s.apiProjectVars))
	m.HandleFunc("PUT /api/v1/projects/{project}/variables/{name}", s.apiAuth("developer", s.apiSetProjectVar))
	m.HandleFunc("DELETE /api/v1/projects/{project}/variables/{name}", s.apiAuth("developer", s.apiDeleteProjectVar))
	m.HandleFunc("POST /api/v1/projects/{project}/redeploy", s.apiAuth("developer", s.apiRedeployProject))
	m.HandleFunc("GET /api/v1/projects/{project}/connections", s.apiAuth("viewer", s.apiPeers))
	m.HandleFunc("POST /api/v1/projects/{project}/connections", s.apiAuth("admin", s.apiConnect))
	m.HandleFunc("DELETE /api/v1/projects/{project}/connections/{other}", s.apiAuth("admin", s.apiDisconnect))
	m.HandleFunc("GET /api/v1/projects/{project}/functions", s.apiAuth("viewer", s.apiFunctions))
	m.HandleFunc("PUT /api/v1/projects/{project}/functions/{fn}", s.apiAuth("developer", s.apiPutFunction))
	m.HandleFunc("DELETE /api/v1/projects/{project}/functions/{fn}", s.apiAuth("developer", s.apiDeleteFunction))
	m.HandleFunc("POST /api/v1/projects/{project}/functions/{fn}/invoke", s.apiAuth("developer", s.apiInvoke))
	m.HandleFunc("GET /api/v1/projects/{project}/functions/{fn}/invocations", s.apiAuth("viewer", s.apiInvocations))
	m.HandleFunc("GET /api/v1/dns/zones", s.apiAuth("viewer", s.apiDNSZones))
	m.HandleFunc("POST /api/v1/dns/zones", s.apiAuth("developer", s.apiAddDNSZone))
	m.HandleFunc("GET /api/v1/dns/zones/{zone}/records", s.apiAuth("viewer", s.apiDNSRecords))
	m.HandleFunc("POST /api/v1/dns/zones/{zone}/records", s.apiAuth("developer", s.apiAddDNSRecord))
	m.HandleFunc("POST /api/v1/dns/zones/{zone}/import", s.apiAuth("developer", s.apiImportZone))
	m.HandleFunc("DELETE /api/v1/dns/zones/{zone}/records/{id}", s.apiAuth("developer", s.apiDeleteDNSRecord))
	m.HandleFunc("GET /api/v1/costs", s.apiAuth("admin", s.apiCosts))
	m.HandleFunc("GET /api/v1/budgets", s.apiAuth("admin", s.apiBudgets))
	m.HandleFunc("POST /api/v1/budgets", s.apiAuth("admin", s.apiSaveBudget))
	m.HandleFunc("DELETE /api/v1/budgets/{id}", s.apiAuth("admin", s.apiDeleteBudget))
	m.HandleFunc("GET /api/v1/alarms", s.apiAuth("viewer", s.apiAlarms))
	m.HandleFunc("POST /api/v1/alarms", s.apiAuth("developer", s.apiCreateAlarm))
	m.HandleFunc("DELETE /api/v1/alarms/{id}", s.apiAuth("developer", s.apiDeleteAlarm))
	m.HandleFunc("GET /api/v1/email/domains", s.apiAuth("viewer", s.apiEmailDomains))
	const md = "/api/v1/mail/domains/{domain}"
	m.HandleFunc("GET /api/v1/mail/domains", s.apiAuth("viewer", s.apiMailDomains))
	m.HandleFunc("POST /api/v1/mail/domains", s.apiAuth("developer", s.apiAddMailDomain))
	m.HandleFunc("GET "+md, s.apiAuth("viewer", s.apiMailDomain))
	m.HandleFunc("POST "+md+"/activate", s.apiAuth("developer", s.apiActivateMailDomain))
	m.HandleFunc("DELETE "+md, s.apiAuth("developer", s.apiDeleteMailDomain))
	m.HandleFunc("PUT "+md+"/catch-all", s.apiAuth("developer", s.apiSetCatchAll))
	m.HandleFunc("PUT "+md+"/server-name", s.apiAuth("developer", s.apiSetServerName))
	m.HandleFunc("GET "+md+"/mailboxes", s.apiAuth("viewer", s.apiMailboxes))
	m.HandleFunc("POST "+md+"/mailboxes", s.apiAuth("developer", s.apiAddMailbox))
	m.HandleFunc("PATCH "+md+"/mailboxes/{mailbox}", s.apiAuth("developer", s.apiUpdateMailbox))
	m.HandleFunc("DELETE "+md+"/mailboxes/{mailbox}", s.apiAuth("developer", s.apiDeleteMailbox))
	m.HandleFunc("POST "+md+"/mailboxes/{mailbox}/password", s.apiAuth("developer", s.apiMailboxPassword))
	m.HandleFunc("POST "+md+"/mailboxes/{mailbox}/aliases", s.apiAuth("developer", s.apiAddMailAlias))
	m.HandleFunc("DELETE "+md+"/aliases/{alias}", s.apiAuth("developer", s.apiDeleteMailAlias))
	m.HandleFunc("POST /api/v1/email/domains", s.apiAuth("developer", s.apiAddEmailDomain))
	m.HandleFunc("DELETE /api/v1/email/domains/{domain}", s.apiAuth("developer", s.apiDeleteEmailDomain))
	m.HandleFunc("POST /api/v1/email/send", s.apiAuth("developer", s.apiSendEmail))
	m.HandleFunc("GET /api/v1/email/messages/{id}", s.apiAuth("viewer", s.apiEmailMessage))
	m.HandleFunc("GET /api/v1/email/suppressions", s.apiAuth("viewer", s.apiSuppressions))
	m.HandleFunc("POST /api/v1/email/suppressions", s.apiAuth("developer", s.apiAddSuppression))
	m.HandleFunc("DELETE /api/v1/email/suppressions/{address}", s.apiAuth("developer", s.apiDeleteSuppression))
	m.HandleFunc("GET /api/v1/email/webhook", s.apiAuth("viewer", s.apiEmailWebhook))
	m.HandleFunc("PUT /api/v1/email/webhook", s.apiAuth("developer", s.apiSetEmailWebhook))
	m.HandleFunc("DELETE /api/v1/email/webhook", s.apiAuth("developer", s.apiDeleteEmailWebhook))
	m.HandleFunc("GET /api/v1/secrets", s.apiAuth("viewer", s.apiOrgSecrets))
	m.HandleFunc("POST /api/v1/secrets", s.apiAuth("developer", s.apiCreateOrgSecret))
	m.HandleFunc("GET /api/v1/secrets/{secret}", s.apiAuth("developer", s.apiGetOrgSecret))
	m.HandleFunc("PUT /api/v1/secrets/{secret}", s.apiAuth("developer", s.apiPutOrgSecret))
	m.HandleFunc("DELETE /api/v1/secrets/{secret}", s.apiAuth("developer", s.apiDeleteOrgSecret))
	m.HandleFunc("GET /api/v1/queues", s.apiAuth("viewer", s.apiQueues))
	m.HandleFunc("POST /api/v1/queues", s.apiAuth("developer", s.apiCreateQueue))
	m.HandleFunc("DELETE /api/v1/queues/{queue}", s.apiAuth("admin", s.apiDeleteQueue))
	m.HandleFunc("POST /api/v1/queues/{queue}/messages", s.apiAuth("developer", s.apiSendMessages))
	m.HandleFunc("POST /api/v1/queues/{queue}/receive", s.apiAuth("developer", s.apiReceive))
	m.HandleFunc("DELETE /api/v1/queues/{queue}/messages/{receipt}", s.apiAuth("developer", s.apiDeleteMessage))
	m.HandleFunc("POST /api/v1/queues/{queue}/purge", s.apiAuth("developer", s.apiPurgeQueue))
	m.HandleFunc("GET /api/v1/buckets", s.apiAuth("viewer", s.apiBuckets))
	m.HandleFunc("POST /api/v1/buckets", s.apiAuth("developer", s.apiCreateBucket))
	m.HandleFunc("DELETE /api/v1/buckets/{bucket}", s.apiAuth("admin", s.apiDeleteBucket))
	m.HandleFunc("GET /api/v1/storage-keys", s.apiAuth("viewer", s.apiStorageKeys))
	m.HandleFunc("POST /api/v1/storage-keys", s.apiAuth("developer", s.apiCreateStorageKey))
	m.HandleFunc("DELETE /api/v1/storage-keys/{id}", s.apiAuth("developer", s.apiDeleteStorageKey))
	m.HandleFunc("GET "+svc, s.apiAuth("viewer", s.apiService1))
	m.HandleFunc("POST "+svc+"/deploy", s.apiAuth("developer", s.apiDeploy))
	m.HandleFunc("POST "+svc+"/scale", s.apiAuth("developer", s.apiScale))
	m.HandleFunc("GET "+svc+"/ports", s.apiAuth("viewer", s.apiServicePorts))
	m.HandleFunc("POST "+svc+"/ports", s.apiAuth("developer", s.apiAddServicePort))
	m.HandleFunc("DELETE "+svc+"/ports/{label}", s.apiAuth("developer", s.apiDeleteServicePort))
	m.HandleFunc("GET "+svc+"/schedules", s.apiAuth("viewer", s.apiSchedules))
	m.HandleFunc("POST "+svc+"/schedules", s.apiAuth("developer", s.apiAddSchedule))
	m.HandleFunc("DELETE "+svc+"/schedules/{id}", s.apiAuth("developer", s.apiDeleteSchedule))
	m.HandleFunc("GET "+svc+"/deployments/{rev}", s.apiAuth("viewer", s.apiDeployment))
	m.HandleFunc("GET "+svc+"/logs", s.apiAuth("viewer", s.apiLogs))
	m.HandleFunc("PUT "+svc+"/secrets/{name}", s.apiAuth("developer", s.apiSetSecret))
	m.HandleFunc("GET "+svc+"/previews", s.apiAuth("viewer", s.apiPreviews))
	m.HandleFunc("GET "+svc+"/jobs", s.apiAuth("viewer", s.apiJobs))
	m.HandleFunc("PUT "+svc+"/jobs/{job}", s.apiAuth("developer", s.apiPutJob))
	m.HandleFunc("DELETE "+svc+"/jobs/{job}", s.apiAuth("developer", s.apiDeleteJob))
	m.HandleFunc("POST "+svc+"/jobs/{job}/run", s.apiAuth("developer", s.apiRunJob))
	m.HandleFunc("GET "+svc+"/jobs/{job}/runs", s.apiAuth("viewer", s.apiJobRuns))
	m.HandleFunc("GET "+svc+"/jobs/{job}/runs/{run}", s.apiAuth("viewer", s.apiJobRun))
	m.HandleFunc("POST "+svc+"/builds", s.apiAuth("developer", s.apiStartBuild))
	m.HandleFunc("POST "+svc+"/zip", s.apiAuth("developer", s.apiZipDeploy))
	m.HandleFunc("GET "+svc+"/analytics", s.apiAuth("viewer", s.apiAnalytics))
	m.HandleFunc("GET "+svc+"/builds/{n}", s.apiAuth("viewer", s.apiBuild))
	m.HandleFunc("POST "+svc+"/previews", s.apiAuth("developer", s.apiCreatePreview))
	m.HandleFunc("DELETE "+svc+"/previews/{name}", s.apiAuth("developer", s.apiDeletePreview))
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	tok, _ := r.Context().Value(apiTokenKey).(string)
	writeJSON(w, map[string]string{"organization": o.Slug, "name": o.Name, "role": o.Role, "token": tok})
}

func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	type project struct {
		Slug     string `json:"id"`
		Name     string `json:"name"`
		Services int    `json:"services"`
	}
	var out []project
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT p.slug, p.name, (SELECT count(*) FROM services s WHERE s.project_id = p.id AND s.preview_of IS NULL) FROM projects p ORDER BY p.slug`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[project])
		return err
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if out == nil {
		out = []project{}
	}
	writeJSON(w, out)
}

func (s *Server) apiServices(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	type service struct {
		Slug   string `json:"id"`
		Image  string `json:"image"`
		Status string `json:"status"`
	}
	var out []service
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
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
	if isNoRows(err) {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if out == nil {
		out = []service{}
	}
	writeJSON(w, out)
}

type apiDeploymentView struct {
	Revision  int       `json:"revision"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	Image     string    `json:"image"`
	By        string    `json:"by"`
	CreatedAt time.Time `json:"created_at"`
}

func toAPIDeployments(in []deploymentView) []apiDeploymentView {
	out := make([]apiDeploymentView, 0, len(in))
	for _, d := range in {
		out = append(out, apiDeploymentView{d.Revision, d.Status, d.Message, d.Image, d.By, d.CreatedAt})
	}
	return out
}

func (s *Server) apiService1(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var resp map[string]any
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		deps, err := recentDeployments(r, tx, sv.ID, 10)
		if err != nil {
			return err
		}
		url := s.appURL(s.serviceHost(o, p, sv))
		if !sv.DefaultAddress && !sv.isPreview() {
			var host string
			if tx.QueryRow(r.Context(), `SELECT hostname FROM domains WHERE service_id = $1 AND verified_at IS NOT NULL ORDER BY hostname LIMIT 1`, sv.ID).Scan(&host) == nil {
				url = "https://" + host
			}
		}
		resp = map[string]any{
			"default_address": sv.DefaultAddress,
			"id":              sv.Slug, "project": p.Slug, "image": sv.Image, "port": sv.Port, "health_path": sv.HealthPath,
			"cpu_millis": sv.CPUMillis, "memory_mb": sv.MemoryMB, "volume_path": sv.VolumePath,
			"instances": sv.Instances, "worker": sv.Worker,
			"url":         url,
			"deployments": toAPIDeployments(deps),
		}
		return nil
	}) {
		return
	}
	writeJSON(w, resp)
}

// apiScale changes the number of instances and deploys: POST {"instances": 3}
func (s *Server) apiScale(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Instances int `json:"instances"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"instances": 3}`)
		return
	}
	var rev int
	var path string
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		if sv.isPreview() {
			return apiErr{http.StatusUnprocessableEntity, "a preview runs a single instance"}
		}
		spec := sv.spec()
		spec.Instances = body.Instances
		if body.Instances < 1 {
			return apiErr{http.StatusUnprocessableEntity, "instances must be between 1 and 10"}
		}
		if err := spec.Validate(); err != nil {
			return apiErr{http.StatusUnprocessableEntity, err.Error()}
		}
		for _, check := range []func() error{
			func() error { return s.checkInstances(r, tx, o, spec.Count()) },
			func() error {
				return s.checkQuota(r, tx, o, sv.ID, spec.CPUMillis*spec.Count(), spec.MemoryMB*spec.Count(), false)
			},
		} {
			if err := check(); err != nil {
				var qe quotaError
				if errors.As(err, &qe) {
					return apiErr{http.StatusUnprocessableEntity, qe.Error()}
				}
				return err
			}
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET instances = $2 WHERE id = $1`, sv.ID, spec.Count()); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "service.scaled", "service:"+p.Slug+"/"+sv.Slug, map[string]int{"instances": spec.Count()})
		var err error
		rev, err = s.createDeployment(r, tx, o, sv.ID, spec)
		path = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/deployments/" + itoa(rev)
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
func (s *Server) apiDeploy(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Image string `json:"image"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
			apiError(w, http.StatusBadRequest, "body must be JSON, for example {\"image\": \"nginx:1.27-alpine\"}")
			return
		}
	}
	var rev int
	var path string
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		spec := sv.spec()
		if img := strings.TrimSpace(body.Image); img != "" {
			spec.Image = img
		}
		if err := spec.Validate(); err != nil {
			return apiErr{http.StatusUnprocessableEntity, err.Error()}
		}
		if err := registry.CheckNamespace(spec.Image, s.cfg.RegistryHost, o.Slug); err != nil {
			return apiErr{http.StatusUnprocessableEntity, err.Error()}
		}
		if err := s.checkDisk(r, tx, o); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				return apiErr{http.StatusInsufficientStorage, qe.Error()}
			}
			return err
		}
		if spec.Image != sv.Image {
			if _, err := tx.Exec(r.Context(), `UPDATE services SET image = $2 WHERE id = $1`, sv.ID, spec.Image); err != nil {
				return err
			}
		}
		var err error
		rev, err = s.createDeployment(r, tx, o, sv.ID, spec)
		path = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/deployments/" + itoa(rev)
		return err
	}) {
		return
	}
	w.Header().Set("Location", path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"revision": rev, "status": "pending", "status_url": path})
}

func (s *Server) apiDeployment(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out apiDeploymentView
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		return tx.QueryRow(r.Context(), `
			SELECT d.revision, d.status, d.message, d.spec->>'image', coalesce(u.email, ''), d.created_at
			FROM deployments d LEFT JOIN users u ON u.id = d.created_by
			WHERE d.service_id = $1 AND d.revision = $2`, sv.ID, atoi(r.PathValue("rev"))).
			Scan(&out.Revision, &out.Status, &out.Message, &out.Image, &out.By, &out.CreatedAt)
	}) {
		return
	}
	writeJSON(w, out)
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var serviceID string
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		serviceID = sv.ID
		return nil
	}) {
		return
	}
	writeJSON(w, s.queryLogs(o.ID, serviceID, r.URL.Query().Get("q")))
}

// apiSetSecret: PUT .../secrets/{NAME} {"value": "..."}; applies at the next deployment.
func (s *Server) apiSetSecret(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	name := r.PathValue("name")
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "body must be JSON: {\"value\": \"...\"}")
		return
	}
	if !model.SecretNameRe.MatchString(name) || strings.HasPrefix(name, "PLATFORM_") {
		apiError(w, http.StatusUnprocessableEntity, "secret names use uppercase letters, digits and underscores")
		return
	}
	if body.Value == "" || len(body.Value) > 8192 {
		apiError(w, http.StatusUnprocessableEntity, "the value must be between 1 and 8192 characters")
		return
	}
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		enc, err := s.box.Seal([]byte(body.Value), "secret:"+sv.ID+":"+name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO secrets (org_id, service_id, name, value_enc) VALUES ($1, $2, $3, $4)
			ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, source = '', created_at = now()`,
			o.ID, sv.ID, name, enc); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "secret.set", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"name": name})
		return nil
	}) {
		return
	}
	writeJSON(w, map[string]string{"name": name, "status": "saved; deploy to apply it"})
}

// ---------- console: API token management (admins) ----------

func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	role := r.PostFormValue("role")
	project := r.PostFormValue("project") // empty: the whole organization
	if project != "" && role == "admin" {
		back(w, r, o.Path()+"/iam#api", "", "A token limited to a project can be viewer or developer.")
		return
	}
	if name == "" || len(name) > 60 || (role != "viewer" && role != "developer" && role != "admin") {
		back(w, r, o.Path()+"/iam#api", "", "Give the token a name (up to 60 characters) and a role.")
		return
	}
	token := apiTokenPrefix + secure.NewToken()
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var projectID any
		if project != "" {
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE slug = $1`, project).Scan(&id); err != nil {
				return apiErr{http.StatusUnprocessableEntity, "Choose a project."}
			}
			projectID = id
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO api_tokens (org_id, name, token_hash, role, created_by, expires_at, project_id)
			VALUES ($1, $2, $3, $4, $5, now() + interval '1 year', $6)`,
			o.ID, name, secure.HashToken(token), role, sessionFrom(r).UserID, projectID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "api_token.created", "token:"+name, map[string]string{"role": role, "project": project})
		return nil
	})
	var ae apiErr
	if errors.As(err, &ae) {
		back(w, r, o.Path()+"/iam#api", "", ae.msg)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderOrg(w, r, o, http.StatusOK, data{"APIToken": token, "APITokenName": name, "Tab": "api", "Page": "iam"})
}

func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `DELETE FROM api_tokens WHERE id = $1 RETURNING name`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "api_token.revoked", "token:"+name, nil)
		return nil
	})
	if err != nil && !isNoRows(err) {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/iam#api", "API token revoked. It stops working immediately.", "")
}
