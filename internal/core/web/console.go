package web

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/lib/model"
)

// The console is organized like a cloud provider's: a catalog of products ("services"
// such as Compute, Databases, Storage), each with its own area, left menu and resource
// lists across projects. Users pin the products they use to a favorites bar, and a
// project filter (kept in a cookie) narrows every list to one project.

type navItem struct {
	Label, Path string // Path relative to the organization, e.g. "/compute"
	Admin       bool   // shown to admins and owners only
}

type Product struct {
	ID, Name, Short, Group, Description string
	Path                                string // relative to the organization
	Nav                                 []navItem
	enabled                             func(s *Server) bool
}

var productGroups = []string{"Compute", "Data", "Networking", "Delivery", "Operations", "Management"}

var Products = []Product{
	{ID: "projects", Name: "Projects", Short: "Projects", Group: "Management", Path: "/projects",
		Description: "Group the resources of one application and see them together",
		Nav:         []navItem{{"Projects", "/projects", false}}},
	{ID: "compute", Name: "Compute", Short: "Compute", Group: "Compute", Path: "/compute",
		Description: "Run containers as web services and workers, with instances, jobs and previews",
		Nav:         []navItem{{"Services", "/compute", false}, {"Scheduled jobs", "/compute/jobs", false}, {"Previews", "/compute/previews", false}}},
	{ID: "servers", Name: "Your servers", Short: "Servers", Group: "Compute", Path: "/servers",
		Description: "Run your applications on your own machines (AWS, Azure, on-premises), behind the platform's proxy",
		Nav:         []navItem{{"Servers", "/servers", false}, {"Placement", "/servers#placement", false}}},
	{ID: "functions", Name: "Functions", Short: "Functions", Group: "Compute", Path: "/functions",
		Description: "Run code on demand without servers: function URLs, schedules and API calls",
		Nav:         []navItem{{"Functions", "/functions", false}}},
	{ID: "databases", Name: "Databases", Short: "Databases", Group: "Data", Path: "/databases",
		Description: "Managed PostgreSQL, MySQL, MariaDB, Redis, MongoDB and Kafka with nightly backups",
		Nav:         []navItem{{"Databases", "/databases", false}}},
	{ID: "storage", Name: "Object storage", Short: "Storage", Group: "Data", Path: "/storage",
		Description: "S3-compatible buckets for files, uploads, backups and static websites",
		Nav:         []navItem{{"Buckets", "/storage", false}, {"Access keys", "/storage#keys", false}, {"Use from code", "/storage#use", false}},
		enabled:     func(s *Server) bool { return s.Cfg.StorageEnabled() }},
	{ID: "domains", Name: "Domains", Short: "Domains", Group: "Networking", Path: "/domains",
		Description: "Custom domains with automatic HTTPS certificates",
		Nav:         []navItem{{"Custom domains", "/domains", false}}},
	{ID: "dns", Name: "DNS", Short: "DNS", Group: "Networking", Path: "/dns",
		Description: "Host your domains' DNS: zones delegated to the platform and their records",
		Nav:         []navItem{{"Hosted zones", "/dns", false}},
		enabled:     func(s *Server) bool { return s.DNSEnabled() }},
	{ID: "registry", Name: "Container registry", Short: "Registry", Group: "Delivery", Path: "/registry",
		Description: "Private image registry with vulnerability scanning",
		Nav:         []navItem{{"Registry", "/registry", false}, {"Tokens", "/registry#tokens", false}, {"Image scans", "/registry#scans", false}},
		enabled:     func(s *Server) bool { return s.Cfg.RegistryHost != "" }},
	{ID: "queues", Name: "Queues", Short: "Queues", Group: "Delivery", Path: "/queues",
		Description: "Message queues with dead-letter queues, consumed by your services or functions",
		Nav:         []navItem{{"Queues", "/queues", false}}},
	{ID: "email", Name: "Email", Short: "Email", Group: "Delivery", Path: "/email",
		Description: "Send email from your own domains with DKIM signing, a send API and a sending log",
		Nav:         []navItem{{"Sending log", "/email", false}, {"Domains", "/email#domains", false}, {"Use from code", "/email#connect", false}},
		enabled:     func(s *Server) bool { return s.EmailEnabled() }},
	{ID: "mail", Name: "Mailboxes", Short: "Mailboxes", Group: "Delivery", Path: "/mail",
		Description: "Mailboxes on your own domains: IMAP, POP3 and SMTP for any mail app, aliases and catch-all",
		Nav:         []navItem{{"Mail domains", "/mail", false}},
		enabled:     func(s *Server) bool { return s.MailHostingEnabled() }},
	{ID: "analytics", Name: "Analytics", Short: "Analytics", Group: "Operations", Path: "/analytics",
		Description: "Per-request traffic, latency and errors of each service, and visitor behavior from a cookieless script",
		Nav:         []navItem{{"Traffic", "/analytics", false}, {"Behavior", "/analytics#behavior", false}}},
	{ID: "monitoring", Name: "Monitoring", Short: "Monitoring", Group: "Operations", Path: "/monitoring",
		Description: "Health checks, incidents, alerts, logs across services and log export",
		Nav:         []navItem{{"Incidents", "/monitoring", false}, {"Logs", "/monitoring/logs", false}, {"Metric alarms", "/monitoring#alarms", false}, {"Notification channels", "/monitoring#channels", false}, {"Grafana and Prometheus", "/monitoring#grafana", false}}},
	{ID: "secrets", Name: "Secrets manager", Short: "Secrets", Group: "Management", Path: "/secrets",
		Description: "Encrypted, versioned secrets shared by your services and functions",
		Nav:         []navItem{{"Secrets", "/secrets", false}}},
	{ID: "iam", Name: "Access management", Short: "IAM", Group: "Management", Path: "/iam",
		Description: "Members and roles, API tokens and the audit log",
		Nav:         []navItem{{"Members", "/iam", false}, {"API tokens", "/iam#api", false}, {"Audit log", "/audit", true}}},
	{ID: "billing", Name: "Billing", Short: "Billing", Group: "Management", Path: "/billing",
		Description: "Usage this month, invoices and billing details",
		Nav:         []navItem{{"Usage and invoices", "/billing", true}}},
}

