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
	m.HandleFunc("POST /api/v1/queues/{queue}/attach", s.APIAuth("viewer", s.APIConsole(s.queueAttach, nil)))
}

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/queues", s.APIAuth("viewer", s.apiQueues))
	m.HandleFunc("POST /api/v1/queues", s.APIAuth("developer", s.apiCreateQueue))
	m.HandleFunc("DELETE /api/v1/queues/{queue}", s.APIAuth("admin", s.apiDeleteQueue))
	m.HandleFunc("POST /api/v1/queues/{queue}/messages", s.APIAuth("developer", s.apiSendMessages))
	m.HandleFunc("POST /api/v1/queues/{queue}/receive", s.APIAuth("developer", s.apiReceive))
	m.HandleFunc("DELETE /api/v1/queues/{queue}/messages/{receipt}", s.APIAuth("developer", s.apiDeleteMessage))
	m.HandleFunc("POST /api/v1/queues/{queue}/purge", s.APIAuth("developer", s.apiPurgeQueue))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /o/{org}/queues", s.Auth(web.AuthFull, s.queuesPage))
	m.HandleFunc("POST /o/{org}/queues", s.Auth(web.AuthFull, s.createQueue))
	m.HandleFunc("GET /o/{org}/queues/{queue}", s.Auth(web.AuthFull, s.queuePage))
	m.HandleFunc("GET /o/{org}/queues/{queue}/settings", s.Auth(web.AuthFull, web.SettingsView(s.queuePage)))
	m.HandleFunc("POST /o/{org}/queues/{queue}/send", s.Auth(web.AuthFull, s.queueSendConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/settings", s.Auth(web.AuthFull, s.queueSettingsConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/purge", s.Auth(web.AuthFull, s.queuePurgeConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/delete", s.Auth(web.AuthFull, s.queueDeleteConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/attach", s.Auth(web.AuthFull, s.queueAttach))
}
