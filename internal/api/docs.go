package api

import (
	"context"
	"embed"
	"net/http"
	"strings"

	"platform/internal/config"
)

// Built-in documentation at /docs/user and /docs/admin. Public (people read it before
// they can log in), rendered once at startup, with this installation's real console,
// registry and apps addresses filled in.

//go:embed docs/*.md
var docsFS embed.FS

func loadDocs(cfg config.Config) (map[string]docPage, error) {
	registry := cfg.RegistryHost
	if registry == "" {
		registry = "registry.example.com"
	}
	repl := strings.NewReplacer("{{console}}", cfg.BaseURL, "{{registry}}", registry, "{{apps}}", cfg.AppsDomain)
	pages := map[string]docPage{}
	for _, name := range []string{"user", "admin"} {
		raw, err := docsFS.ReadFile("docs/" + name + ".md")
		if err != nil {
			return nil, err
		}
		pages[name] = renderMarkdown(repl.Replace(string(raw)))
	}
	return pages, nil
}

func (s *Server) docsPage(w http.ResponseWriter, r *http.Request) {
	guide := r.PathValue("guide")
	page, ok := s.docs[guide]
	if !ok {
		s.notFound(w, r)
		return
	}
	// Show the signed-in header when there is a fully authenticated session.
	if sess := s.loadSession(r); sess != nil && sess.TOTPOK {
		r = r.WithContext(context.WithValue(r.Context(), sessKey, sess))
	}
	s.render(w, r, "docs", http.StatusOK, data{"Guide": guide, "DocTitle": page.Title, "Body": page.HTML, "TOC": page.TOC})
}
