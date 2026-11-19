package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/registry"
	"platform/internal/secure"
)

type serviceInfo struct {
	ID, Slug, Image, HealthPath, VolumePath string
	Port, CPUMillis, MemoryMB, Instances    int
	Worker                                  bool
	Env                                     map[string]string
	Ports                                   []model.PublicPort
	AutoMin, AutoMax, AutoCPU               int    // autoscaling (AutoMin 0 = off)
	MetricsPath                             string // Prometheus endpoint scraped by the platform ("" = none)
	MetricsPort                             int    // 0 = the service's port
	AnalyticsKey                            string // web analytics script key ("" = off)
	EdgeCache                               bool   // the proxy caches responses that allow it
	Public                                  bool   // has a public address (false: only its .internal name)
	DefaultAddress                          bool   // the platform address answers (false: only its verified custom domains)
	PreviewName, ParentSlug                 string // set for preview environments
	Expires                                 *time.Time
}

func (sv *serviceInfo) isPreview() bool { return sv.PreviewName != "" }

// serviceHost is the platform address of a service or preview.
func (s *Server) serviceHost(o *orgInfo, p *projectInfo, sv *serviceInfo) string {
	if sv.isPreview() {
		return s.previewHost(sv.PreviewName, sv.ParentSlug, p.Slug, o.Slug)
	}
	return model.DefaultHost(sv.Slug, p.Slug, o.Slug, s.cfg.AppsDomain)
}

func (sv *serviceInfo) spec() model.Spec {
	return model.Spec{Image: sv.Image, Port: sv.Port, CPUMillis: sv.CPUMillis, MemoryMB: sv.MemoryMB,
		HealthPath: sv.HealthPath, VolumePath: sv.VolumePath, Env: sv.Env, Worker: sv.Worker, Instances: sv.Instances, Ports: sv.Ports}
}

var metricsPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

func (s *Server) loadService(r *http.Request, tx pgx.Tx, projectID string) (*serviceInfo, error) {
	var sv serviceInfo
	err := tx.QueryRow(r.Context(), `
		SELECT s.id, s.slug, s.image, s.health_path, s.volume_path, s.port, s.cpu_millis, s.memory_mb, s.env,
		       coalesce(s.preview_name, ''), coalesce(ps.slug, ''), s.expires_at, s.worker, s.instances, s.ports,
		       s.autoscale_min, s.autoscale_max, s.autoscale_cpu, s.metrics_path, s.metrics_port, coalesce(s.analytics_key, ''), s.edge_cache, s.public, s.default_address
		FROM services s LEFT JOIN services ps ON ps.id = s.preview_of
		WHERE s.project_id = $1 AND s.slug = $2`, projectID, r.PathValue("service")).
		Scan(&sv.ID, &sv.Slug, &sv.Image, &sv.HealthPath, &sv.VolumePath, &sv.Port, &sv.CPUMillis, &sv.MemoryMB, &sv.Env,
			&sv.PreviewName, &sv.ParentSlug, &sv.Expires, &sv.Worker, &sv.Instances, &sv.Ports, &sv.AutoMin, &sv.AutoMax, &sv.AutoCPU,
			&sv.MetricsPath, &sv.MetricsPort, &sv.AnalyticsKey, &sv.EdgeCache, &sv.Public, &sv.DefaultAddress)
	if sv.Env == nil {
		sv.Env = map[string]string{}
	}
	return &sv, err
}

// withService resolves org (role check), project and service inside one RLS-scoped transaction.
func (s *Server) withService(w http.ResponseWriter, r *http.Request, minRole string,
	fn func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error) (*orgInfo, *projectInfo, *serviceInfo, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, nil, false
	}
	var p *projectInfo
	var sv *serviceInfo
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.loadProject(r, tx); err != nil {
			return err
		}
		if sv, err = s.loadService(r, tx, p.ID); err != nil {
			return err
		}
		return fn(tx, o, p, sv)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, nil, false
	}
	return o, p, sv, err, true
}

func svcPath(o *orgInfo, p *projectInfo, sv *serviceInfo) string {
	return o.Path() + "/p/" + p.Slug + "/s/" + sv.Slug
}

