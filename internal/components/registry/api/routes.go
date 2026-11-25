package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.consoleRoutes(m)
}

// InternalRoutes registers this component's endpoints on the internal listener (never published).
func (s *API) InternalRoutes(m *http.ServeMux) {
	s.internalRoutes(m)
}

// internalRoutes: this component's part of the former internalRoutes table.
func (s *API) internalRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /internal/registry-events", s.registryEvents)
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /o/{org}/registry-tokens", s.Auth(web.AuthFull, s.createRegistryToken))
	m.HandleFunc("POST /o/{org}/registry-tokens/{id}/delete", s.Auth(web.AuthFull, s.deleteRegistryToken))
	m.HandleFunc("GET /token", s.registryToken)
}
