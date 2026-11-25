package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.apiConsoleRoutes(m)
	s.consoleRoutes(m)
}

// apiConsoleRoutes: this component's part of the former apiConsoleRoutes table.
func (s *API) apiConsoleRoutes(m *http.ServeMux) {
	const p = "/api/v1/projects/{project}"
	m.HandleFunc("POST "+p+"/databases/{db}/attach", s.APIAuth("viewer", s.APIConsole(s.attachDatabase, nil)))
	m.HandleFunc("POST "+p+"/databases/{db}/public", s.APIAuth("viewer", s.APIConsole(s.databasePublic, nil)))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	const dbp = "/o/{org}/p/{project}/d/{db}"
	m.HandleFunc("GET "+dbp, s.Auth(web.AuthFull, s.databasePage))
	m.HandleFunc("GET "+dbp+"/settings", s.Auth(web.AuthFull, web.SettingsView(s.databasePage)))
	m.HandleFunc("POST "+dbp+"/settings", s.Auth(web.AuthFull, s.databaseSettings))
	m.HandleFunc("POST "+dbp+"/attach", s.Auth(web.AuthFull, s.attachDatabase))
	m.HandleFunc("POST "+dbp+"/public", s.Auth(web.AuthFull, s.databasePublic))
	m.HandleFunc("POST "+dbp+"/backups", s.Auth(web.AuthFull, s.databaseBackupNow))
	m.HandleFunc("POST "+dbp+"/backups/restore/{snap}", s.Auth(web.AuthFull, s.databaseRestore))
	m.HandleFunc("POST "+dbp+"/delete", s.Auth(web.AuthFull, s.deleteDatabase))
}