func (s *Server) appURL(host string) string {
	u, err := url.Parse(s.cfg.BaseURL)
	if err != nil {
		return "http://" + host
	}
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	return u.Scheme + "://" + host
}

// ---------- create / view ----------

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	image := strings.TrimSpace(r.PostFormValue("image"))
	base := o.Path() + "/p/" + r.PathValue("project")
	if !model.ValidSlug(slug) {
		back(w, r, base, "", "Service ID: 2 to 20 characters, lowercase letters, digits and dashes.")
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		if err := s.checkQuota(r, tx, o, "", 250, 256, true); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO services (org_id, project_id, slug, image) VALUES ($1, $2, $3, $4)`,
			o.ID, p.ID, slug, image); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "service.created", "service:"+p.Slug+"/"+slug, nil)
		return nil
	})
	var qe quotaError
	switch {
	case errors.As(err, &qe):
		back(w, r, base, "", qe.Error())
	case isUniqueViolation(err):
		back(w, r, base, "", "A service with this ID already exists in the project.")
	case isNoRows(err):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, base+"/s/"+slug, "Service created. Check the settings, then deploy.", "")
	}
}

type deploymentView struct {
	Revision  int
	Status    string
	Message   string
	Image     string
	By        string
	CreatedAt time.Time
}

type domainView struct {
	ID, Hostname, Token string
	Verified            bool
	Zone                string // the hosted zone where the platform wrote its records ("" if none)
	Delegation          string // that zone's delegation problem, if any
}

func (s *Server) servicePage(w http.ResponseWriter, r *http.Request) {
	var deps []deploymentView
	var secrets []string
	var projectVars []projectVar
	var domains []domainView
	var schedules []scheduleView
	var extraPorts []portView
	var domainPaths map[string][]pathView
	var pathTargets []string
	var scan *scanSummary
	var findings []finding
	var backups *backupView
	var incidents []incidentView
	var cpuPct, memPct *int
	var prot protection
	var previews []previewView
	var source sourceInfo
	var builds []buildView
	o, p, sv, err, ok := s.withService(w, r, "viewer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		var err error
		if deps, err = recentDeployments(r, tx, sv.ID, 15); err != nil {
			return err
		}
		if backups, err = loadBackups(r, tx, sv.ID); err != nil {
			return err
		}
		if incidents, err = loadIncidents(r, tx, sv.ID, 8); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), `SELECT alert_cpu_pct, alert_mem_pct FROM services WHERE id = $1`, sv.ID).Scan(&cpuPct, &memPct); err != nil {
			return err
		}
		if prot, err = loadProtection(r, tx, sv.ID); err != nil {
			return err
		}
		if previews, err = s.loadPreviews(r, tx, o, p, sv); err != nil {
			return err
		}
		if source, err = loadSource(r, tx, sv.ID); err != nil {
			return err
		}
		if builds, err = loadBuilds(r, tx, sv.ID, 15); err != nil {
			return err
		}
		var sc scanSummary
		var raw []byte
		err = tx.QueryRow(r.Context(), `
			SELECT s.id, s.image, s.source, s.status, s.critical, s.high, s.medium, s.low, s.error, s.created_at, s.findings
			FROM deployments d JOIN image_scans s ON s.id = d.scan_id
			WHERE d.service_id = $1 ORDER BY d.revision DESC LIMIT 1`, sv.ID).
			Scan(&sc.ID, &sc.Image, &sc.Source, &sc.Status, &sc.Critical, &sc.High, &sc.Medium, &sc.Low, &sc.Error, &sc.CreatedAt, &raw)
		if err == nil {
			scan = &sc
			_ = json.Unmarshal(raw, &findings)
			if len(findings) > 25 {
				findings = findings[:25]
			}
		} else if !isNoRows(err) {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT name FROM secrets WHERE service_id = $1 ORDER BY name`, sv.ID)
		if err != nil {
			return err
		}
		if secrets, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		if projectVars, err = s.loadProjectVars(r, tx, p.ID); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `SELECT dm.id, dm.hostname, dm.verify_token, dm.verified_at IS NOT NULL,
			coalesce((SELECT z.domain FROM dns_records x JOIN dns_zones z ON z.id = x.zone_id WHERE x.managed_by = 'domain:' || dm.id::text LIMIT 1), ''),
			coalesce((SELECT z.delegation FROM dns_records x JOIN dns_zones z ON z.id = x.zone_id WHERE x.managed_by = 'domain:' || dm.id::text LIMIT 1), '')
			FROM domains dm WHERE dm.service_id = $1 ORDER BY dm.hostname`, sv.ID)
		if err != nil {
			return err
		}
		if domains, err = pgx.CollectRows(rows, pgx.RowToStructByPos[domainView]); err != nil {
			return err
		}
		if extraPorts, err = s.loadServicePorts(r.Context(), tx, o, p, sv); err != nil {
			return err
		}
		if domainPaths, err = loadDomainPaths(r.Context(), tx, sv.ID); err != nil {
			return err
		}
		trow, err := tx.Query(r.Context(), `SELECT tp.slug || '/' || t.slug FROM services t JOIN projects tp ON tp.id = t.project_id
			WHERE t.preview_of IS NULL AND NOT t.worker AND t.id <> $1 ORDER BY 1`, sv.ID)
		if err != nil {
			return err
		}
		if pathTargets, err = pgx.CollectRows(trow, pgx.RowTo[string]); err != nil {
			return err
		}
		schedules, err = loadSchedules(r.Context(), tx, sv.ID)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var jobs []jobView
	var jobRuns []jobRunView
	if !sv.isPreview() {
		if err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
			var err error
			if jobs, err = loadJobs(r, tx, sv.ID); err != nil {
				return err
			}
			jobRuns, err = loadJobRuns(r, tx, sv.ID, "", 15, true)
			return err
		}); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	host := s.serviceHost(o, p, sv)
	url := s.appURL(host)
	var verified []string
	for _, d := range domains {
		if d.Verified {
			verified = append(verified, d.Hostname)
		}
	}
	if !sv.DefaultAddress && !sv.isPreview() && len(verified) > 0 {
		url = "https://" + verified[0]
	}
	s.render(w, r, "service", http.StatusOK, data{
		"Jobs": jobs, "JobRuns": jobRuns, "Schedules": schedules, "ExtraPorts": extraPorts, "DomainPaths": domainPaths, "PathTargets": pathTargets,
		"Org": o, "Project": p, "Service": sv, "EnvText": model.FormatEnv(sv.Env),
		"Deployments": deps, "Secrets": secrets, "Domains": domains, "ProjectVars": projectVars,
		"URL": url, "PlatformURL": s.appURL(host), "HasVerifiedDomain": len(verified) > 0, "Path": svcPath(o, p, sv), "AppsDomain": s.cfg.AppsDomain,
		"PortHost": host, "PortsText": model.PortsText(sv.Ports),
		"CanEdit": model.RoleAtLeast(o.Role, "developer"),
		"Scan":    scan, "Findings": findings, "RegistryHost": s.cfg.RegistryHost,
		"Backups": backups, "CanRestore": model.RoleAtLeast(o.Role, "admin"),
		"Incidents": incidents, "AlertCPU": cpuPct, "AlertMem": memPct,
		"Protection": prot, "Previews": previews, "MaxPreviews": maxPreviewsPerService,
		"Source": source, "Builds": builds, "BuildsEnabled": s.cfg.BuildsEnabled && s.cfg.RegistryHost != "",
	})
}

