package web

import (
	"io/fs"
	"net/http"
)

// CoreInternalRoutes registers the core's own endpoints (the former internalRoutes table keeps the rest in the components).
func (s *Server) CoreInternalRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
}

// CoreRoutes registers the core's own endpoints (the former routes table keeps the rest in the components).
func (s *Server) CoreRoutes(m *http.ServeMux) {
	const svc = "/o/{org}/p/{project}/s/{service}"
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	m.HandleFunc("POST /o/{org}/domains", s.Auth(AuthFull, s.addDomainFromList))
	m.HandleFunc("POST /o/{org}/domains/actions", s.Auth(AuthFull, s.bulkActions("domains")))
	m.HandleFunc("POST /o/{org}/databases/actions", s.Auth(AuthFull, s.bulkActions("databases")))
	m.HandleFunc("POST /o/{org}/functions/actions", s.Auth(AuthFull, s.bulkActions("functions")))
	m.HandleFunc("POST /o/{org}/storage/actions", s.Auth(AuthFull, s.bulkActions("storage")))
	m.HandleFunc("GET /theme", s.setTheme)
	m.HandleFunc("POST /o/{org}/p/{project}/databases", s.Auth(AuthFull, s.CreateDatabase))
	m.HandleFunc("POST "+svc+"/domains", s.Auth(AuthFull, s.addDomain))
}
