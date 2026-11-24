package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/registry"
)

// ---------- create / view ----------

func (s *API) createService(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	image := strings.TrimSpace(r.PostFormValue("image"))
	base := o.Path() + "/p/" + r.PathValue("project")
	if !model.ValidSlug(slug) {
		web.Back(w, r, base, "", "Service ID: 2 to 20 characters, lowercase letters, digits and dashes.")
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.LoadProject(r, tx)
		if err != nil {
			return err
		}
		own, err := web.OnOwnServer(r.Context(), tx, p.ID)
		if err != nil {
			return err
		}
		if !own {
			if err := s.CheckQuota(r, tx, o, "", 250, 256, true); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO services (org_id, project_id, slug, image) VALUES ($1, $2, $3, $4)`,
			o.ID, p.ID, slug, image); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "service.created", "service:"+p.Slug+"/"+slug, nil)
		return nil
	})
	var qe web.QuotaError
	switch {
	case errors.As(err, &qe):
		web.Back(w, r, base, "", qe.Error())
	case web.IsUniqueViolation(err):
		web.Back(w, r, base, "", "A service with this ID already exists in the project.")
	case web.IsNoRows(err):
		s.NotFound(w, r)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, base+"/s/"+slug, "Service created. Check the settings, then deploy.", "")
	}
}

type domainView struct {
	ID, Hostname, Token string
	Verified            bool
	Zone                string // the hosted zone where the platform wrote its records ("" if none)
	Delegation          string // that zone's delegation problem, if any
}

func (s *API) servicePage(w http.ResponseWriter, r *http.Request) {
	var deps []web.DeploymentView
	var secrets []string
	var projectVars []web.ProjectVar
	var domains []domainView
	var schedules []scheduleView
	var extraPorts []portView
	var domainPaths map[string][]pathView
	var pathTargets []string
	var scan *web.ScanSummary
	var findings []web.Finding
	var backups *web.BackupView
	var incidents []web.IncidentView
	var cpuPct, memPct *int
	var prot protection
	var previews []previewView
	var source web.SourceInfo
	var builds []web.BuildView
	o, p, sv, err, ok := s.WithService(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		if deps, err = web.RecentDeployments(r, tx, sv.ID, 15); err != nil {
			return err
		}
		if backups, err = web.LoadBackups(r, tx, sv.ID); err != nil {
			return err
		}
		if incidents, err = web.LoadIncidents(r, tx, sv.ID, 8); err != nil {
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
		if source, err = web.LoadSource(r, tx, sv.ID); err != nil {
			return err
		}
		if builds, err = web.LoadBuilds(r, tx, sv.ID, 15); err != nil {
			return err
		}
		var sc web.ScanSummary
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
		} else if !web.IsNoRows(err) {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT name FROM secrets WHERE service_id = $1 ORDER BY name`, sv.ID)
		if err != nil {
			return err
		}
		if secrets, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		if projectVars, err = s.LoadProjectVars(r, tx, p.ID); err != nil {
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
		s.ServerError(w, r, err)
		return
	}
	var jobs []jobView
	var jobRuns []jobRunView
	if !sv.IsPreview() {
		if err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
			var err error
			if jobs, err = loadJobs(r, tx, sv.ID); err != nil {
				return err
			}
			jobRuns, err = loadJobRuns(r, tx, sv.ID, "", 15, true)
			return err
		}); err != nil {
			s.ServerError(w, r, err)
			return
		}
	}
	host := s.ServiceHost(o, p, sv)
	url := s.AppURL(host)
	var verified []string
	for _, d := range domains {
		if d.Verified {
			verified = append(verified, d.Hostname)
		}
	}
	if !sv.DefaultAddress && !sv.IsPreview() && len(verified) > 0 {
		url = "https://" + verified[0]
	}
	s.Render(w, r, "service", http.StatusOK, web.Data{
		"Jobs": jobs, "JobRuns": jobRuns, "Schedules": schedules, "ExtraPorts": extraPorts, "DomainPaths": domainPaths, "PathTargets": pathTargets,
		"Org": o, "Project": p, "Service": sv, "EnvText": model.FormatEnv(sv.Env),
		"Deployments": deps, "Secrets": secrets, "Domains": domains, "ProjectVars": projectVars,
		"URL": url, "PlatformURL": s.AppURL(host), "HasVerifiedDomain": len(verified) > 0, "Path": web.SvcPath(o, p, sv), "AppsDomain": s.Cfg.AppsDomain,
		"PortHost": host, "PortsText": model.PortsText(sv.Ports),
		"CanEdit": model.RoleAtLeast(o.Role, "developer"),
		"Scan":    scan, "Findings": findings, "RegistryHost": s.Cfg.RegistryHost,
		"Backups": backups, "CanRestore": model.RoleAtLeast(o.Role, "admin"),
		"Incidents": incidents, "AlertCPU": cpuPct, "AlertMem": memPct,
		"Protection": prot, "Previews": previews, "MaxPreviews": web.MaxPreviewsPerService,
		"Source": source, "Builds": builds, "BuildsEnabled": s.Cfg.BuildsEnabled && s.Cfg.RegistryHost != "",
	})
}

