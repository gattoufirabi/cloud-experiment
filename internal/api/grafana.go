package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/secure"
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

func (s *Server) createGrafana(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	project := r.PostFormValue("project")
	var formErr, password, host string
	var buildN int
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var projectID string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE slug = $1`, project).Scan(&projectID); err != nil {
			formErr = "Choose a project."
			return nil
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
			if _, err := tx.Exec(r.Context(), `DELETE FROM api_tokens WHERE name = $1`, truncateStr("grafana in "+project, 60)); err != nil {
				return err
			}
			s.audit(r, tx, o.ID, "service.deleted", "service:"+project+"/grafana", map[string]any{"replaced_failed_grafana": true})
		}
		spec := model.Spec{Image: "", Port: 3000, CPUMillis: 250, MemoryMB: 256, HealthPath: "/api/health", Instances: 1}
		if err := s.checkQuota(r, tx, o, "", spec.CPUMillis, spec.MemoryMB, true); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		host = model.DefaultHost("grafana", project, o.Slug, s.cfg.AppsDomain)
		env, _ := json.Marshal(map[string]string{"GF_SERVER_ROOT_URL": "https://" + host, "GF_SECURITY_ADMIN_USER": "admin",
			"GRAFANA_PROM_URL": s.cfg.BaseURL + "/api/v1/prometheus"})
		var serviceID string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO services (org_id, project_id, slug, image, port, cpu_millis, memory_mb, health_path, env, volume_path)
			VALUES ($1, $2, 'grafana', '', $3, $4, $5, $6, $7, '/var/lib/grafana') RETURNING id`,
			o.ID, projectID, spec.Port, spec.CPUMillis, spec.MemoryMB, spec.HealthPath, env).Scan(&serviceID); err != nil {
			if isUniqueViolation(err) {
				formErr = "This project already has a service named grafana."
				return errRollback
			}
			return err
		}
		token := apiTokenPrefix + secure.NewToken()
		if _, err := tx.Exec(r.Context(), `INSERT INTO api_tokens (org_id, name, token_hash, role, created_by) VALUES ($1, $2, $3, 'viewer', $4)`,
			o.ID, truncateStr("grafana in "+project, 60), secure.HashToken(token), nullableUser(r)); err != nil {
			return err
		}
		password = strings.ToLower(secure.NewToken()[:20])
		for name, value := range map[string]string{"GRAFANA_PROM_TOKEN": token, "GF_SECURITY_ADMIN_PASSWORD": password} {
			enc, err := s.box.Seal([]byte(value), "secret:"+serviceID+":"+name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO secrets (org_id, service_id, name, value_enc) VALUES ($1, $2, $3, $4)`, o.ID, serviceID, name, enc); err != nil {
				return err
			}
		}
		s.audit(r, tx, o.ID, "service.created", "service:"+project+"/grafana", map[string]any{"grafana": true})
		var err error
		buildN, err = s.queueZipBuild(r, tx, o, serviceID, grafanaArchive(), "grafana.zip", true, sessionFrom(r).UserID)
		var ae apiErr
		if errors.As(err, &ae) {
			formErr = "Grafana is built from a small image: " + ae.msg
			return errRollback
		}
		return err
	})
	switch {
	case formErr != "":
		back(w, r, o.Path()+"/monitoring#grafana", "", formErr)
	case err != nil:
		s.serverError(w, r, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, r, "grafana_created", http.StatusOK, data{"Org": o, "Project": project, "Host": host, "Password": password, "Build": buildN})
	}
}
