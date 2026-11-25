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
	m.HandleFunc("GET /api/v1/secrets", s.APIAuth("viewer", s.apiOrgSecrets))
	m.HandleFunc("POST /api/v1/secrets", s.APIAuth("developer", s.apiCreateOrgSecret))
	m.HandleFunc("GET /api/v1/secrets/{secret}", s.APIAuth("developer", s.apiGetOrgSecret))
	m.HandleFunc("PUT /api/v1/secrets/{secret}", s.APIAuth("developer", s.apiPutOrgSecret))
	m.HandleFunc("DELETE /api/v1/secrets/{secret}", s.APIAuth("developer", s.apiDeleteOrgSecret))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /o/{org}/secrets", s.Auth(web.AuthFull, s.orgSecretsPage))
	m.HandleFunc("POST /o/{org}/secrets", s.Auth(web.AuthFull, s.createOrgSecret))
	m.HandleFunc("GET /o/{org}/secrets/{secret}", s.Auth(web.AuthFull, s.orgSecretPage))
	m.HandleFunc("POST /o/{org}/secrets/{secret}/reveal", s.Auth(web.AuthFull, s.revealOrgSecret))
	m.HandleFunc("POST /o/{org}/secrets/{secret}/version", s.Auth(web.AuthFull, s.rotateOrgSecret))
	m.HandleFunc("POST /o/{org}/secrets/{secret}/delete", s.Auth(web.AuthFull, s.deleteOrgSecret))
}
