package api

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
)

// The console is organized like a cloud provider's: a catalog of products ("services"
// such as Compute, Databases, Storage), each with its own area, left menu and resource
// lists across projects. Users pin the products they use to a favorites bar, and a
// project filter (kept in a cookie) narrows every list to one project.

type navItem struct {
	Label, Path string // Path relative to the organization, e.g. "/compute"
	Admin       bool   // shown to admins and owners only
}

type product struct {
	ID, Name, Short, Group, Description string
	Path                                string // relative to the organization
	Nav                                 []navItem
	enabled                             func(s *Server) bool
}

var productGroups = []string{"Compute", "Data", "Networking", "Delivery", "Operations", "Management"}

var products = []product{
	{ID: "projects", Name: "Projects", Short: "Projects", Group: "Management", Path: "/projects",
		Description: "Group the resources of one application and see them together",
		Nav:         []navItem{{"Projects", "/projects", false}}},
	{ID: "compute", Name: "Compute", Short: "Compute", Group: "Compute", Path: "/compute",
		Description: "Run containers as web services and workers, with instances, jobs and previews",
		Nav:         []navItem{{"Services", "/compute", false}, {"Scheduled jobs", "/compute/jobs", false}, {"Previews", "/compute/previews", false}}},
	{ID: "functions", Name: "Functions", Short: "Functions", Group: "Compute", Path: "/functions",
		Description: "Run code on demand without servers: function URLs, schedules and API calls",
		Nav:         []navItem{{"Functions", "/functions", false}}},
	{ID: "databases", Name: "Databases", Short: "Databases", Group: "Data", Path: "/databases",
		Description: "Managed PostgreSQL, MySQL, MariaDB and Redis with nightly backups",
		Nav:         []navItem{{"Databases", "/databases", false}}},
	{ID: "storage", Name: "Object storage", Short: "Storage", Group: "Data", Path: "/storage",
		Description: "S3-compatible buckets for files, uploads, backups and static websites",
		Nav:         []navItem{{"Buckets", "/storage", false}, {"Access keys", "/storage#keys", false}, {"Use from code", "/storage#use", false}},
		enabled:     func(s *Server) bool { return s.cfg.StorageEnabled() }},
	{ID: "domains", Name: "Domains", Short: "Domains", Group: "Networking", Path: "/domains",
		Description: "Custom domains with automatic HTTPS certificates",
		Nav:         []navItem{{"Custom domains", "/domains", false}}},
	{ID: "dns", Name: "DNS", Short: "DNS", Group: "Networking", Path: "/dns",
		Description: "Host your domains' DNS: zones delegated to the platform and their records",
		Nav:         []navItem{{"Hosted zones", "/dns", false}},
		enabled:     func(s *Server) bool { return s.dnsEnabled() }},
	{ID: "registry", Name: "Container registry", Short: "Registry", Group: "Delivery", Path: "/registry",
		Description: "Private image registry with vulnerability scanning",
		Nav:         []navItem{{"Registry", "/registry", false}, {"Tokens", "/registry#tokens", false}, {"Image scans", "/registry#scans", false}},
		enabled:     func(s *Server) bool { return s.cfg.RegistryHost != "" }},
	{ID: "queues", Name: "Queues", Short: "Queues", Group: "Delivery", Path: "/queues",
		Description: "Message queues with dead-letter queues, consumed by your services or functions",
		Nav:         []navItem{{"Queues", "/queues", false}}},
	{ID: "email", Name: "Email", Short: "Email", Group: "Delivery", Path: "/email",
		Description: "Send email from your own domains with DKIM signing, a send API and a sending log",
		Nav:         []navItem{{"Sending log", "/email", false}, {"Domains", "/email#domains", false}, {"Use from code", "/email#connect", false}},
		enabled:     func(s *Server) bool { return s.emailEnabled() }},
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

func productByID(id string) *product {
	for i := range products {
		if products[i].ID == id {
			return &products[i]
		}
	}
	return nil
}

func (s *Server) productEnabled(p *product) bool { return p.enabled == nil || p.enabled(s) }

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
	if productByID(seg) != nil {
		return seg
	}
	return ""
}

