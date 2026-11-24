package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.apiRoutes(m)
	s.consoleRoutes(m)
}

// InternalRoutes registers this component's endpoints on the internal listener (never published).
func (s *API) InternalRoutes(m *http.ServeMux) {
	s.internalRoutes(m)
}

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	const svc = "/api/v1/projects/{project}/services/{service}"
	m.HandleFunc("GET /api/v1/prometheus/api/v1/{rest...}", s.APIAuth("viewer", s.apiPrometheus))
	m.HandleFunc("POST /api/v1/prometheus/api/v1/{rest...}", s.APIAuth("viewer", s.apiPrometheus))
	m.HandleFunc("GET /api/v1/alarms", s.APIAuth("viewer", s.apiAlarms))
	m.HandleFunc("POST /api/v1/alarms", s.APIAuth("developer", s.apiCreateAlarm))
	m.HandleFunc("DELETE /api/v1/alarms/{id}", s.APIAuth("developer", s.apiDeleteAlarm))
	m.HandleFunc("GET "+svc+"/analytics", s.APIAuth("viewer", s.apiAnalytics))
}

// internalRoutes: this component's part of the former internalRoutes table.
func (s *API) internalRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /internal/tls-ask", s.tlsAsk)
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	const svc = "/o/{org}/p/{project}/s/{service}"
	m.HandleFunc("GET /o/{org}/monitoring/logs", s.Auth(web.AuthFull, s.orgLogsPage))
	m.HandleFunc("GET /o/{org}/monitoring/logs.txt", s.Auth(web.AuthFull, s.downloadLogs))
	m.HandleFunc("POST /o/{org}/monitoring/logs/exports", s.Auth(web.AuthFull, s.createLogExport))
	m.HandleFunc("POST /o/{org}/monitoring/logs/exports/{id}", s.Auth(web.AuthFull, s.updateLogExport))
	m.HandleFunc("GET /a/s.js", s.analyticsScriptHandler)
	m.HandleFunc("POST /a/e", s.analyticsCollect)
	m.HandleFunc("OPTIONS /a/e", s.analyticsCollect)
	m.HandleFunc("GET /o/{org}/analytics", s.Auth(web.AuthFull, s.analyticsPage))
	m.HandleFunc("POST /o/{org}/analytics/script", s.Auth(web.AuthFull, s.analyticsScriptToggle))
	m.HandleFunc("POST /o/{org}/monitoring/grafana", s.Auth(web.AuthFull, s.createGrafana))
	m.HandleFunc("POST /o/{org}/monitoring/alarms", s.Auth(web.AuthFull, s.createAlarm))
	m.HandleFunc("POST /o/{org}/monitoring/alarms/{id}", s.Auth(web.AuthFull, s.alarmAction))
	m.HandleFunc("POST /o/{org}/alerts/channels", s.Auth(web.AuthFull, s.createChannel))
	m.HandleFunc("POST /o/{org}/alerts/channels/{id}/delete", s.Auth(web.AuthFull, s.deleteChannel))
	m.HandleFunc("POST /o/{org}/alerts/channels/{id}/test", s.Auth(web.AuthFull, s.testChannel))
	m.HandleFunc("POST "+svc+"/alerts", s.Auth(web.AuthFull, s.alertThresholds))
	m.HandleFunc("GET "+svc+"/status.json", s.Auth(web.AuthFull, s.statusJSON))
	m.HandleFunc("GET "+svc+"/logs.json", s.Auth(web.AuthFull, s.logsJSON))
	m.HandleFunc("GET "+svc+"/metrics.json", s.Auth(web.AuthFull, s.metricsJSON))
}
