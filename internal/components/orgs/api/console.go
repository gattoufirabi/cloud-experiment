package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
)

// setScope remembers the project filter and returns to the page it was chosen on.
func (s *API) setScope(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	p := r.URL.Query().Get("project")
	c := &http.Cookie{Name: web.ScopeCookie(o), Value: p, Path: o.Path(), HttpOnly: true, Secure: s.Cfg.CookieSecure,
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
func (s *API) togglePin(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	id := r.PostFormValue("product")
	p := web.ProductByID(id)
	if p == nil {
		s.NotFound(w, r)
		return
	}
	uid := web.SessionFrom(r).UserID
	pins := s.LoadPins(r, uid)
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
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `DELETE FROM user_pins WHERE user_id = $1`, uid); err != nil {
		s.ServerError(w, r, err)
		return
	}
	for i, x := range next {
		if _, err := tx.Exec(r.Context(), `INSERT INTO user_pins (user_id, service, position) VALUES ($1, $2, $3)`, uid, x, i); err != nil {
			s.ServerError(w, r, err)
			return
		}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE users SET pins_customized = true WHERE id = $1`, uid); err != nil {
		s.ServerError(w, r, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.ServerError(w, r, err)
		return
	}
	dest := r.PostFormValue("back")
	if !strings.HasPrefix(dest, o.Path()) || strings.Contains(dest, "//") {
		dest = o.Path() + "/services"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// servicesPage is the full product catalog, where products are pinned.
func (s *API) servicesPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	s.Render(w, r, "services", http.StatusOK, web.Data{"Org": o, "Area": ""})
}

// ---------- area pages built from the organization data ----------

func (s *API) orgArea(page, minRole string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, ok := s.Org(w, r, minRole)
		if !ok {
			return
		}
		s.RenderOrg(w, r, o, http.StatusOK, web.Data{"Page": page})
	}
}

// ---------- resource lists across projects ----------

type serviceRow struct {
	Project, ProjectName, Slug, Image, Status string
	Instances                                 int
	Worker                                    bool
	Updated                                   *time.Time
}

func (s *API) computePage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.ScopeProject(r, o)
	var rows []serviceRow
	var projects []web.ProjectOption
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if projects, err = web.LoadProjectOptions(r, tx); err != nil {
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
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "compute", http.StatusOK, web.Data{"Org": o, "Services": rows, "Projects": projects, "Scope": scope,
		"CanEdit": model.RoleAtLeast(o.Role, "developer"), "AppsDomain": s.Cfg.AppsDomain})
}

type databaseRow struct {
	Project, ProjectName, Slug, Engine, Version, Status string
	CPUMillis, MemoryMB                                 int
	Created                                             time.Time
}

func (s *API) databasesPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.ScopeProject(r, o)
	var rows []databaseRow
	var projects []web.ProjectOption
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if projects, err = web.LoadProjectOptions(r, tx); err != nil {
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
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "databases", http.StatusOK, web.Data{"Org": o, "Databases": rows, "Projects": projects, "Scope": scope,
		"Engines": model.Engines, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

type domainRow struct {
	Hostname, Project, Service string
	Verified                   *time.Time
	Created                    time.Time
}

func (s *API) domainsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.ScopeProject(r, o)
	var rows []domainRow
	var webServices []string
	var maxDomains int
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "domains", http.StatusOK, web.Data{"Org": o, "Domains": rows, "Scope": scope, "MaxDomains": maxDomains, "AppsDomain": s.Cfg.AppsDomain,
		"WebServices": webServices, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

// createDatabaseIn creates a database in the project chosen in the form (the area
// pages are not inside a project).
func (s *API) createDatabaseIn(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("project", r.PostFormValue("project"))
	s.CreateDatabase(w, r)
}

// ---------- search ----------

type searchHit struct {
	Kind, Name, Detail, Path string
}

func (s *API) searchPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var hits []searchHit
	var prods []*web.Product
	if q != "" {
		lq := strings.ToLower(q)
		for i := range web.Products {
			p := &web.Products[i]
			if s.ProductEnabled(p) && (strings.Contains(strings.ToLower(p.Name+" "+p.Short+" "+p.Description), lq)) {
				prods = append(prods, p)
			}
		}
		like := "%" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(lq) + "%"
		err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
			s.ServerError(w, r, err)
			return
		}
	}
	s.Render(w, r, "search", http.StatusOK, web.Data{"Org": o, "Q": q, "Hits": hits, "Products": prods})
}

type jobRow struct {
	Project, Service, Slug, Schedule, Command string
	Enabled                                   bool
	NextRun                                   *time.Time
	LastStatus                                string
	LastRun                                   *time.Time
}

func (s *API) computeJobsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.ScopeProject(r, o)
	var rows []jobRow
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "jobs", http.StatusOK, web.Data{"Org": o, "Jobs": rows, "Scope": scope})
}

type previewRow struct {
	Project, Service, Name, Status string
	Expires                        *time.Time
}

func (s *API) computePreviewsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.ScopeProject(r, o)
	var rows []previewRow
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "previews", http.StatusOK, web.Data{"Org": o, "Previews": rows, "Scope": scope})
}
