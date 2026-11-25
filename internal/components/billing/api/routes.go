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
	m.HandleFunc("GET /api/v1/costs", s.APIAuth("admin", s.apiCosts))
	m.HandleFunc("GET /api/v1/budgets", s.APIAuth("admin", s.apiBudgets))
	m.HandleFunc("POST /api/v1/budgets", s.APIAuth("admin", s.apiSaveBudget))
	m.HandleFunc("DELETE /api/v1/budgets/{id}", s.APIAuth("admin", s.apiDeleteBudget))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /o/{org}/billing", s.Auth(web.AuthFull, s.billingPage))
	m.HandleFunc("POST /o/{org}/billing/details", s.Auth(web.AuthFull, s.billingDetails))
	m.HandleFunc("POST /o/{org}/billing/budgets", s.Auth(web.AuthFull, s.saveBudget))
	m.HandleFunc("POST /o/{org}/billing/budgets/{id}/delete", s.Auth(web.AuthFull, s.deleteBudget))
	m.HandleFunc("GET /o/{org}/invoices/{number}", s.Auth(web.AuthFull, s.invoicePage))
}
