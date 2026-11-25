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
	m.HandleFunc("POST "+p+"/functions/{fn}/triggers", s.APIAuth("viewer", s.APIConsole(s.addBucketTrigger, nil)))
	m.HandleFunc("POST "+p+"/functions/{fn}/webhook", s.APIAuth("viewer", s.APIConsole(s.functionWebhook, extractWebhook)))
}

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/projects/{project}/functions", s.APIAuth("viewer", s.apiFunctions))
	m.HandleFunc("PUT /api/v1/projects/{project}/functions/{fn}", s.APIAuth("developer", s.apiPutFunction))
	m.HandleFunc("DELETE /api/v1/projects/{project}/functions/{fn}", s.APIAuth("developer", s.apiDeleteFunction))
	m.HandleFunc("POST /api/v1/projects/{project}/functions/{fn}/invoke", s.APIAuth("developer", s.apiInvoke))
	m.HandleFunc("GET /api/v1/projects/{project}/functions/{fn}/invocations", s.APIAuth("viewer", s.apiInvocations))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	const fnp = "/o/{org}/p/{project}/f/{fn}"
	m.HandleFunc("GET /o/{org}/functions", s.Auth(web.AuthFull, s.functionsPage))
	m.HandleFunc("POST /o/{org}/functions", s.Auth(web.AuthFull, s.createFunction))
	m.HandleFunc("GET "+fnp, s.Auth(web.AuthFull, s.functionPage))
	m.HandleFunc("GET "+fnp+"/settings", s.Auth(web.AuthFull, web.SettingsView(s.functionPage)))
	m.HandleFunc("POST "+fnp+"/code", s.Auth(web.AuthFull, s.saveFunctionCode))
	m.HandleFunc("POST "+fnp+"/test", s.Auth(web.AuthFull, s.testFunction))
	m.HandleFunc("POST "+fnp+"/settings", s.Auth(web.AuthFull, s.functionSettings))
	m.HandleFunc("POST "+fnp+"/token", s.Auth(web.AuthFull, s.newFunctionToken))
	m.HandleFunc("POST "+fnp+"/delete", s.Auth(web.AuthFull, s.deleteFunction))
	m.HandleFunc("POST "+fnp+"/triggers", s.Auth(web.AuthFull, s.addBucketTrigger))
	m.HandleFunc("POST "+fnp+"/triggers/{id}/delete", s.Auth(web.AuthFull, s.deleteBucketTrigger))
	m.HandleFunc("POST "+fnp+"/webhook", s.Auth(web.AuthFull, s.functionWebhook))
	m.HandleFunc("POST /hooks/fn/{token}", s.functionWebhookCall)
}
