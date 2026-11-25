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
	m.HandleFunc("GET /api/v1/dns/zones", s.APIAuth("viewer", s.apiDNSZones))
	m.HandleFunc("POST /api/v1/dns/zones", s.APIAuth("developer", s.apiAddDNSZone))
	m.HandleFunc("DELETE /api/v1/dns/zones/{zone}", s.APIAuth("admin", s.apiDeleteDNSZone))
	m.HandleFunc("GET /api/v1/dns/zones/{zone}/records", s.APIAuth("viewer", s.apiDNSRecords))
	m.HandleFunc("POST /api/v1/dns/zones/{zone}/records", s.APIAuth("developer", s.apiAddDNSRecord))
	m.HandleFunc("POST /api/v1/dns/zones/{zone}/import", s.APIAuth("developer", s.apiImportZone))
	m.HandleFunc("DELETE /api/v1/dns/zones/{zone}/records/{id}", s.APIAuth("developer", s.apiDeleteDNSRecord))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /o/{org}/dns", s.Auth(web.AuthFull, s.dnsPage))
	m.HandleFunc("POST /o/{org}/dns", s.Auth(web.AuthFull, s.addZone))
	m.HandleFunc("GET /o/{org}/dns/{zone}", s.Auth(web.AuthFull, s.zonePage))
	m.HandleFunc("POST /o/{org}/dns/{zone}", s.Auth(web.AuthFull, s.zoneAction))
}
