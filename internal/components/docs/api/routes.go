package api

import (
	"net/http"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.consoleRoutes(m)
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /docs", s.docsIndex)
	m.HandleFunc("GET /docs/{guide}", s.docsPage)
}
