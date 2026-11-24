package web

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

	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/registry"
	"platform/internal/lib/secure"
)

type ServiceInfo struct {
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

func (sv *ServiceInfo) IsPreview() bool { return sv.PreviewName != "" }

// serviceHost is the platform address of a service or preview.
func (s *Server) ServiceHost(o *OrgInfo, p *ProjectInfo, sv *ServiceInfo) string {
	if sv.IsPreview() {
		return s.PreviewHost(sv.PreviewName, sv.ParentSlug, p.Slug, o.Slug)
	}
	return model.DefaultHost(sv.Slug, p.Slug, o.Slug, s.Cfg.AppsDomain)
}

func (sv *ServiceInfo) Spec() model.Spec {
	return model.Spec{Image: sv.Image, Port: sv.Port, CPUMillis: sv.CPUMillis, MemoryMB: sv.MemoryMB,
		HealthPath: sv.HealthPath, VolumePath: sv.VolumePath, Env: sv.Env, Worker: sv.Worker, Instances: sv.Instances, Ports: sv.Ports}
}

var metricsPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

func (s *Server) LoadService(r *http.Request, tx pgx.Tx, projectID string) (*ServiceInfo, error) {
	var sv ServiceInfo
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
func (s *Server) WithService(w http.ResponseWriter, r *http.Request, minRole string,
	fn func(tx pgx.Tx, o *OrgInfo, p *ProjectInfo, sv *ServiceInfo) error) (*OrgInfo, *ProjectInfo, *ServiceInfo, error, bool) {
	o, ok := s.Org(w, r, minRole)
	if !ok {
		return nil, nil, nil, nil, false
	}
	var p *ProjectInfo
	var sv *ServiceInfo
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.LoadProject(r, tx); err != nil {
			return err
		}
		if sv, err = s.LoadService(r, tx, p.ID); err != nil {
			return err
		}
		return fn(tx, o, p, sv)
	})
	if IsNoRows(err) {
		s.NotFound(w, r)
		return nil, nil, nil, nil, false
	}
	return o, p, sv, err, true
}

func SvcPath(o *OrgInfo, p *ProjectInfo, sv *ServiceInfo) string {
	return o.Path() + "/p/" + p.Slug + "/s/" + sv.Slug
}

func (s *Server) AppURL(host string) string {
	u, err := url.Parse(s.Cfg.BaseURL)
	if err != nil {
		return "http://" + host
	}
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	return u.Scheme + "://" + host
}

type DeploymentView struct {
	Revision  int
	Status    string
	Message   string
	Image     string
	By        string
	CreatedAt time.Time
}

func RecentDeployments(r *http.Request, tx pgx.Tx, serviceID string, limit int) ([]DeploymentView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT d.revision, d.status, d.message, d.spec->>'image', coalesce(u.email, ''), d.created_at
		FROM deployments d LEFT JOIN users u ON u.id = d.created_by
		WHERE d.service_id = $1 ORDER BY d.revision DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DeploymentView])
}