var defaultPins = []string{"compute", "functions", "databases", "storage", "monitoring"}

func ProductByID(id string) *Product {
	for i := range Products {
		if Products[i].ID == id {
			return &Products[i]
		}
	}
	return nil
}

func (s *Server) ProductEnabled(p *Product) bool { return p.enabled == nil || p.enabled(s) }

// areaOf names the product a console path belongs to.
func areaOf(orgPath, path string) string {
	rest := strings.TrimPrefix(path, orgPath)
	switch {
	case rest == "" || rest == "/":
		return ""
	case strings.HasPrefix(rest, "/p/"):
		parts := strings.SplitN(strings.TrimPrefix(rest, "/p/"), "/", 3)
		if len(parts) >= 2 {
			switch parts[1] {
			case "s":
				return "compute"
			case "d":
				return "databases"
			case "f":
				return "functions"
			}
		}
		return "projects"
	case strings.HasPrefix(rest, "/audit"):
		return "iam"
	}
	seg := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 2)[0]
	if ProductByID(seg) != nil {
		return seg
	}
	return ""
}

type productView struct {
	*Product
	Pinned bool
}

// loadPins returns the user's pinned products (enabled ones, in order).
func (s *Server) LoadPins(r *http.Request, userID string) []string {
	var customized bool
	_ = s.Pool.QueryRow(r.Context(), `SELECT pins_customized FROM users WHERE id = $1`, userID).Scan(&customized)
	ids := defaultPins
	if customized {
		rows, err := s.Pool.Query(r.Context(), `SELECT service FROM user_pins WHERE user_id = $1 ORDER BY position, created_at`, userID)
		if err == nil {
			ids, _ = pgx.CollectRows(rows, pgx.RowTo[string])
		}
	}
	var out []string
	for _, id := range ids {
		if p := ProductByID(id); p != nil && s.ProductEnabled(p) {
			out = append(out, id)
		}
	}
	return out
}

