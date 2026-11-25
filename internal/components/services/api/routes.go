package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.apiConsoleRoutes(m)
	s.apiRoutes(m)
	s.consoleRoutes(m)
}

// apiConsoleRoutes: this component's part of the former apiConsoleRoutes table.
func (s *API) apiConsoleRoutes(m *http.ServeMux) {
	const p = "/api/v1/projects/{project}"
	m.HandleFunc("PUT "+p+"/services/{service}/settings", s.APIAuth("viewer", s.APIConsole(s.saveSettings, nil)))
	m.HandleFunc("POST "+p+"/services/{service}/default-address", s.APIAuth("viewer", s.APIConsole(s.setDefaultAddress, nil)))
}

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	const svc = "/api/v1/projects/{project}/services/{service}"
	m.HandleFunc("GET "+svc+"/ports", s.APIAuth("viewer", s.apiServicePorts))
	m.HandleFunc("POST "+svc+"/ports", s.APIAuth("developer", s.apiAddServicePort))
	m.HandleFunc("DELETE "+svc+"/ports/{label}", s.APIAuth("developer", s.apiDeleteServicePort))
	m.HandleFunc("GET "+svc+"/schedules", s.APIAuth("viewer", s.apiSchedules))
	m.HandleFunc("POST "+svc+"/schedules", s.APIAuth("developer", s.apiAddSchedule))
	m.HandleFunc("DELETE "+svc+"/schedules/{id}", s.APIAuth("developer", s.apiDeleteSchedule))
	m.HandleFunc("GET "+svc+"/previews", s.APIAuth("viewer", s.apiPreviews))
	m.HandleFunc("GET "+svc+"/jobs", s.APIAuth("viewer", s.apiJobs))
	m.HandleFunc("PUT "+svc+"/jobs/{job}", s.APIAuth("developer", s.apiPutJob))
	m.HandleFunc("DELETE "+svc+"/jobs/{job}", s.APIAuth("developer", s.apiDeleteJob))
	m.HandleFunc("POST "+svc+"/jobs/{job}/run", s.APIAuth("developer", s.apiRunJob))
	m.HandleFunc("GET "+svc+"/jobs/{job}/runs", s.APIAuth("viewer", s.apiJobRuns))
	m.HandleFunc("GET "+svc+"/jobs/{job}/runs/{run}", s.APIAuth("viewer", s.apiJobRun))
	m.HandleFunc("POST "+svc+"/previews", s.APIAuth("developer", s.apiCreatePreview))
	m.HandleFunc("DELETE "+svc+"/previews/{name}", s.APIAuth("developer", s.apiDeletePreview))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	const svc = "/o/{org}/p/{project}/s/{service}"
	m.HandleFunc("POST /o/{org}/compute", s.Auth(web.AuthFull, s.createServiceFull))
	m.HandleFunc("POST /o/{org}/compute/actions", s.Auth(web.AuthFull, s.computeActions))
	m.HandleFunc("POST /o/{org}/p/{project}/services", s.Auth(web.AuthFull, s.createService))
	m.HandleFunc("GET "+svc, s.Auth(web.AuthFull, s.servicePage))
	m.HandleFunc("GET "+svc+"/settings", s.Auth(web.AuthFull, web.SettingsView(s.servicePage)))
	m.HandleFunc("POST "+svc+"/settings", s.Auth(web.AuthFull, s.saveSettings))
	m.HandleFunc("POST "+svc+"/deploy", s.Auth(web.AuthFull, s.deployService))
	m.HandleFunc("POST "+svc+"/rollback/{rev}", s.Auth(web.AuthFull, s.rollback))
	m.HandleFunc("POST "+svc+"/secrets", s.Auth(web.AuthFull, s.setSecret))
	m.HandleFunc("POST "+svc+"/jobs", s.Auth(web.AuthFull, s.saveJobForm))
	m.HandleFunc("POST "+svc+"/schedules", s.Auth(web.AuthFull, s.addScheduleForm))
	m.HandleFunc("POST "+svc+"/schedules/{id}/delete", s.Auth(web.AuthFull, s.deleteScheduleForm))
	m.HandleFunc("POST "+svc+"/jobs/{job}/{action}", s.Auth(web.AuthFull, s.jobAction))
	m.HandleFunc("GET "+svc+"/job-runs/{run}", s.Auth(web.AuthFull, s.jobRunLog))
	m.HandleFunc("POST "+svc+"/secrets/{name}/delete", s.Auth(web.AuthFull, s.deleteSecret))
	m.HandleFunc("POST "+svc+"/default-address", s.Auth(web.AuthFull, s.setDefaultAddress))
	m.HandleFunc("POST "+svc+"/domains/{id}/verify", s.Auth(web.AuthFull, s.verifyDomain))
	m.HandleFunc("POST "+svc+"/domains/{id}/delete", s.Auth(web.AuthFull, s.deleteDomain))
	m.HandleFunc("POST "+svc+"/domains/{id}/paths", s.Auth(web.AuthFull, s.addDomainPathForm))
	m.HandleFunc("POST "+svc+"/domains/paths/{path}/delete", s.Auth(web.AuthFull, s.deleteDomainPathForm))
	m.HandleFunc("POST "+svc+"/ports", s.Auth(web.AuthFull, s.addServicePortForm))
	m.HandleFunc("POST "+svc+"/ports/{label}/delete", s.Auth(web.AuthFull, s.deleteServicePortForm))
	m.HandleFunc("POST "+svc+"/delete", s.Auth(web.AuthFull, s.deleteService))
	m.HandleFunc("POST "+svc+"/protection", s.Auth(web.AuthFull, s.protectionSettings))
	m.HandleFunc("POST "+svc+"/previews", s.Auth(web.AuthFull, s.createPreviewForm))
	m.HandleFunc("POST "+svc+"/previews/{name}/delete", s.Auth(web.AuthFull, s.deletePreviewForm))
}
