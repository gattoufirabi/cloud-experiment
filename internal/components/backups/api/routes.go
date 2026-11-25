package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.consoleRoutes(m)
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	const svc = "/o/{org}/p/{project}/s/{service}"
	m.HandleFunc("POST "+svc+"/backups", s.Auth(web.AuthFull, s.backupNow))
	m.HandleFunc("POST "+svc+"/backups/settings", s.Auth(web.AuthFull, s.backupSettings))
	m.HandleFunc("POST "+svc+"/backups/restore/{snap}", s.Auth(web.AuthFull, s.restoreSnapshot))
}