// addConsole fills the top bar, favorites bar and area menu of an organization page.
func (s *Server) addConsole(r *http.Request, d Data, o *OrgInfo) {
	sess := SessionFrom(r)
	if sess == nil {
		return
	}
	pins := s.LoadPins(r, sess.UserID)
	pinned := map[string]bool{}
	for _, id := range pins {
		pinned[id] = true
	}
	groups := map[string][]productView{}
	var favs []*Product
	for i := range Products {
		p := &Products[i]
		if !s.ProductEnabled(p) || o.Off[p.ID] {
			continue
		}
		groups[p.Group] = append(groups[p.Group], productView{p, pinned[p.ID]})
	}
	for _, id := range pins {
		if !o.Off[id] {
			favs = append(favs, ProductByID(id))
		}
	}
	type group struct {
		Name     string
		Products []productView
	}
	var catalog []group
	for _, g := range productGroups {
		if len(groups[g]) > 0 {
			catalog = append(catalog, group{g, groups[g]})
		}
	}
	area := ""
	if a, ok := d["Area"].(string); ok {
		area = a
	} else {
		area = areaOf(o.Path(), r.URL.Path)
	}
	if p := ProductByID(area); p != nil {
		d["AreaProduct"] = p
		var items []navItem
		for _, it := range p.Nav {
			if !it.Admin || model.RoleAtLeast(o.Role, "admin") {
				items = append(items, it)
			}
		}
		d["AreaNav"] = items
	}
	d["Area"], d["Catalog"], d["Favorites"], d["CurPath"] = area, catalog, favs, strings.TrimPrefix(r.URL.Path, o.Path())
	d["ScopeProject"] = s.ScopeProject(r, o)
	// Organization switcher.
	type orgRow struct{ Slug, Name string }
	if rows, err := s.Pool.Query(r.Context(), `
		SELECT o.slug, o.name FROM memberships m JOIN orgs o ON o.id = m.org_id WHERE m.user_id = $1 ORDER BY o.name`, sess.UserID); err == nil {
		d["MyOrgs"], _ = pgx.CollectRows(rows, pgx.RowToStructByPos[orgRow])
	}
}

// ---------- project filter ----------

func ScopeCookie(o *OrgInfo) string { return "scope_" + strings.ReplaceAll(o.Slug, "-", "_") }

// scopeProject is the project the lists are narrowed to ("" = all projects); a
// ?project= parameter wins over the cookie.
func (s *Server) ScopeProject(r *http.Request, o *OrgInfo) string {
	if q := r.URL.Query(); q.Has("project") {
		if p := q.Get("project"); p == "" || model.ValidSlug(p) {
			return p
		}
	}
	if c, err := r.Cookie(ScopeCookie(o)); err == nil && model.ValidSlug(c.Value) {
		return c.Value
	}
	return ""
}

type ProjectOption struct{ Slug, Name string }

