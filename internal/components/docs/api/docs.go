package api

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"platform/internal/core/web"
	"platform/internal/lib/config"
)

// Built-in documentation. Public (people read it before they can log in), rendered
// once at startup, with this installation's real console, registry and apps addresses
// filled in.
//
// /docs           index of every topic, grouped
// /docs/<topic>   one page per product or service (docs/user/<topic>.md)
// /docs/admin     the operator's guide
// /docs/user      the former single-page guide: sends old /docs/user#anchor links to
//                 the topic page that now covers them

//go:embed docs/*.md docs/user/*.md docs/diagrams/*.svg
var docsFS embed.FS

type docGroup struct {
	Name   string
	Topics []web.DocTopic
}

// docGroups is the table of contents of the documentation, in reading order.
var docGroups = []docGroup{
	{"Get started", []web.DocTopic{
		{Slug: "getting-started", Title: "Getting started", Summary: "Create your account, sign in safely and learn how organizations, projects and services fit together."},
		{Slug: "how-it-works", Title: "How the platform works", Summary: "The architecture under the hood: trust zones, the deploy loop, and what runs each feature."},
	}},
	{"Run applications", []web.DocTopic{
		{Slug: "applications", Title: "Applications", Summary: "Deploy containers from an image or from git, scale them, expose ports and roll back in one click."},
		{Slug: "builds", Title: "Builds from git", Summary: "Build images from your repository on every push, with or without a Dockerfile."},
		{Slug: "previews", Title: "Preview environments", Summary: "A temporary copy of your app for every branch or pull request."},
		{Slug: "scheduled-jobs", Title: "Scheduled jobs", Summary: "Run commands on a schedule: reports, clean-ups, imports."},
		{Slug: "variables", Title: "Variables and secrets", Summary: "Configure services with shared variables and encrypted secrets."},
		{Slug: "functions", Title: "Functions", Summary: "Run code on demand without a server: HTTP endpoints, schedules and queue consumers."},
		{Slug: "servers", Title: "Your own servers", Summary: "Run applications on your own machines at AWS, Azure or anywhere, behind the platform's proxy."},
	}},
	{"Data and messaging", []web.DocTopic{
		{Slug: "databases", Title: "Managed databases", Summary: "PostgreSQL, MySQL, Redis and more, with backups, users and connection details handled for you."},
		{Slug: "object-storage", Title: "Object storage", Summary: "S3-compatible buckets for files, uploads, backups and static websites."},
		{Slug: "queues", Title: "Queues", Summary: "Hand work from one service to another reliably, with retries and dead-letter queues."},
	}},
	{"Email", []web.DocTopic{
		{Slug: "email", Title: "Transactional email", Summary: "Send email from your applications through SMTP or the API, from your own domain."},
		{Slug: "mailboxes", Title: "Mail hosting", Summary: "Mailboxes, aliases and webmail on your own domain."},
	}},
	{"Networking", []web.DocTopic{
		{Slug: "domains", Title: "Custom domains and HTTPS", Summary: "Serve your apps on your own domain names with automatic certificates."},
		{Slug: "dns", Title: "DNS hosting", Summary: "Host your domain's DNS zone and manage its records."},
		{Slug: "cdn", Title: "Edge cache (CDN)", Summary: "Cache responses at the edge to make sites faster and lighten your apps."},
		{Slug: "private-network", Title: "Private network", Summary: "Let services of different projects talk to each other privately."},
		{Slug: "traffic-protection", Title: "Traffic protection", Summary: "Rate limits, IP allow and block lists, and protection against abuse."},
	}},
	{"Monitoring", []web.DocTopic{
		{Slug: "logs-and-metrics", Title: "Logs and metrics", Summary: "Live logs, search, resource graphs, and Prometheus and Grafana integration."},
		{Slug: "analytics", Title: "Analytics", Summary: "Privacy-friendly visitor statistics for your sites."},
		{Slug: "alerts", Title: "Alerts", Summary: "Be told by email or webhook when something goes wrong."},
		{Slug: "status-page", Title: "Status page", Summary: "A public page showing the health of your services."},
	}},
	{"Security and recovery", []web.DocTopic{
		{Slug: "registry", Title: "Image registry", Summary: "A private registry for your container images."},
		{Slug: "security-scanning", Title: "Security scanning", Summary: "Find known vulnerabilities in the images you run."},
		{Slug: "backups", Title: "Backups and restore", Summary: "Automatic backups of your data and how to restore them."},
	}},
	{"Account and automation", []web.DocTopic{
		{Slug: "team", Title: "Team and roles", Summary: "Invite people and decide what each of them can do."},
		{Slug: "single-sign-on", Title: "Single sign-on", Summary: "Let your team sign in with your company's identity provider."},
		{Slug: "account", Title: "Account security", Summary: "Passwords, two-step login, passkeys and sessions."},
		{Slug: "billing", Title: "Billing and usage", Summary: "Plans, usage, invoices and disk limits."},
		{Slug: "api-and-cli", Title: "API and command-line tool", Summary: "Automate everything with the REST API, API tokens and platformctl."},
		{Slug: "terraform", Title: "Terraform and OpenTofu", Summary: "Describe projects, applications, databases, domains and DNS as code, and apply them with one command."},
		{Slug: "good-practices", Title: "Limits and troubleshooting", Summary: "Platform limits, good practices and fixes for common problems."},
	}},
}