// ---------- settings and deployments ----------

func (s *API) saveSettings(w http.ResponseWriter, r *http.Request) {
	var formErr string
	deployNow := r.PostFormValue("deploy") == "1"
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		formErr, _, err = s.ApplyServiceSettings(r, tx, o, p, sv, r.PostForm, deployNow)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	switch {
	case formErr != "":
		web.Back(w, r, web.SvcPath(o, p, sv)+"#settings", "", formErr)
	case deployNow:
		web.Back(w, r, web.SvcPath(o, p, sv)+"#settings", "Settings saved. Deployment started.", "")
	default:
		web.Back(w, r, web.SvcPath(o, p, sv)+"#settings", "Settings saved. They apply at the next deployment.", "")
	}
}

func (s *API) deployService(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if err := sv.Spec().Validate(); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := registry.CheckNamespace(sv.Image, s.Cfg.RegistryHost, o.Slug); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := s.CheckDisk(r, tx, o); err != nil {
			var qe web.QuotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		_, err := s.CreateDeployment(r, tx, o, sv.ID, sv.Spec())
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
		web.Back(w, r, web.SvcPath(o, p, sv)+"#overview", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#overview", "Deployment started.", "")
}

func (s *API) rollback(w http.ResponseWriter, r *http.Request) {
	rev := web.Atoi(r.PathValue("rev"))
	var newRev int
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
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
		newRev, err = s.CreateDeployment(r, tx, o, sv.ID, spec)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#overview", "Rolling back: revision "+web.Itoa(rev)+" is being redeployed as revision "+web.Itoa(newRev)+".", "")
}

func (s *API) deleteService(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if r.PostFormValue("confirm") != sv.Slug {
			formErr = "Type the service ID exactly to confirm deletion."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM services WHERE id = $1`, sv.ID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "service.deleted", "service:"+p.Slug+"/"+sv.Slug, nil)
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
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
		web.Back(w, r, web.SvcPath(o, p, sv)+"#settings", "", formErr)
		return
	}
	web.Back(w, r, o.Path()+"/p/"+p.Slug, "Service deleted. Its container is being removed.", "")
}

// ---------- secrets ----------

func (s *API) setSecret(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
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
		enc, err := s.Box.Seal([]byte(value), "secret:"+sv.ID+":"+name)
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
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#settings", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#settings", "Secret saved. Deploy to apply it.", "")
}

func (s *API) deleteSecret(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		name := r.PathValue("name")
		if _, err := tx.Exec(r.Context(), `DELETE FROM secrets WHERE service_id = $1 AND name = $2`, sv.ID, name); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "secret.deleted", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"name": name})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#settings", "Secret deleted. Deploy to apply it.", "")
}

func (s *API) verifyDomain(w http.ResponseWriter, r *http.Request) {
	var formErr, host string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var token string
		if err := tx.QueryRow(r.Context(), `SELECT hostname, verify_token FROM domains WHERE id = $1 AND service_id = $2`,
			r.PathValue("id"), sv.ID).Scan(&host, &token); err != nil {
			return err
		}
		if !s.Limiter.Allow("verify:"+host, 10, 10*time.Minute) {
			formErr = "Too many verification attempts. Wait a few minutes."
			return nil
		}
		txts, err := net.DefaultResolver.LookupTXT(r.Context(), web.ChallengePrefix+host)
		found := false
		for _, t := range txts {
			if strings.TrimSpace(t) == "platform-verify="+token {
				found = true
			}
		}
		if err != nil || !found {
			formErr = "The TXT record " + web.ChallengePrefix + host + " was not found yet. DNS changes can take a few minutes."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `UPDATE domains SET verified_at = now() WHERE id = $1`, r.PathValue("id")); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "domain.verified", "domain:"+host, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
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
		web.Back(w, r, web.SvcPath(o, p, sv)+"#domains", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#domains", host+" verified. It will serve this application within a few seconds.", "")
}

func (s *API) deleteDomain(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.DeleteDomainTx(r, tx, o, sv, r.PathValue("id"))
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#domains", "Domain removed.", "")
}
