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
	m.HandleFunc("GET /{$}", s.Auth(web.AuthFull, s.home))
	m.HandleFunc("GET /o/{org}", s.Auth(web.AuthFull, s.orgPage))
	m.HandleFunc("GET /o/{org}/services", s.Auth(web.AuthFull, s.servicesPage))
	m.HandleFunc("POST /o/{org}/pins", s.Auth(web.AuthFull, s.togglePin))
	m.HandleFunc("GET /o/{org}/scope", s.Auth(web.AuthFull, s.setScope))
	m.HandleFunc("GET /o/{org}/search", s.Auth(web.AuthFull, s.searchPage))
	m.HandleFunc("GET /o/{org}/projects", s.Auth(web.AuthFull, s.orgArea("projects", "viewer")))
	m.HandleFunc("GET /o/{org}/iam", s.Auth(web.AuthFull, s.orgArea("iam", "viewer")))
	m.HandleFunc("GET /o/{org}/registry", s.Auth(web.AuthFull, s.orgArea("registry", "viewer")))
	m.HandleFunc("GET /o/{org}/monitoring", s.Auth(web.AuthFull, s.orgArea("monitoring", "viewer")))
	m.HandleFunc("GET /o/{org}/compute", s.Auth(web.AuthFull, s.computePage))
	m.HandleFunc("GET /o/{org}/compute/jobs", s.Auth(web.AuthFull, s.computeJobsPage))
	m.HandleFunc("GET /o/{org}/compute/previews", s.Auth(web.AuthFull, s.computePreviewsPage))
	m.HandleFunc("GET /o/{org}/databases", s.Auth(web.AuthFull, s.databasesPage))
	m.HandleFunc("POST /o/{org}/databases", s.Auth(web.AuthFull, s.createDatabaseIn))
	m.HandleFunc("GET /o/{org}/domains", s.Auth(web.AuthFull, s.domainsPage))
	m.HandleFunc("GET /o/{org}/activity.json", s.Auth(web.AuthFull, s.activityJSON))
	m.HandleFunc("GET /o/{org}/audit", s.Auth(web.AuthFull, s.auditPage))
	m.HandleFunc("POST /o/{org}/projects", s.Auth(web.AuthFull, s.createProject))
	m.HandleFunc("POST /o/{org}/invites", s.Auth(web.AuthFull, s.createInvite))
	m.HandleFunc("POST /o/{org}/members/{user}/remove", s.Auth(web.AuthFull, s.removeMember))
	m.HandleFunc("POST /o/{org}/members/{user}/access", s.Auth(web.AuthFull, s.setMemberAccess))
	m.HandleFunc("GET /o/{org}/p/{project}", s.Auth(web.AuthFull, s.projectPage))
	m.HandleFunc("GET /o/{org}/p/{project}/settings", s.Auth(web.AuthFull, web.SettingsView(s.projectPage)))
	m.HandleFunc("POST /o/{org}/p/{project}/delete", s.Auth(web.AuthFull, s.deleteProject))
}
