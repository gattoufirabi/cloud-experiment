package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

// Managed Grafana: one click creates a "grafana" service in a project, built from a
// small image that provisions the platform's Prometheus API (with a read-only token of
// the organization) and a dashboard of the organization's services. Grafana's data is
// kept in a persistent folder; the admin password is shown once.

const grafanaImage = "grafana/grafana-oss:11.4.0"

func grafanaArchive() []byte {
	files := map[string]string{
		"Dockerfile": `FROM ` + grafanaImage + `
USER root
COPY provisioning /etc/grafana/provisioning
COPY dashboards /var/lib/platform-dashboards
COPY apply-admin-password.sh /apply-admin-password.sh
RUN chmod 755 /apply-admin-password.sh
ENTRYPOINT ["/apply-admin-password.sh"]
ENV GF_USERS_ALLOW_SIGN_UP=false GF_AUTH_ANONYMOUS_ENABLED=false GF_ANALYTICS_REPORTING_ENABLED=false \
    GF_ANALYTICS_CHECK_FOR_UPDATES=false GF_SERVER_HTTP_PORT=3000 GF_PATHS_DATA=/var/lib/grafana
`,
		"provisioning/datasources/platform.yaml": `apiVersion: 1
datasources:
  - name: Platform
    uid: platform
    type: prometheus
    access: proxy
    url: $GRAFANA_PROM_URL
    isDefault: true
    editable: false
    jsonData:
      httpMethod: POST
      httpHeaderName1: Authorization
      timeInterval: 15s
    secureJsonData:
      httpHeaderValue1: Bearer $GRAFANA_PROM_TOKEN
`,
		"provisioning/dashboards/platform.yaml": `apiVersion: 1
providers:
  - name: Platform
    folder: Platform
    type: file
    disableDeletion: true
    options:
      path: /var/lib/platform-dashboards
`,
		"dashboards/services.json": grafanaDashboard(),
		// Grafana reads GF_SECURITY_ADMIN_PASSWORD only when it creates its database; the
		// database lives in the persistent folder, so later changes would be ignored. On
		// every start after the first, the password is set from the secret, which stays
		// the reference: change the secret, redeploy, and the new password applies.
		"apply-admin-password.sh": `#!/bin/sh
if [ -n "$GF_SECURITY_ADMIN_PASSWORD" ] && [ -f "${GF_PATHS_DATA:-/var/lib/grafana}/grafana.db" ]; then
  if grafana cli --homepath "${GF_PATHS_HOME:-/usr/share/grafana}" admin reset-admin-password "$GF_SECURITY_ADMIN_PASSWORD" >/dev/null 2>&1; then
    echo "admin password set from GF_SECURITY_ADMIN_PASSWORD"
  else
    echo "could not set the admin password from GF_SECURITY_ADMIN_PASSWORD" >&2
  fi
fi
exec /run.sh "$@"
`,
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func grafanaDashboard() string {
	panel := func(id int, title, expr, unit string, x, y int) map[string]any {
		return map[string]any{
			"id": id, "type": "timeseries", "title": title, "datasource": map[string]string{"type": "prometheus", "uid": "platform"},
			"gridPos":     map[string]int{"h": 8, "w": 12, "x": x, "y": y},
			"fieldConfig": map[string]any{"defaults": map[string]any{"unit": unit}, "overrides": []any{}},
			"targets":     []any{map[string]any{"refId": "A", "expr": expr, "legendFormat": "{{service_id}} #{{instance}}"}},
		}
	}
	d := map[string]any{
		"uid": "platform-services", "title": "Services", "schemaVersion": 39, "refresh": "30s",
		"time": map[string]string{"from": "now-6h", "to": "now"},
		"panels": []any{
			panel(1, "CPU (cores)", `sum by (service_id, instance) (rate(platform_container_cpu_seconds_total[2m]))`, "short", 0, 0),
			panel(2, "Memory", `sum by (service_id, instance) (platform_container_memory_bytes)`, "bytes", 12, 0),
			panel(3, "Network in", `sum by (service_id, instance) (rate(platform_container_network_rx_bytes_total[2m]))`, "Bps", 0, 8),
			panel(4, "Network out", `sum by (service_id, instance) (rate(platform_container_network_tx_bytes_total[2m]))`, "Bps", 12, 8),
		},
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	return string(b)
}

func (s *API) createGrafana(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	project := r.PostFormValue("project")
	var formErr, password, host string
	var buildN int
	var updated bool
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var projectID string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE slug = $1`, project).Scan(&projectID); err != nil {
			formErr = "Choose a project."
			return nil
		}
		// A live grafana is rebuilt from the current image (data, secrets and settings
		// kept), so older installations get fixes such as the password one.
		var liveID string
		err := tx.QueryRow(r.Context(), `
			SELECT s.id FROM services s WHERE s.project_id = $1 AND s.slug = 'grafana' AND s.preview_of IS NULL
			  AND EXISTS (SELECT 1 FROM deployments d WHERE d.service_id = s.id AND d.status = 'live')`, projectID).Scan(&liveID)
		if err == nil {
			buildN, err = s.QueueZipBuild(r, tx, o, liveID, grafanaArchive(), "grafana.zip", true, web.SessionFrom(r).UserID)
			var ae web.APIErr
			if errors.As(err, &ae) {
				formErr = "Grafana is built from a small image: " + ae.Msg
				return nil
			}
			if err == nil {
				s.Audit(r, tx, o.ID, "service.updated", "service:"+project+"/grafana", map[string]any{"grafana_rebuilt": true})
				updated = true
			}
			return err
		} else if !web.IsNoRows(err) {
			return err
		}
		// A grafana service that never went live (a failed first build or deploy) is
		// replaced, so "Create Grafana" also repairs a failed attempt.
		var replaced bool
		if err := tx.QueryRow(r.Context(), `
			WITH gone AS (
				DELETE FROM services s WHERE s.project_id = $1 AND s.slug = 'grafana' AND s.preview_of IS NULL
				  AND NOT EXISTS (SELECT 1 FROM deployments d WHERE d.service_id = s.id AND d.status = 'live')
				RETURNING s.id)
			SELECT count(*) > 0 FROM gone`, projectID).Scan(&replaced); err != nil {
			return err
		}
		if replaced {
			if _, err := tx.Exec(r.Context(), `DELETE FROM api_tokens WHERE name = $1`, web.TruncateStr("grafana in "+project, 60)); err != nil {
				return err
			}
			s.Audit(r, tx, o.ID, "service.deleted", "service:"+project+"/grafana", map[string]any{"replaced_failed_grafana": true})
		}
		spec := model.Spec{Image: "", Port: 3000, CPUMillis: 250, MemoryMB: 256, HealthPath: "/api/health", Instances: 1}
		if err := s.CheckQuota(r, tx, o, "", spec.CPUMillis, spec.MemoryMB, true); err != nil {
			var qe web.QuotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		host = model.DefaultHost("grafana", project, o.Slug, s.Cfg.AppsDomain)
		env, _ := json.Marshal(map[string]string{"GF_SERVER_ROOT_URL": "https://" + host, "GF_SECURITY_ADMIN_USER": "admin",
			"GRAFANA_PROM_URL": s.Cfg.BaseURL + "/api/v1/prometheus"})
		var serviceID string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO services (org_id, project_id, slug, image, port, cpu_millis, memory_mb, health_path, env, volume_path)
			VALUES ($1, $2, 'grafana', '', $3, $4, $5, $6, $7, '/var/lib/grafana') RETURNING id`,
			o.ID, projectID, spec.Port, spec.CPUMillis, spec.MemoryMB, spec.HealthPath, env).Scan(&serviceID); err != nil {
			if web.IsUniqueViolation(err) {
				formErr = "This project already has a service named grafana."
				return web.ErrRollback
			}
			return err
		}
		token := web.APITokenPrefix + secure.NewToken()
		if _, err := tx.Exec(r.Context(), `INSERT INTO api_tokens (org_id, name, token_hash, role, created_by) VALUES ($1, $2, $3, 'viewer', $4)`,
			o.ID, web.TruncateStr("grafana in "+project, 60), secure.HashToken(token), web.NullableUser(r)); err != nil {
			return err
		}
		password = strings.ToLower(secure.NewToken()[:20])
		for name, value := range map[string]string{"GRAFANA_PROM_TOKEN": token, "GF_SECURITY_ADMIN_PASSWORD": password} {
			enc, err := s.Box.Seal([]byte(value), "secret:"+serviceID+":"+name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO secrets (org_id, service_id, name, value_enc) VALUES ($1, $2, $3, $4)`, o.ID, serviceID, name, enc); err != nil {
				return err
			}
		}
		s.Audit(r, tx, o.ID, "service.created", "service:"+project+"/grafana", map[string]any{"grafana": true})
		buildN, err = s.QueueZipBuild(r, tx, o, serviceID, grafanaArchive(), "grafana.zip", true, web.SessionFrom(r).UserID)
		var ae web.APIErr
		if errors.As(err, &ae) {
			formErr = "Grafana is built from a small image: " + ae.Msg
			return web.ErrRollback
		}
		return err
	})
	switch {
	case formErr != "":
		web.Back(w, r, o.Path()+"/monitoring#grafana", "", formErr)
	case err != nil:
		s.ServerError(w, r, err)
	case updated:
		web.Back(w, r, o.Path()+"/p/"+project+"/s/grafana/builds/"+web.Itoa(buildN), "Grafana is being rebuilt with the latest image; its data, password secret and settings are kept.", "")
	default:
		w.Header().Set("Cache-Control", "no-store")
		s.Render(w, r, "grafana_created", http.StatusOK, web.Data{"Org": o, "Project": project, "Host": host, "Password": password, "Build": buildN})
	}
}
