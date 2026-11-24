package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.apiRoutes(m)
	s.apiInfraRoutes(m)
	s.consoleRoutes(m)
}

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	const svc = "/api/v1/projects/{project}/services/{service}"
	m.HandleFunc("GET /api/v1/me", s.APIAuth("viewer", s.apiMe))
	m.HandleFunc("GET /api/v1/projects", s.APIAuth("viewer", s.apiProjects))
	m.HandleFunc("GET /api/v1/projects/{project}/services", s.APIAuth("viewer", s.apiServices))
	m.HandleFunc("GET "+svc, s.APIAuth("viewer", s.apiService1))
	m.HandleFunc("POST "+svc+"/deploy", s.APIAuth("developer", s.apiDeploy))
	m.HandleFunc("POST "+svc+"/scale", s.APIAuth("developer", s.apiScale))
	m.HandleFunc("GET "+svc+"/deployments/{rev}", s.APIAuth("viewer", s.apiDeployment))
	m.HandleFunc("GET "+svc+"/logs", s.APIAuth("viewer", s.apiLogs))
	m.HandleFunc("PUT "+svc+"/secrets/{name}", s.APIAuth("developer", s.apiSetSecret))
}

// apiInfraRoutes: this component's part of the former apiInfraRoutes table.
func (s *API) apiInfraRoutes(m *http.ServeMux) {
	const proj = "/api/v1/projects/{project}"
	const svc = proj + "/services/{service}"
	const dbp = proj + "/databases/{db}"
	m.HandleFunc("POST /api/v1/projects", s.APIAuth("admin", s.apiCreateProject))
	m.HandleFunc("GET "+proj, s.APIAuth("viewer", s.apiProject1))
	m.HandleFunc("PATCH "+proj, s.APIAuth("admin", s.apiUpdateProject))
	m.HandleFunc("DELETE "+proj, s.APIAuth("admin", s.apiDeleteProject))
	m.HandleFunc("POST "+proj+"/services", s.APIAuth("developer", s.apiCreateService))
	m.HandleFunc("PATCH "+svc, s.APIAuth("developer", s.apiUpdateService))
	m.HandleFunc("DELETE "+svc, s.APIAuth("developer", s.apiDeleteService))
	m.HandleFunc("GET "+svc+"/secrets", s.APIAuth("viewer", s.apiServiceSecrets))
	m.HandleFunc("DELETE "+svc+"/secrets/{name}", s.APIAuth("developer", s.apiDeleteServiceSecret))
	m.HandleFunc("GET "+svc+"/domains", s.APIAuth("viewer", s.apiDomains))
	m.HandleFunc("POST "+svc+"/domains", s.APIAuth("developer", s.apiAddDomain))
	m.HandleFunc("GET "+svc+"/domains/{hostname}", s.APIAuth("viewer", s.apiDomain))
	m.HandleFunc("DELETE "+svc+"/domains/{hostname}", s.APIAuth("developer", s.apiDeleteDomain))
	m.HandleFunc("GET "+proj+"/databases", s.APIAuth("viewer", s.apiDatabases))
	m.HandleFunc("POST "+proj+"/databases", s.APIAuth("developer", s.apiCreateDatabase))
	m.HandleFunc("GET "+dbp, s.APIAuth("viewer", s.apiDatabase))
	m.HandleFunc("PATCH "+dbp, s.APIAuth("developer", s.apiUpdateDatabase))
	m.HandleFunc("DELETE "+dbp, s.APIAuth("admin", s.apiDeleteDatabase))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /o/{org}/api-tokens", s.Auth(web.AuthFull, s.createAPIToken))
	m.HandleFunc("POST /o/{org}/api-tokens/{id}/delete", s.Auth(web.AuthFull, s.deleteAPIToken))
}