// applyServiceSettings validates and saves a service's settings from form-style values
// (the console form, or the API's JSON turned into the same fields). formErr is a
// message for the user; err an internal error.
func (s *Server) ApplyServiceSettings(r *http.Request, tx pgx.Tx, o *OrgInfo, p *ProjectInfo, sv *ServiceInfo, f url.Values, deployNow bool) (formErr string, rev int, err error) {
	err = func() error {
		env, err := model.ParseEnv(f.Get("env"))
		if err != nil {
			formErr = "Environment: " + err.Error()
			return nil
		}
		ports, err := model.ParsePorts(f.Get("ports"))
		if err != nil {
			formErr = "Public ports: " + err.Error()
			return nil
		}
		if sv.IsPreview() {
			ports = nil // previews have no public ports
		}
		own, err := OnOwnServer(r.Context(), tx, p.ID)
		if err != nil {
			return err
		}
		if own && len(ports) > 0 {
			formErr = "Public TCP/UDP ports are not available for applications on your own servers yet."
			return nil
		}
		next := model.Spec{
			Ports:      ports,
			Image:      strings.TrimSpace(f.Get("image")),
			Port:       Atoi(f.Get("port")),
			CPUMillis:  Atoi(f.Get("cpu_millis")),
			MemoryMB:   Atoi(f.Get("memory_mb")),
			HealthPath: strings.TrimSpace(f.Get("health_path")),
			VolumePath: strings.TrimSpace(f.Get("volume_path")),
			Env:        env,
			Worker:     f.Get("kind") == "worker",
			Instances:  max(1, Atoi(f.Get("instances"))),
		}
		if sv.IsPreview() {
			next.Instances = 1 // previews run a single instance
		}
		if next.Worker && !strings.HasPrefix(next.HealthPath, "/") {
			next.HealthPath = "/" // unused by workers
		}
		// Autoscaling: between min and max instances around a CPU target.
		autoMin, autoMax, autoCPU := 0, 0, AtoiDefault(f.Get("autoscale_cpu"), 70)
		if f.Get("autoscale") == "1" && !sv.IsPreview() {
			autoMin, autoMax = Atoi(f.Get("autoscale_min")), Atoi(f.Get("autoscale_max"))
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
		if err := registry.CheckNamespace(next.Image, s.Cfg.RegistryHost, o.Slug); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := s.CheckInstances(r, tx, o, max(next.Count(), autoMax)); err != nil && !own {
			var qe QuotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		if err := s.CheckQuota(r, tx, o, sv.ID, next.CPUMillis*max(next.Count(), autoMax), next.MemoryMB*max(next.Count(), autoMax), false); err != nil && !own {
			var qe QuotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		metricsPath, metricsPort := strings.TrimSpace(f.Get("metrics_path")), AtoiDefault(f.Get("metrics_port"), 0)
		if metricsPath != "" && (!metricsPathRe.MatchString(metricsPath) || len(metricsPath) > 200) || metricsPort < 0 || metricsPort > 65535 {
			formErr = "Metrics: a path such as /metrics, and a port from 1 to 65535 (empty: the service's port)."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET autoscale_min = $2, autoscale_max = $3, autoscale_cpu = $4, metrics_path = $5, metrics_port = $6, edge_cache = $7, public = $8 WHERE id = $1`,
			sv.ID, autoMin, autoMax, autoCPU, metricsPath, metricsPort, f.Get("edge_cache") == "1",
			sv.IsPreview() || f.Get("private") != "1"); err != nil {
			return err
		}
		if next.Ports, err = s.allocatePorts(r, tx, o, sv.ID, next.Ports); err != nil {
			var qe QuotaError
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
		s.Audit(r, tx, o.ID, "service.updated", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"image": next.Image, "cpu_millis": next.CPUMillis, "memory_mb": next.MemoryMB, "worker": next.Worker, "instances": next.Count()})
		if deployNow {
			var err error
			rev, err = s.CreateDeployment(r, tx, o, sv.ID, next)
			return err
		}
		return nil
	}()
	return formErr, rev, err
}

func Atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// createDeployment records a new revision and wakes the agent. That is all the API can do.
func (s *Server) CreateDeployment(r *http.Request, tx pgx.Tx, o *OrgInfo, serviceID string, spec model.Spec) (int, error) {
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	if err := registry.CheckNamespace(spec.Image, s.Cfg.RegistryHost, o.Slug); err != nil {
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
		RETURNING revision`, o.ID, serviceID, b, SessionFrom(r).UserID).Scan(&rev); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', $1)`, serviceID); err != nil {
		return 0, err
	}
	s.Audit(r, tx, o.ID, "deployment.created", "service:"+serviceID, map[string]any{"revision": rev, "image": spec.Image})
	return rev, nil
}

// ---------- custom domains ----------

const ChallengePrefix = "_platform-challenge."

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	var formErr, auto string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *OrgInfo, p *ProjectInfo, sv *ServiceInfo) error {
		var err error
		formErr, auto, _, err = s.AddDomainTx(r, tx, o, p, sv, r.PostFormValue("hostname"))
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		Back(w, r, SvcPath(o, p, sv)+"#domains", "", formErr)
		return
	}
	if auto != "" {
		Back(w, r, SvcPath(o, p, sv)+"#domains", auto, "")
		return
	}
	Back(w, r, SvcPath(o, p, sv)+"#domains", "Domain added. Create the DNS records shown, then click Verify.", "")
}

// addDomainTx adds a custom domain to a service; in a zone hosted here the platform
// writes its records (auto is then the message to show). id is the new domain's ID.
func (s *Server) AddDomainTx(r *http.Request, tx pgx.Tx, o *OrgInfo, p *ProjectInfo, sv *ServiceInfo, hostname string) (formErr, auto, id string, err error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	err = func() error {
		if sv.Worker {
			formErr = "A worker has no public address, so it cannot have a domain."
			return nil
		}
		if !model.ValidHostname(host) {
			formErr = "Enter a domain name such as shop.example.com."
			return nil
		}
		if host == s.Cfg.ConsoleHost || strings.HasSuffix(host, "."+s.Cfg.AppsDomain) || host == s.Cfg.AppsDomain {
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
			formErr = "Your plan allows " + Itoa(plan.MaxDomains) + " custom domains."
			return nil
		}
		token := secure.NewToken()[:32]
		if err := tx.QueryRow(r.Context(), `INSERT INTO domains (org_id, service_id, hostname, verify_token) VALUES ($1, $2, $3, $4) RETURNING id`,
			o.ID, sv.ID, host, token).Scan(&id); err != nil {
			if IsUniqueViolation(err) {
				formErr = "This domain is already registered on the platform."
				return nil
			}
			return err
		}
		s.Audit(r, tx, o.ID, "domain.added", "domain:"+host, nil)
		// in a zone hosted here: the platform writes the records (and verifies it if it can)
		done, err := s.AutoServiceDomainTx(r.Context(), r, tx, o, id, host, token, s.ServiceHost(o, p, sv))
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
	}()
	return formErr, auto, id, err
}

// deleteDomainTx removes a custom domain and the DNS records the platform wrote for it.
func (s *Server) DeleteDomainTx(r *http.Request, tx pgx.Tx, o *OrgInfo, sv *ServiceInfo, id string) error {
	var host string
	if err := tx.QueryRow(r.Context(), `DELETE FROM domains WHERE id = $1 AND service_id = $2 RETURNING hostname`,
		id, sv.ID).Scan(&host); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "domain.removed", "domain:"+host, nil)
	if err := s.RemoveManagedTx(r.Context(), r, tx, o, "domain:"+id); err != nil {
		return err
	}
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}
