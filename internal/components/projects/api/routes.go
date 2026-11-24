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

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/projects/{project}/variables", s.APIAuth("viewer", s.apiProjectVars))
	m.HandleFunc("PUT /api/v1/projects/{project}/variables/{name}", s.APIAuth("developer", s.apiSetProjectVar))
	m.HandleFunc("DELETE /api/v1/projects/{project}/variables/{name}", s.APIAuth("developer", s.apiDeleteProjectVar))
	m.HandleFunc("POST /api/v1/projects/{project}/redeploy", s.APIAuth("developer", s.apiRedeployProject))
	m.HandleFunc("GET /api/v1/projects/{project}/connections", s.APIAuth("viewer", s.apiPeers))
	m.HandleFunc("POST /api/v1/projects/{project}/connections", s.APIAuth("admin", s.apiConnect))
	m.HandleFunc("DELETE /api/v1/projects/{project}/connections/{other}", s.APIAuth("admin", s.apiDisconnect))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /status/{org}/{project}", s.statusPage)
	m.HandleFunc("POST /o/{org}/p/{project}/status-page", s.Auth(web.AuthFull, s.toggleStatusPage))
	m.HandleFunc("POST /o/{org}/p/{project}/variables", s.Auth(web.AuthFull, s.setProjectVar))
	m.HandleFunc("POST /o/{org}/p/{project}/variables/{name}/delete", s.Auth(web.AuthFull, s.deleteProjectVar))
	m.HandleFunc("POST /o/{org}/p/{project}/redeploy", s.Auth(web.AuthFull, s.redeployProject))
	m.HandleFunc("POST /o/{org}/p/{project}/connections", s.Auth(web.AuthFull, s.projectConnection))
	m.HandleFunc("GET /o/{org}/p/{project}/map", s.Auth(web.AuthFull, s.projectMapFragment))
}