func LoadProjectOptions(r *http.Request, tx pgx.Tx) ([]ProjectOption, error) {
	rows, err := tx.Query(r.Context(), `SELECT slug, name FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ProjectOption])
}

// ---------- icons ----------

// Simple line icons (24x24, stroke), drawn for this console.
var iconPaths = map[string]string{
	"projects":   `<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>`,
	"compute":    `<rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/>`,
	"databases":  `<ellipse cx="12" cy="5.5" rx="8" ry="2.5"/><path d="M4 5.5v13c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5v-13M4 12c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5"/>`,
	"storage":    `<path d="M4 6c0-1.1 3.6-2 8-2s8 .9 8 2l-1.6 13c-.2 1.1-3 2-6.4 2s-6.2-.9-6.4-2z"/><path d="M4 6c0 1.1 3.6 2 8 2s8-.9 8-2"/>`,
	"domains":    `<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18"/>`,
	"registry":   `<path d="M12 3l8 4.5v9L12 21l-8-4.5v-9z"/><path d="M4 7.5l8 4.5 8-4.5M12 12v9"/>`,
	"monitoring": `<path d="M3 12h4l3-8 4 16 3-8h4"/>`,
	"functions":  `<path d="M6 4h3l8 16"/><path d="M12.5 11L7 20"/>`,
	"queues":     `<rect x="3" y="5" width="18" height="4" rx="1"/><rect x="3" y="10" width="18" height="4" rx="1"/><rect x="3" y="15" width="18" height="4" rx="1"/>`,
	"analytics":  `<path d="M4 20V10M10 20V4M16 20v-7M22 20H2"/>`,
	"dns":        `<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.7 3.8 5.7 3.8 9s-1.3 6.3-3.8 9c-2.5-2.7-3.8-5.7-3.8-9S9.5 5.7 12 3z"/>`,
	"email":      `<rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3.5 6.5 8.5 6.5 8.5-6.5"/>`,
	"mail":       `<path d="M4 4h16v12H4z"/><path d="m4 7 8 5 8-5"/><path d="M8 20h8"/>`,
	"secrets":    `<rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/><circle cx="12" cy="15.5" r="1.5"/>`,
	"iam":        `<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c.8-3.6 3.4-5.5 6.5-5.5s5.7 1.9 6.5 5.5"/><path d="M16 4.5a3.5 3.5 0 0 1 0 7M18.5 14.8c1.6.8 2.6 2.4 3 5.2"/>`,
	"billing":    `<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 10h18M7 15h4"/>`,
	"grid":       `<rect x="4" y="4" width="6" height="6" rx="1"/><rect x="14" y="4" width="6" height="6" rx="1"/><rect x="4" y="14" width="6" height="6" rx="1"/><rect x="14" y="14" width="6" height="6" rx="1"/>`,
	"search":     `<circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/>`,
	"star":       `<path d="M12 3.5l2.6 5.3 5.9.9-4.3 4.1 1 5.8-5.2-2.7-5.2 2.7 1-5.8-4.3-4.1 5.9-.9z"/>`,
	"sun":        `<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>`,
	"moon":       `<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>`,
	"monitor":    `<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>`,
	"home":       `<path d="M4 11l8-7 8 7v8a1 1 0 0 1-1 1h-5v-6h-4v6H5a1 1 0 0 1-1-1z"/>`,
}

func icon(name string) template.HTML {
	p, ok := iconPaths[name]
	if !ok {
		p = iconPaths["grid"]
	}
	return template.HTML(`<svg class="ico" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + p + `</svg>`)
}

type crumb struct{ Label, Path string }

// crumbsFor builds the breadcrumb: product, project, then the resource.
func crumbsFor(d Data) []crumb {
	var out []crumb
	if p, ok := d["AreaProduct"].(*Product); ok && p.ID != "projects" {
		out = append(out, crumb{p.Name, p.Path})
	}
	if p, ok := d["Project"].(*ProjectInfo); ok && p != nil {
		if len(out) == 0 {
			out = append(out, crumb{"Projects", "/projects"})
		}
		out = append(out, crumb{p.Name, "/p/" + p.Slug})
	}
	switch {
	case d["Service"] != nil:
		if sv, ok := d["Service"].(*ServiceInfo); ok && sv != nil {
			out = append(out, crumb{sv.Slug, ""})
		}
	case d["Database"] != nil:
		if db, ok := d["Database"].(*DatabaseInfo); ok && db != nil {
			out = append(out, crumb{db.Slug, ""})
		}
	case d["Function"] != nil:
		if f, ok := d["Function"].(*FunctionInfo); ok && f != nil {
			out = append(out, crumb{f.Slug, ""})
		}
	case d["Bucket"] != nil:
		if b, ok := d["Bucket"].(*BucketInfo); ok && b != nil {
			out = append(out, crumb{b.Name, ""})
		}
	}
	if len(out) > 0 && out[len(out)-1].Path != "" && len(out) == 1 {
		out[0].Path = "" // product root: the current page
	}
	return out
}