type productView struct {
	*product
	Pinned bool
}

// loadPins returns the user's pinned products (enabled ones, in order).
func (s *Server) loadPins(r *http.Request, userID string) []string {
	var customized bool
	_ = s.pool.QueryRow(r.Context(), `SELECT pins_customized FROM users WHERE id = $1`, userID).Scan(&customized)
	ids := defaultPins
	if customized {
		rows, err := s.pool.Query(r.Context(), `SELECT service FROM user_pins WHERE user_id = $1 ORDER BY position, created_at`, userID)
		if err == nil {
			ids, _ = pgx.CollectRows(rows, pgx.RowTo[string])
		}
	}
	var out []string
	for _, id := range ids {
		if p := productByID(id); p != nil && s.productEnabled(p) {
			out = append(out, id)
		}
	}
	return out
}

// addConsole fills the top bar, favorites bar and area menu of an organization page.
func (s *Server) addConsole(r *http.Request, d data, o *orgInfo) {
	sess := sessionFrom(r)
	if sess == nil {
		return
	}
	pins := s.loadPins(r, sess.UserID)
	pinned := map[string]bool{}
	for _, id := range pins {
		pinned[id] = true
	}
	groups := map[string][]productView{}
	var favs []*product
	for i := range products {
		p := &products[i]
		if !s.productEnabled(p) {
			continue
		}
		groups[p.Group] = append(groups[p.Group], productView{p, pinned[p.ID]})
	}
	for _, id := range pins {
		favs = append(favs, productByID(id))
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
	if p := productByID(area); p != nil {
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
	d["ScopeProject"] = s.scopeProject(r, o)
	// Organization switcher.
	type orgRow struct{ Slug, Name string }
	if rows, err := s.pool.Query(r.Context(), `
		SELECT o.slug, o.name FROM memberships m JOIN orgs o ON o.id = m.org_id WHERE m.user_id = $1 ORDER BY o.name`, sess.UserID); err == nil {
		d["MyOrgs"], _ = pgx.CollectRows(rows, pgx.RowToStructByPos[orgRow])
	}
}

// ---------- project filter ----------

func scopeCookie(o *orgInfo) string { return "scope_" + strings.ReplaceAll(o.Slug, "-", "_") }

// scopeProject is the project the lists are narrowed to ("" = all projects); a
// ?project= parameter wins over the cookie.
func (s *Server) scopeProject(r *http.Request, o *orgInfo) string {
	if q := r.URL.Query(); q.Has("project") {
		if p := q.Get("project"); p == "" || model.ValidSlug(p) {
			return p
		}
	}
	if c, err := r.Cookie(scopeCookie(o)); err == nil && model.ValidSlug(c.Value) {
		return c.Value
	}
	return ""
}

// setScope remembers the project filter and returns to the page it was chosen on.
func (s *Server) setScope(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	p := r.URL.Query().Get("project")
	c := &http.Cookie{Name: scopeCookie(o), Value: p, Path: o.Path(), HttpOnly: true, Secure: s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode, MaxAge: 180 * 24 * 3600}
	if p == "" || !model.ValidSlug(p) {
		c.Value, c.MaxAge = "", -1
	}
	http.SetCookie(w, c)
	dest := r.URL.Query().Get("back")
	if !strings.HasPrefix(dest, o.Path()) || strings.Contains(dest, "//") {
		dest = o.Path()
	}
	u, _ := url.Parse(dest)
	if u == nil {
		u = &url.URL{Path: o.Path()}
	}
	q := u.Query()
	q.Del("project")
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// ---------- pins ----------

// togglePin pins or unpins a product for the signed-in user.
func (s *Server) togglePin(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	id := r.PostFormValue("product")
	p := productByID(id)
	if p == nil {
		s.notFound(w, r)
		return
	}
	uid := sessionFrom(r).UserID
	pins := s.loadPins(r, uid)
	var next []string
	found := false
	for _, x := range pins {
		if x == id {
			found = true
			continue
		}
		next = append(next, x)
	}
	if !found {
		next = append(next, id)
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `DELETE FROM user_pins WHERE user_id = $1`, uid); err != nil {
		s.serverError(w, r, err)
		return
	}
	for i, x := range next {
		if _, err := tx.Exec(r.Context(), `INSERT INTO user_pins (user_id, service, position) VALUES ($1, $2, $3)`, uid, x, i); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE users SET pins_customized = true WHERE id = $1`, uid); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.serverError(w, r, err)
		return
	}
	dest := r.PostFormValue("back")
	if !strings.HasPrefix(dest, o.Path()) || strings.Contains(dest, "//") {
		dest = o.Path() + "/services"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// servicesPage is the full product catalog, where products are pinned.
func (s *Server) servicesPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	s.render(w, r, "services", http.StatusOK, data{"Org": o, "Area": ""})
}

// ---------- area pages built from the organization data ----------

func (s *Server) orgArea(page, minRole string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, ok := s.org(w, r, minRole)
		if !ok {
			return
		}
		s.renderOrg(w, r, o, http.StatusOK, data{"Page": page})
	}
}

// ---------- resource lists across projects ----------

type serviceRow struct {
	Project, ProjectName, Slug, Image, Status string
	Instances                                 int
	Worker                                    bool
	Updated                                   *time.Time
}

func (s *Server) computePage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.scopeProject(r, o)
	var rows []serviceRow
	var projects []projectOption
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if projects, err = loadProjectOptions(r, tx); err != nil {
			return err
		}
		q, err := tx.Query(r.Context(), `
			SELECT p.slug, p.name, s.slug, s.image,
			       coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'not deployed'),
			       s.instances, s.worker,
			       (SELECT max(d.created_at) FROM deployments d WHERE d.service_id = s.id)
			FROM services s JOIN projects p ON p.id = s.project_id
			WHERE s.preview_of IS NULL AND ($1 = '' OR p.slug = $1)
			ORDER BY p.name, s.slug`, scope)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(q, pgx.RowToStructByPos[serviceRow])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "compute", http.StatusOK, data{"Org": o, "Services": rows, "Projects": projects, "Scope": scope,
		"CanEdit": model.RoleAtLeast(o.Role, "developer"), "AppsDomain": s.cfg.AppsDomain})
}

type projectOption struct{ Slug, Name string }

func loadProjectOptions(r *http.Request, tx pgx.Tx) ([]projectOption, error) {
	rows, err := tx.Query(r.Context(), `SELECT slug, name FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[projectOption])
}

type databaseRow struct {
	Project, ProjectName, Slug, Engine, Version, Status string
	CPUMillis, MemoryMB                                 int
	Created                                             time.Time
}

func (s *Server) databasesPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.scopeProject(r, o)
	var rows []databaseRow
	var projects []projectOption
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if projects, err = loadProjectOptions(r, tx); err != nil {
			return err
		}
		q, err := tx.Query(r.Context(), `
			SELECT p.slug, p.name, d.slug, d.engine, d.version, d.status, d.cpu_millis, d.memory_mb, d.created_at
			FROM databases d JOIN projects p ON p.id = d.project_id
			WHERE $1 = '' OR p.slug = $1 ORDER BY p.name, d.slug`, scope)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(q, pgx.RowToStructByPos[databaseRow])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "databases", http.StatusOK, data{"Org": o, "Databases": rows, "Projects": projects, "Scope": scope,
		"Engines": model.Engines, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

type domainRow struct {
	Hostname, Project, Service string
	Verified                   *time.Time
	Created                    time.Time
}

func (s *Server) domainsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.scopeProject(r, o)
	var rows []domainRow
	var webServices []string
	var maxDomains int
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		q, err := tx.Query(r.Context(), `
			SELECT dm.hostname, p.slug, s.slug, dm.verified_at, dm.created_at
			FROM domains dm JOIN services s ON s.id = dm.service_id JOIN projects p ON p.id = s.project_id
			WHERE $1 = '' OR p.slug = $1 ORDER BY dm.hostname`, scope)
		if err != nil {
			return err
		}
		if rows, err = pgx.CollectRows(q, pgx.RowToStructByPos[domainRow]); err != nil {
			return err
		}
		q, err = tx.Query(r.Context(), `SELECT p.slug || '/' || s.slug FROM services s JOIN projects p ON p.id = s.project_id
			WHERE s.preview_of IS NULL AND NOT s.worker AND ($1 = '' OR p.slug = $1) ORDER BY 1`, scope)
		if err != nil {
			return err
		}
		if webServices, err = pgx.CollectRows(q, pgx.RowTo[string]); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT max_domains FROM plans WHERE id = $1`, o.PlanID).Scan(&maxDomains)
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "domains", http.StatusOK, data{"Org": o, "Domains": rows, "Scope": scope, "MaxDomains": maxDomains, "AppsDomain": s.cfg.AppsDomain,
		"WebServices": webServices, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

// createDatabaseIn creates a database in the project chosen in the form (the area
// pages are not inside a project).
func (s *Server) createDatabaseIn(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("project", r.PostFormValue("project"))
	s.createDatabase(w, r)
}

// ---------- search ----------

type searchHit struct {
	Kind, Name, Detail, Path string
}

func (s *Server) searchPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var hits []searchHit
	var prods []*product
	if q != "" {
		lq := strings.ToLower(q)
		for i := range products {
			p := &products[i]
			if s.productEnabled(p) && (strings.Contains(strings.ToLower(p.Name+" "+p.Short+" "+p.Description), lq)) {
				prods = append(prods, p)
			}
		}
		like := "%" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(lq) + "%"
		err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
			rows, err := tx.Query(r.Context(), `
				SELECT 'Project', p.name, p.slug, '/p/' || p.slug FROM projects p WHERE lower(p.slug) LIKE $1 OR lower(p.name) LIKE $1
				UNION ALL
				SELECT 'Service', s.slug, p.slug || ' · ' || s.image, '/p/' || p.slug || '/s/' || s.slug
				FROM services s JOIN projects p ON p.id = s.project_id WHERE s.preview_of IS NULL AND (lower(s.slug) LIKE $1 OR lower(s.image) LIKE $1)
				UNION ALL
				SELECT 'Database', d.slug, p.slug || ' · ' || d.engine || ' ' || d.version, '/p/' || p.slug || '/d/' || d.slug
				FROM databases d JOIN projects p ON p.id = d.project_id WHERE lower(d.slug) LIKE $1 OR lower(d.engine) LIKE $1
				UNION ALL
				SELECT 'Function', f.slug, p.slug || ' · ' || f.runtime, '/p/' || p.slug || '/f/' || f.slug
				FROM functions f JOIN projects p ON p.id = f.project_id WHERE lower(f.slug) LIKE $1
				UNION ALL
				SELECT 'Queue', q.name, 'queue', '/queues/' || q.name FROM queues q WHERE lower(q.name) LIKE $1
				UNION ALL
				SELECT 'Bucket', b.name, b.full_name, '/storage/b/' || b.name FROM buckets b WHERE lower(b.full_name) LIKE $1
				UNION ALL
				SELECT 'Domain', dm.hostname, p.slug || '/' || s.slug, '/p/' || p.slug || '/s/' || s.slug || '#domains'
				FROM domains dm JOIN services s ON s.id = dm.service_id JOIN projects p ON p.id = s.project_id WHERE dm.hostname LIKE $1
				LIMIT 50`, like)
			if err != nil {
				return err
			}
			hits, err = pgx.CollectRows(rows, pgx.RowToStructByPos[searchHit])
			return err
		})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, r, "search", http.StatusOK, data{"Org": o, "Q": q, "Hits": hits, "Products": prods})
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
	"secrets":    `<rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/><circle cx="12" cy="15.5" r="1.5"/>`,
	"iam":        `<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c.8-3.6 3.4-5.5 6.5-5.5s5.7 1.9 6.5 5.5"/><path d="M16 4.5a3.5 3.5 0 0 1 0 7M18.5 14.8c1.6.8 2.6 2.4 3 5.2"/>`,
	"billing":    `<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 10h18M7 15h4"/>`,
	"grid":       `<rect x="4" y="4" width="6" height="6" rx="1"/><rect x="14" y="4" width="6" height="6" rx="1"/><rect x="4" y="14" width="6" height="6" rx="1"/><rect x="14" y="14" width="6" height="6" rx="1"/>`,
	"search":     `<circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/>`,
	"star":       `<path d="M12 3.5l2.6 5.3 5.9.9-4.3 4.1 1 5.8-5.2-2.7-5.2 2.7 1-5.8-4.3-4.1 5.9-.9z"/>`,
	"home":       `<path d="M4 11l8-7 8 7v8a1 1 0 0 1-1 1h-5v-6h-4v6H5a1 1 0 0 1-1-1z"/>`,
}

func icon(name string) template.HTML {
	p, ok := iconPaths[name]
	if !ok {
		p = iconPaths["grid"]
	}
	return template.HTML(`<svg class="ico" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + p + `</svg>`)
}

type jobRow struct {
	Project, Service, Slug, Schedule, Command string
	Enabled                                   bool
	NextRun                                   *time.Time
	LastStatus                                string
	LastRun                                   *time.Time
}

func (s *Server) computeJobsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.scopeProject(r, o)
	var rows []jobRow
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		q, err := tx.Query(r.Context(), `
			SELECT p.slug, s.slug, j.slug, j.schedule, j.command, j.enabled, j.next_run_at,
			       coalesce(lr.status, ''), lr.started_at
			FROM jobs j JOIN services s ON s.id = j.service_id JOIN projects p ON p.id = s.project_id
			LEFT JOIN LATERAL (SELECT status, started_at FROM job_runs r WHERE r.job_id = j.id ORDER BY started_at DESC LIMIT 1) lr ON true
			WHERE $1 = '' OR p.slug = $1 ORDER BY p.slug, s.slug, j.slug`, scope)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(q, pgx.RowToStructByPos[jobRow])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "jobs", http.StatusOK, data{"Org": o, "Jobs": rows, "Scope": scope})
}

type previewRow struct {
	Project, Service, Name, Status string
	Expires                        *time.Time
}

func (s *Server) computePreviewsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.scopeProject(r, o)
	var rows []previewRow
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		q, err := tx.Query(r.Context(), `
			SELECT p.slug, ps.slug, s.preview_name,
			       coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'not deployed'),
			       s.expires_at
			FROM services s JOIN services ps ON ps.id = s.preview_of JOIN projects p ON p.id = s.project_id
			WHERE $1 = '' OR p.slug = $1 ORDER BY p.slug, ps.slug, s.preview_name`, scope)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(q, pgx.RowToStructByPos[previewRow])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "previews", http.StatusOK, data{"Org": o, "Previews": rows, "Scope": scope})
}

type crumb struct{ Label, Path string }

// crumbsFor builds the breadcrumb: product, project, then the resource.
func crumbsFor(d data) []crumb {
	var out []crumb
	if p, ok := d["AreaProduct"].(*product); ok && p.ID != "projects" {
		out = append(out, crumb{p.Name, p.Path})
	}
	if p, ok := d["Project"].(*projectInfo); ok && p != nil {
		if len(out) == 0 {
			out = append(out, crumb{"Projects", "/projects"})
		}
		out = append(out, crumb{p.Name, "/p/" + p.Slug})
	}
	switch {
	case d["Service"] != nil:
		if sv, ok := d["Service"].(*serviceInfo); ok && sv != nil {
			out = append(out, crumb{sv.Slug, ""})
		}
	case d["Database"] != nil:
		if db, ok := d["Database"].(*databaseInfo); ok && db != nil {
			out = append(out, crumb{db.Slug, ""})
		}
	case d["Function"] != nil:
		if f, ok := d["Function"].(*functionInfo); ok && f != nil {
			out = append(out, crumb{f.Slug, ""})
		}
	case d["Bucket"] != nil:
		if b, ok := d["Bucket"].(*bucketInfo); ok && b != nil {
			out = append(out, crumb{b.Name, ""})
		}
	}
	if len(out) > 0 && out[len(out)-1].Path != "" && len(out) == 1 {
		out[0].Path = "" // product root: the current page
	}
	return out
}