// adminTopic is the operator's guide, listed last.
var adminTopic = web.DocTopic{Slug: "admin", Title: "Running the platform", Summary: "Install, configure and operate your own installation (for operators).", Group: "Operators"}

func LoadDocs(cfg config.Config) (*web.DocsSite, error) {
	registry := cfg.RegistryHost
	if registry == "" {
		registry = "registry.example.com"
	}
	repl := strings.NewReplacer("{{console}}", cfg.BaseURL, "{{registry}}", registry, "{{apps}}", cfg.AppsDomain, "{{s3}}", cfg.S3PublicURL(), "{{s3region}}", cfg.S3Region)
	site := &web.DocsSite{Pages: map[string]web.DocPage{}}
	if svgs, err := fs.Glob(docsFS, "docs/diagrams/*.svg"); err == nil {
		for _, f := range svgs {
			raw, err := docsFS.ReadFile(f)
			if err != nil {
				return nil, err
			}
			docDiagrams[strings.TrimSuffix(path.Base(f), ".svg")] = template.HTML(raw) // embedded at build time
		}
	}
	for _, g := range docGroups {
		for _, t := range g.Topics {
			t.Group = g.Name
			raw, err := docsFS.ReadFile("docs/user/" + t.Slug + ".md")
			if err != nil {
				return nil, err
			}
			site.Pages[t.Slug] = renderMarkdown(repl.Replace(string(raw)))
			site.Order = append(site.Order, t)
		}
	}
	raw, err := docsFS.ReadFile("docs/admin.md")
	if err != nil {
		return nil, err
	}
	site.Pages["admin"] = renderMarkdown(repl.Replace(string(raw)))
	site.Order = append(site.Order, adminTopic)

	// Old links: keep the anchor when the topic page still has it.
	legacy := map[string]string{}
	for anchor, slug := range web.LegacyDocAnchors {
		url := "/docs/" + slug
		if web.HasAnchor(site.Pages[slug], anchor) {
			url += "#" + anchor
		}
		legacy[anchor] = url
	}
	b, _ := json.Marshal(legacy)
	site.Legacy = string(b)
	return site, nil
}

func (s *API) withDocsSession(r *http.Request) *http.Request {
	// Show the signed-in header when there is a fully authenticated session.
	if sess := s.LoadSession(r); sess != nil && sess.TOTPOK {
		r = r.WithContext(context.WithValue(r.Context(), web.SessKey, sess))
	}
	return r
}

func (s *API) docsIndex(w http.ResponseWriter, r *http.Request) {
	r = s.withDocsSession(r)
	s.Render(w, r, "docs_index", http.StatusOK, web.Data{"Groups": docGroups, "Admin": adminTopic, "Legacy": ""})
}

func (s *API) docsPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("guide")
	if slug == "user" {
		// The former single-page guide. The anchor never reaches the server, so the
		// page carries the map and app.js forwards #anchor to its new place.
		r = s.withDocsSession(r)
		s.Render(w, r, "docs_index", http.StatusOK, web.Data{"Groups": docGroups, "Admin": adminTopic, "Legacy": s.Docs.Legacy})
		return
	}
	page, ok := s.Docs.Pages[slug]
	if !ok {
		s.NotFound(w, r)
		return
	}
	var cur, prev, next web.DocTopic
	for i, t := range s.Docs.Order {
		if t.Slug == slug {
			cur = t
			if i > 0 {
				prev = s.Docs.Order[i-1]
			}
			if i+1 < len(s.Docs.Order) {
				next = s.Docs.Order[i+1]
			}
		}
	}
	r = s.withDocsSession(r)
	s.Render(w, r, "docs", http.StatusOK, web.Data{
		"Topic": cur, "DocTitle": page.Title, "Lede": page.Lede, "Body": page.HTML, "TOC": page.TOC,
		"Groups": docGroups, "Admin": adminTopic, "Prev": prev, "Next": next,
	})
}
