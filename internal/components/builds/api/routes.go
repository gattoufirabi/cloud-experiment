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
	const svc = "/api/v1/projects/{project}/services/{service}"
	m.HandleFunc("GET "+svc+"/source", s.APIAuth("viewer", s.apiSource))
	m.HandleFunc("PUT "+svc+"/source", s.APIAuth("developer", s.apiPutSource))
	m.HandleFunc("GET /api/v1/servers", s.APIAuth("viewer", s.apiServers))
	m.HandleFunc("GET /api/v1/servers/{id}", s.APIAuth("viewer", s.apiServer))
	m.HandleFunc("PUT /api/v1/servers/{id}/builds", s.APIAuth("admin", s.apiPutServerBuilds))
	m.HandleFunc("POST "+svc+"/builds", s.APIAuth("developer", s.apiStartBuild))
	m.HandleFunc("POST "+svc+"/zip", s.APIAuth("developer", s.apiZipDeploy))
	m.HandleFunc("GET "+svc+"/builds/{n}", s.APIAuth("viewer", s.apiBuild))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	const svc = "/o/{org}/p/{project}/s/{service}"
	m.HandleFunc("POST /hooks/build/{token}", s.buildHook)
	m.HandleFunc("POST "+svc+"/source", s.Auth(web.AuthFull, s.saveSource))
	m.HandleFunc("POST "+svc+"/builds", s.Auth(web.AuthFull, s.startBuildForm))
	m.HandleFunc("POST "+svc+"/zip", s.Auth(web.AuthFull, s.zipDeployForm))
	m.HandleFunc("GET "+svc+"/builds/{n}", s.Auth(web.AuthFull, s.buildPage))
	m.HandleFunc("POST "+svc+"/build-hook", s.Auth(web.AuthFull, s.rotateBuildHook))
}