func recentDeployments(r *http.Request, tx pgx.Tx, serviceID string, limit int) ([]deploymentView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT d.revision, d.status, d.message, d.spec->>'image', coalesce(u.email, ''), d.created_at
		FROM deployments d LEFT JOIN users u ON u.id = d.created_by
		WHERE d.service_id = $1 ORDER BY d.revision DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[deploymentView])
}

// ---------- settings and deployments ----------

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var formErr string
	deployNow := r.PostFormValue("deploy") == "1"
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		env, err := model.ParseEnv(r.PostFormValue("env"))
		if err != nil {
			formErr = "Environment: " + err.Error()
			return nil
		}
		ports, err := model.ParsePorts(r.PostFormValue("ports"))
		if err != nil {
			formErr = "Public ports: " + err.Error()
			return nil
		}
		if sv.isPreview() {
			ports = nil // previews have no public ports
		}
		next := model.Spec{
			Ports:      ports,
			Image:      strings.TrimSpace(r.PostFormValue("image")),
			Port:       atoi(r.PostFormValue("port")),
			CPUMillis:  atoi(r.PostFormValue("cpu_millis")),
			MemoryMB:   atoi(r.PostFormValue("memory_mb")),
			HealthPath: strings.TrimSpace(r.PostFormValue("health_path")),
			VolumePath: strings.TrimSpace(r.PostFormValue("volume_path")),
			Env:        env,
			Worker:     r.PostFormValue("kind") == "worker",
			Instances:  max(1, atoi(r.PostFormValue("instances"))),
		}
		if sv.isPreview() {
			next.Instances = 1 // previews run a single instance
		}
		if next.Worker && !strings.HasPrefix(next.HealthPath, "/") {
			next.HealthPath = "/" // unused by workers
		}
		// Autoscaling: between min and max instances around a CPU target.
		autoMin, autoMax, autoCPU := 0, 0, atoiDefault(r.PostFormValue("autoscale_cpu"), 70)
		if r.PostFormValue("autoscale") == "1" && !sv.isPreview() {
			autoMin, autoMax = atoi(r.PostFormValue("autoscale_min")), atoi(r.PostFormValue("autoscale_max"))
			switch {
			case autoMin < 1 || autoMax < autoMin || autoMax > 10:
				formErr = "Autoscaling: from 1 to 10 instances, the minimum at most the maximum."
			case autoCPU < 10 || autoCPU > 95:
				formErr = "Autoscaling: a CPU target between 10 and 95%."
			case next.VolumePath != "" || len(next.Ports) > 0:
				formErr = "Autoscaling needs a service without a persistent folder or public ports (they run a single instance)."
			}
			if formErr != "" {
				return nil
			}
			next.Instances = min(max(next.Count(), autoMin), autoMax)
		}
		if autoCPU < 10 || autoCPU > 95 {
			autoCPU = 70
		}
		if next.Worker && !sv.Worker {
			var n int
			if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM domains WHERE service_id = $1`, sv.ID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				formErr = "Remove this service's custom domains first: a worker has no public address."
				return nil
			}
		}
		if err := next.Validate(); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := registry.CheckNamespace(next.Image, s.cfg.RegistryHost, o.Slug); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := s.checkInstances(r, tx, o, max(next.Count(), autoMax)); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		if err := s.checkQuota(r, tx, o, sv.ID, next.CPUMillis*max(next.Count(), autoMax), next.MemoryMB*max(next.Count(), autoMax), false); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		metricsPath, metricsPort := strings.TrimSpace(r.PostFormValue("metrics_path")), atoiDefault(r.PostFormValue("metrics_port"), 0)
		if metricsPath != "" && (!metricsPathRe.MatchString(metricsPath) || len(metricsPath) > 200) || metricsPort < 0 || metricsPort > 65535 {
			formErr = "Metrics: a path such as /metrics, and a port from 1 to 65535 (empty: the service's port)."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET autoscale_min = $2, autoscale_max = $3, autoscale_cpu = $4, metrics_path = $5, metrics_port = $6, edge_cache = $7, public = $8 WHERE id = $1`,
			sv.ID, autoMin, autoMax, autoCPU, metricsPath, metricsPort, r.PostFormValue("edge_cache") == "1",
			sv.isPreview() || r.PostFormValue("private") != "1"); err != nil {
			return err
		}
		if next.Ports, err = s.allocatePorts(r, tx, o, sv.ID, next.Ports); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		envJSON, _ := json.Marshal(next.Env)
		portsJSON, _ := json.Marshal(next.Ports)
		if _, err := tx.Exec(r.Context(), `
			UPDATE services SET image = $2, port = $3, cpu_millis = $4, memory_mb = $5, health_path = $6, volume_path = $7, env = $8, worker = $9,
			       instances = $10, ports = $11
			WHERE id = $1`, sv.ID, next.Image, next.Port, next.CPUMillis, next.MemoryMB, next.HealthPath, next.VolumePath, envJSON, next.Worker, next.Count(), portsJSON); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "service.updated", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"image": next.Image, "cpu_millis": next.CPUMillis, "memory_mb": next.MemoryMB, "worker": next.Worker, "instances": next.Count()})
		if deployNow {
			_, err := s.createDeployment(r, tx, o, sv.ID, next)
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
	switch {
	case formErr != "":
		back(w, r, svcPath(o, p, sv)+"#settings", "", formErr)
	case deployNow:
		back(w, r, svcPath(o, p, sv)+"#settings", "Settings saved. Deployment started.", "")
	default:
		back(w, r, svcPath(o, p, sv)+"#settings", "Settings saved. They apply at the next deployment.", "")
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// createDeployment records a new revision and wakes the agent. That is all the API can do.
func (s *Server) createDeployment(r *http.Request, tx pgx.Tx, o *orgInfo, serviceID string, spec model.Spec) (int, error) {
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	if err := registry.CheckNamespace(spec.Image, s.cfg.RegistryHost, o.Slug); err != nil {
		return 0, err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return 0, err
	}
	var rev int
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO deployments (org_id, service_id, revision, spec, created_by)
		VALUES ($1, $2, (SELECT coalesce(max(revision), 0) + 1 FROM deployments WHERE service_id = $2), $3, nullif($4, '')::uuid)
		RETURNING revision`, o.ID, serviceID, b, sessionFrom(r).UserID).Scan(&rev); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', $1)`, serviceID); err != nil {
		return 0, err
	}
	s.audit(r, tx, o.ID, "deployment.created", "service:"+serviceID, map[string]any{"revision": rev, "image": spec.Image})
	return rev, nil
}

func (s *Server) deployService(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if err := sv.spec().Validate(); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := registry.CheckNamespace(sv.Image, s.cfg.RegistryHost, o.Slug); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := s.checkDisk(r, tx, o); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		_, err := s.createDeployment(r, tx, o, sv.ID, sv.spec())
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#overview", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#overview", "Deployment started.", "")
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	rev := atoi(r.PathValue("rev"))
	var newRev int
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		var raw []byte
		if err := tx.QueryRow(r.Context(), `SELECT spec FROM deployments WHERE service_id = $1 AND revision = $2`, sv.ID, rev).Scan(&raw); err != nil {
			return err
		}
		var spec model.Spec
		if err := json.Unmarshal(raw, &spec); err != nil {
			return err
		}
		envJSON, _ := json.Marshal(spec.Env)
		if _, err := tx.Exec(r.Context(), `
			UPDATE services SET image = $2, port = $3, cpu_millis = $4, memory_mb = $5, health_path = $6, volume_path = $7, env = $8, worker = $9,
			       instances = $10
			WHERE id = $1`, sv.ID, spec.Image, spec.Port, spec.CPUMillis, spec.MemoryMB, spec.HealthPath, spec.VolumePath, envJSON, spec.Worker, spec.Count()); err != nil {
			return err
		}
		var err error
		newRev, err = s.createDeployment(r, tx, o, sv.ID, spec)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#overview", "Rolling back: revision "+itoa(rev)+" is being redeployed as revision "+itoa(newRev)+".", "")
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if r.PostFormValue("confirm") != sv.Slug {
			formErr = "Type the service ID exactly to confirm deletion."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM services WHERE id = $1`, sv.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "service.deleted", "service:"+p.Slug+"/"+sv.Slug, nil)
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#settings", "", formErr)
		return
	}
	back(w, r, o.Path()+"/p/"+p.Slug, "Service deleted. Its container is being removed.", "")
}

// ---------- secrets ----------

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		name := strings.TrimSpace(r.PostFormValue("name"))
		value := r.PostFormValue("value")
		if !model.SecretNameRe.MatchString(name) || strings.HasPrefix(name, "PLATFORM_") {
			formErr = "Secret names use uppercase letters, digits and underscores (for example DATABASE_URL)."
			return nil
		}
		if value == "" || len(value) > 8192 {
			formErr = "The secret value must be between 1 and 8192 characters."
			return nil
		}
		enc, err := s.box.Seal([]byte(value), "secret:"+sv.ID+":"+name)
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
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#settings", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#settings", "Secret saved. Deploy to apply it.", "")
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		name := r.PathValue("name")
		if _, err := tx.Exec(r.Context(), `DELETE FROM secrets WHERE service_id = $1 AND name = $2`, sv.ID, name); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "secret.deleted", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"name": name})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#settings", "Secret deleted. Deploy to apply it.", "")
}

// ---------- custom domains ----------

const challengePrefix = "_platform-challenge."

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	var formErr, auto string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.PostFormValue("hostname"))), ".")
		if sv.Worker {
			formErr = "A worker has no public address, so it cannot have a domain."
			return nil
		}
		if !model.ValidHostname(host) {
			formErr = "Enter a domain name such as shop.example.com."
			return nil
		}
		if host == s.cfg.ConsoleHost || strings.HasSuffix(host, "."+s.cfg.AppsDomain) || host == s.cfg.AppsDomain {
			formErr = "This domain belongs to the platform."
			return nil
		}
		plan, err := s.loadPlan(r, tx, o.PlanID)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM domains`).Scan(&n); err != nil {
			return err
		}
		if n >= plan.MaxDomains {
			formErr = "Your plan allows " + itoa(plan.MaxDomains) + " custom domains."
			return nil
		}
		token := secure.NewToken()[:32]
		var id string
		if err := tx.QueryRow(r.Context(), `INSERT INTO domains (org_id, service_id, hostname, verify_token) VALUES ($1, $2, $3, $4) RETURNING id`,
			o.ID, sv.ID, host, token).Scan(&id); err != nil {
			if isUniqueViolation(err) {
				formErr = "This domain is already registered on the platform."
				return nil
			}
			return err
		}
		s.audit(r, tx, o.ID, "domain.added", "domain:"+host, nil)
		// in a zone hosted here: the platform writes the records (and verifies it if it can)
		done, err := s.autoServiceDomainTx(r.Context(), r, tx, o, id, host, token, s.serviceHost(o, p, sv))
		if err != nil {
			return err
		}
		if done {
			var verified bool
			_ = tx.QueryRow(r.Context(), `SELECT verified_at IS NOT NULL FROM domains WHERE id = $1`, id).Scan(&verified)
			auto = "Domain added. Its DNS records were created in your hosted zone"
			if verified {
				auto += " and it is verified: HTTPS starts within a minute."
			} else {
				auto += "; it is verified as soon as the zone's delegation is confirmed."
			}
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
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#domains", "", formErr)
		return
	}
	if auto != "" {
		back(w, r, svcPath(o, p, sv)+"#domains", auto, "")
		return
	}
	back(w, r, svcPath(o, p, sv)+"#domains", "Domain added. Create the DNS records shown, then click Verify.", "")
}

func (s *Server) verifyDomain(w http.ResponseWriter, r *http.Request) {
	var formErr, host string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		var token string
		if err := tx.QueryRow(r.Context(), `SELECT hostname, verify_token FROM domains WHERE id = $1 AND service_id = $2`,
			r.PathValue("id"), sv.ID).Scan(&host, &token); err != nil {
			return err
		}
		if !s.limiter.allow("verify:"+host, 10, 10*time.Minute) {
			formErr = "Too many verification attempts. Wait a few minutes."
			return nil
		}
		txts, err := net.DefaultResolver.LookupTXT(r.Context(), challengePrefix+host)
		found := false
		for _, t := range txts {
			if strings.TrimSpace(t) == "platform-verify="+token {
				found = true
			}
		}
		if err != nil || !found {
			formErr = "The TXT record " + challengePrefix + host + " was not found yet. DNS changes can take a few minutes."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `UPDATE domains SET verified_at = now() WHERE id = $1`, r.PathValue("id")); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "domain.verified", "domain:"+host, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#domains", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#domains", host+" verified. It will serve this application within a few seconds.", "")
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		var host string
		if err := tx.QueryRow(r.Context(), `DELETE FROM domains WHERE id = $1 AND service_id = $2 RETURNING hostname`,
			r.PathValue("id"), sv.ID).Scan(&host); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "domain.removed", "domain:"+host, nil)
		if err := s.removeManagedTx(r.Context(), r, tx, o, "domain:"+r.PathValue("id")); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#domains", "Domain removed.", "")
}
