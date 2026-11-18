package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"platform/internal/db"
	platmail "platform/internal/mail"
	"platform/internal/model"
	"platform/internal/secure"
)

func itoa(n int) string                 { return strconv.Itoa(n) }
func roleAtLeast(role, min string) bool { return model.RoleAtLeast(role, min) }

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

type orgInfo struct {
	ID, Slug, Name, Role, PlanID, ScanPolicy string
	// Scoped members (fine-grained access) only reach these projects (slug -> role).
	Scoped        bool
	Projects      map[string]string
	Suspended     bool
	SuspendReason string
	TrialEnds     *time.Time
}

// TrialDaysLeft is shown on the organization page during a trial.
func (o *orgInfo) TrialDaysLeft() int {
	if o.TrialEnds == nil {
		return 0
	}
	d := int(time.Until(*o.TrialEnds).Hours()/24) + 1
	if d < 0 {
		return 0
	}
	return d
}

func (o *orgInfo) suspendedMessage() string {
	switch o.SuspendReason {
	case "trial_ended":
		return "Your trial has ended: your applications are stopped and changes are blocked. Contact the platform operator to choose a plan."
	case "cpu_anomaly":
		return "This organization was suspended automatically because its applications used all their CPU for a long time (this looks like crypto-mining). Contact the platform operator."
	}
	return "This organization is suspended. Contact the platform operator."
}

func (o *orgInfo) Path() string { return "/o/" + o.Slug }

// scopedOrgRoutes are the organization pages a member limited to some projects may open.
var scopedOrgRoutes = map[string]bool{
	"GET /o/{org}": true, "GET /o/{org}/projects": true, "GET /o/{org}/activity.json": true, "POST /o/{org}/pins": true,
}

// sees reports whether the user may see a project (always, unless limited to others).
func (o *orgInfo) sees(projectSlug string) bool {
	if !o.Scoped {
		return true
	}
	_, ok := o.Projects[projectSlug]
	return ok
}

// org resolves the {org} path segment for the current user and enforces a minimum role.
// Organizations the user does not belong to answer 404, so their existence is not revealed.
func (s *Server) org(w http.ResponseWriter, r *http.Request, minRole string) (*orgInfo, bool) {
	var o orgInfo
	err := s.pool.QueryRow(r.Context(), `
		SELECT o.id, o.slug, o.name, m.role, o.plan_id, o.scan_policy, o.suspended, o.suspend_reason, o.trial_ends_at
		FROM orgs o JOIN memberships m ON m.org_id = o.id
		WHERE o.slug = $1 AND m.user_id = $2`, r.PathValue("org"), sessionFrom(r).UserID).
		Scan(&o.ID, &o.Slug, &o.Name, &o.Role, &o.PlanID, &o.ScanPolicy, &o.Suspended, &o.SuspendReason, &o.TrialEnds)
	if err != nil {
		if !isNoRows(err) {
			s.serverError(w, r, err)
		} else {
			s.notFound(w, r)
		}
		return nil, false
	}
	// Fine-grained access: a developer or viewer limited to some projects gets the role
	// of the project in the path, and only the few organization pages listed here.
	if o.Role == "developer" || o.Role == "viewer" {
		rows, err := s.pool.Query(r.Context(), `SELECT project_slug, role FROM member_project_scope($1, $2)`, o.ID, sessionFrom(r).UserID)
		if err != nil {
			s.serverError(w, r, err)
			return nil, false
		}
		scope := map[string]string{}
		for rows.Next() {
			var slug, role string
			if rows.Scan(&slug, &role) == nil {
				scope[slug] = role
			}
		}
		rows.Close()
		if len(scope) > 0 {
			o.Scoped, o.Projects = true, scope
			if proj := r.PathValue("project"); proj != "" {
				role, ok := scope[proj]
				if !ok {
					s.notFound(w, r)
					return nil, false
				}
				o.Role = role
			} else if !scopedOrgRoutes[r.Pattern] {
				s.notFound(w, r)
				return nil, false
			}
		}
	}
	if !model.RoleAtLeast(o.Role, minRole) {
		s.forbidden(w, r)
		return nil, false
	}
	if o.Suspended && r.Method == http.MethodPost {
		back(w, r, o.Path(), "", o.suspendedMessage())
		return nil, false
	}
	return &o, true
}

func (s *Server) audit(r *http.Request, q db.Execer, orgID, action, target string, detail any) {
	// Actions made with an API token are recorded under the token's creator, with the token named.
	if tok, ok := r.Context().Value(apiTokenKey).(string); ok && tok != "" {
		detail = map[string]any{"via_api_token": tok, "detail": detail}
	}
	if err := db.Audit(r.Context(), q, orgID, sessionFrom(r).UserID, action, target, detail, clientIP(r)); err != nil {
		s.log.Error("audit", "action", action, "err", err)
	}
}

// ---------- home and organization ----------

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	type row struct{ Slug, Name, Role string }
	rows, err := s.pool.Query(r.Context(), `
		SELECT o.slug, o.name, m.role FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.user_id = $1 ORDER BY o.name`, sessionFrom(r).UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(orgs) == 1 && r.URL.Query().Get("msg") == "" {
		http.Redirect(w, r, "/o/"+orgs[0].Slug, http.StatusSeeOther)
		return
	}
	s.render(w, r, "home", http.StatusOK, data{"Orgs": orgs})
}

type plan struct {
	Name                                                     string
	MaxServices, MaxCPU, MaxMemory, MaxDomains, MaxDatabases int
	MaxDiskMB, DiskUsedMB, MaxInstances                      int
}

func (s *Server) loadPlan(r *http.Request, q pgx.Tx, id string) (plan, error) {
	var p plan
	err := q.QueryRow(r.Context(), `
		SELECT p.name, p.max_services, p.max_cpu_millis, p.max_memory_mb, p.max_domains, p.max_databases, p.max_disk_mb,
		       coalesce((SELECT disk_used_mb FROM orgs WHERE id = nullif(current_setting('app.org_id', true), '')::uuid), 0),
		       p.max_instances
		FROM plans p WHERE p.id = $1`, id).
		Scan(&p.Name, &p.MaxServices, &p.MaxCPU, &p.MaxMemory, &p.MaxDomains, &p.MaxDatabases, &p.MaxDiskMB, &p.DiskUsedMB, &p.MaxInstances)
	return p, err
}

func (s *Server) orgPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	s.renderOrg(w, r, o, http.StatusOK, data{})
}

func (s *Server) renderOrg(w http.ResponseWriter, r *http.Request, o *orgInfo, status int, d data) {
	d["SuspendedMessage"] = o.suspendedMessage()
	d["EmailEnabled"] = platmail.Configured(s.cfg)
	type project struct {
		Slug, Name          string
		Services, Databases int
	}
	type usage struct{ Services, CPU, Memory, Databases int }
	type regToken struct {
		ID, Name string
		CanPush  bool
		Created  time.Time
		LastUsed *time.Time
	}
	var projects []project
	var u usage
	var p plan
	var tokens []regToken
	var scans []scanSummary
	var channels []channelView
	var incidents []incidentView
	var alarms []alarmView
	type apiToken struct {
		ID, Name, Role string
		Created        time.Time
		LastUsed       *time.Time
		Expires        *time.Time
	}
	var apiTokens []apiToken
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT p.slug, p.name, (SELECT count(*) FROM services s WHERE s.project_id = p.id AND s.preview_of IS NULL),
			       (SELECT count(*) FROM databases d WHERE d.project_id = p.id)
			FROM projects p ORDER BY p.name`)
		if err != nil {
			return err
		}
		if projects, err = pgx.CollectRows(rows, pgx.RowToStructByPos[project]); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), `
			SELECT (SELECT count(*) FROM services WHERE preview_of IS NULL),
			       (SELECT coalesce(sum(cpu_millis), 0) FROM services) + (SELECT coalesce(sum(cpu_millis), 0) FROM databases),
			       (SELECT coalesce(sum(memory_mb), 0) FROM services) + (SELECT coalesce(sum(memory_mb), 0) FROM databases),
			       (SELECT count(*) FROM databases)`).
			Scan(&u.Services, &u.CPU, &u.Memory, &u.Databases); err != nil {
			return err
		}
		if p, err = s.loadPlan(r, tx, o.PlanID); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `SELECT id, name, can_push, created_at, last_used_at FROM registry_tokens ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		if tokens, err = pgx.CollectRows(rows, pgx.RowToStructByPos[regToken]); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `SELECT `+scanColumns+` FROM image_scans ORDER BY created_at DESC LIMIT 10`)
		if err != nil {
			return err
		}
		if scans, err = pgx.CollectRows(rows, pgx.RowToStructByPos[scanSummary]); err != nil {
			return err
		}
		if channels, err = loadChannels(r, tx); err != nil {
			return err
		}
		if incidents, err = loadIncidents(r, tx, "", 12); err != nil {
			return err
		}
		if alarms, err = loadAlarms(r.Context(), tx); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `SELECT id, name, role, created_at, last_used_at, expires_at FROM api_tokens ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		apiTokens, err = pgx.CollectRows(rows, pgx.RowToStructByPos[apiToken])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	type member struct {
		ID, Name, Email, Role string
		Scope                 map[string]string // project slug -> role (empty: the whole organization)
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT u.id, u.name, u.email, m.role FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 ORDER BY u.name`, o.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (member, error) {
		var m member
		err := row.Scan(&m.ID, &m.Name, &m.Email, &m.Role)
		m.Scope = map[string]string{}
		return m, err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	_ = db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT mr.user_id, p.slug, mr.role FROM member_project_roles mr JOIN projects p ON p.id = mr.project_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var uid, slug, role string
			if rows.Scan(&uid, &slug, &role) == nil {
				for i := range members {
					if members[i].ID == uid {
						members[i].Scope[slug] = role
					}
				}
			}
		}
		return nil
	})
	if o.Scoped { // a member limited to some projects sees only those
		kept := projects[:0]
		for _, p := range projects {
			if o.sees(p.Slug) {
				kept = append(kept, p)
			}
		}
		projects = kept
	}
	type pending struct {
		Email, Role string
		Expires     time.Time
	}
	var invites []pending
	if model.RoleAtLeast(o.Role, "admin") {
		rows, err := s.pool.Query(r.Context(), `
			SELECT email, role, expires_at FROM invites
			WHERE org_id = $1 AND accepted_at IS NULL AND expires_at > now() ORDER BY created_at DESC`, o.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if invites, err = pgx.CollectRows(rows, pgx.RowToStructByPos[pending]); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	d["Org"], d["Projects"], d["Members"], d["Invites"], d["Usage"], d["Plan"] = o, projects, members, invites, u, p
	d["RegistryHost"], d["RegistryTokens"], d["Scans"] = s.cfg.RegistryHost, tokens, scans
	if o.Scoped { // a member limited to some projects sees none of the organization-wide lists
		for _, k := range []string{"Members", "Invites", "Channels", "Incidents", "Alarms", "APITokens", "RegistryTokens", "Scans"} {
			delete(d, k)
		}
	}
	d["Channels"], d["Incidents"], d["AlertKinds"], d["Alarms"] = channels, incidents, alertKinds, alarms
	d["APITokens"], d["BaseURL"] = apiTokens, s.cfg.BaseURL
	if model.RoleAtLeast(o.Role, "admin") {
		sso, err := s.loadSSO(r.Context(), o.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d["SSO"], d["SSOCallback"] = sso, ssoCallback(s)
	}
	page, _ := d["Page"].(string)
	if page == "" {
		page = "console"
	}
	s.render(w, r, page, status, d)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = slug
	}
	if !model.ValidSlug(slug) || len(name) > 100 {
		back(w, r, o.Path()+"/projects#create", "", "Project ID: 2 to 20 characters, lowercase letters, digits and dashes, starting with a letter or digit.")
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $3)`, o.ID, slug, name); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "project.created", "project:"+slug, nil)
		return nil
	})
	if isUniqueViolation(err) {
		back(w, r, o.Path()+"/projects#create", "", "A project with this ID already exists.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/p/"+slug, "Project created.", "")
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	role := r.PostFormValue("role")
	if !strings.Contains(email, "@") || len(email) > 254 || !model.ValidRole(role) {
		back(w, r, o.Path()+"/iam#members", "", "Enter a valid email address and role.")
		return
	}
	if role == "owner" && o.Role != "owner" {
		back(w, r, o.Path()+"/iam#members", "", "Only owners can invite other owners.")
		return
	}
	token := secure.NewToken()
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO invites (org_id, email, role, token_hash, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + interval '7 days')`,
		o.ID, email, role, secure.HashToken(token), sessionFrom(r).UserID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.audit(r, s.pool, o.ID, "invite.created", "email:"+email, map[string]string{"role": role})
	// Shown once, never stored in clear: the admin sends it to the person over a channel they trust.
	s.renderOrg(w, r, o, http.StatusOK, data{"InviteLink": s.cfg.BaseURL + "/invite/" + token, "InviteEmail": email, "Tab": "members", "Page": "iam"})
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	userID := r.PathValue("user")
	var role string
	if err := s.pool.QueryRow(r.Context(), `SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2`, o.ID, userID).Scan(&role); err != nil {
		back(w, r, o.Path()+"/iam#members", "", "Member not found.")
		return
	}
	if role == "owner" {
		var owners int
		_ = s.pool.QueryRow(r.Context(), `SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner'`, o.ID).Scan(&owners)
		if o.Role != "owner" || owners <= 1 {
			back(w, r, o.Path()+"/iam#members", "", "Only an owner can remove an owner, and the last owner cannot be removed.")
			return
		}
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, o.ID, userID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.audit(r, s.pool, o.ID, "member.removed", "user:"+userID, nil)
	back(w, r, o.Path()+"/iam#members", "Member removed.", "")
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	type event struct {
		At                          time.Time
		Who, Action, Target, Detail string
		IP                          string
	}
	var events []event
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT a.created_at, coalesce(u.email, 'platform'), a.action, a.target, a.detail::text, coalesce(a.ip, '')
			FROM audit_events a LEFT JOIN users u ON u.id = a.user_id
			ORDER BY a.id DESC LIMIT 300`)
		if err != nil {
			return err
		}
		events, err = pgx.CollectRows(rows, pgx.RowToStructByPos[event])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "audit", http.StatusOK, data{"Org": o, "Events": events, "AuditPage": true})
}

// ---------- projects ----------

type projectInfo struct {
	ID, Slug, Name string
	StatusPublic   bool
}

func (s *Server) loadProject(r *http.Request, tx pgx.Tx) (*projectInfo, error) {
	var p projectInfo
	err := tx.QueryRow(r.Context(), `SELECT id, slug, name, status_public FROM projects WHERE slug = $1`, r.PathValue("project")).
		Scan(&p.ID, &p.Slug, &p.Name, &p.StatusPublic)
	return &p, err
}

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	type svc struct {
		Slug, Image, Status string
	}
	type dbRow struct {
		Slug, Engine, Version, Status string
		CPUMillis, MemoryMB           int
	}
	type resource struct{ Kind, Name, Status, Detail, Path string }
	var p *projectInfo
	var services []svc
	var dbs []dbRow
	var vars []projectVar
	var others []resource // domains and scheduled jobs of the project
	var peers []peerView
	var otherProjects []string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.loadProject(r, tx); err != nil {
			return err
		}
		orows, err := tx.Query(r.Context(), `
			SELECT 'Domain', dm.hostname, CASE WHEN dm.verified_at IS NULL THEN 'pending' ELSE 'verified' END, 'serves ' || s.slug, '/s/' || s.slug || '#domains'
			FROM domains dm JOIN services s ON s.id = dm.service_id WHERE s.project_id = $1
			UNION ALL
			SELECT 'Scheduled job', j.slug, CASE WHEN j.enabled THEN coalesce((SELECT r.status FROM job_runs r WHERE r.job_id = j.id ORDER BY r.started_at DESC LIMIT 1), 'none') ELSE 'paused' END,
			       s.slug || ' · ' || j.schedule, '/s/' || s.slug || '#jobs'
			FROM jobs j JOIN services s ON s.id = j.service_id WHERE s.project_id = $1
			ORDER BY 1, 2`, p.ID)
		if err != nil {
			return err
		}
		if others, err = pgx.CollectRows(orows, pgx.RowToStructByPos[resource]); err != nil {
			return err
		}
		drows, err := tx.Query(r.Context(), `SELECT slug, engine, version, status, cpu_millis, memory_mb FROM databases WHERE project_id = $1 ORDER BY slug`, p.ID)
		if err != nil {
			return err
		}
		if dbs, err = pgx.CollectRows(drows, pgx.RowToStructByPos[dbRow]); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `
			SELECT s.slug, s.image,
			       coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'not deployed')
			FROM services s WHERE s.project_id = $1 AND s.preview_of IS NULL ORDER BY s.slug`, p.ID)
		if err != nil {
			return err
		}
		if services, err = pgx.CollectRows(rows, pgx.RowToStructByPos[svc]); err != nil {
			return err
		}
		if vars, err = s.loadProjectVars(r, tx, p.ID); err != nil {
			return err
		}
		if peers, err = loadPeers(r.Context(), tx, p.ID); err != nil {
			return err
		}
		prow, err := tx.Query(r.Context(), `SELECT slug FROM projects WHERE id <> $1 ORDER BY slug`, p.ID)
		if err != nil {
			return err
		}
		otherProjects, err = pgx.CollectRows(prow, pgx.RowTo[string])
		return err
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	live := 0
	for _, sv := range services {
		if sv.Status == "live" {
			live++
		}
	}
	s.render(w, r, "project", http.StatusOK, data{"Org": o, "Project": p, "Services": services, "Databases": dbs, "Variables": vars, "BaseURL": s.cfg.BaseURL,
		"Engines": model.Engines, "Others": others, "LiveServices": live, "Peers": peers, "OtherProjects": otherProjects})
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	slug := r.PathValue("project")
	if r.PostFormValue("confirm") != slug {
		back(w, r, o.Path()+"/p/"+slug+"#project-settings", "", "Type the project ID exactly to confirm deletion.")
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM projects WHERE slug = $1`, slug)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		s.audit(r, tx, o.ID, "project.deleted", "project:"+slug, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if err != nil && !isNoRows(err) {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/projects", "Project deleted. Its applications are being stopped.", "")
}

// checkQuota verifies the plan allows the change. excludeID skips the service being edited.
func (s *Server) checkQuota(r *http.Request, tx pgx.Tx, o *orgInfo, excludeID string, cpu, mem int, newService bool) error {
	if err := s.checkDisk(r, tx, o); err != nil {
		return err
	}
	p, err := s.loadPlan(r, tx, o.PlanID)
	if err != nil {
		return err
	}
	var n, usedCPU, usedMem int
	// Services and managed databases share the plan's CPU and memory; excludeID skips
	// the service or database being edited.
	if err := tx.QueryRow(r.Context(), `
		SELECT (SELECT count(*) FROM services WHERE id::text <> $1 AND preview_of IS NULL),
		       (SELECT coalesce(sum(cpu_millis * instances), 0) FROM services WHERE id::text <> $1) +
		       (SELECT coalesce(sum(cpu_millis), 0) FROM databases WHERE id::text <> $1),
		       (SELECT coalesce(sum(memory_mb * instances), 0) FROM services WHERE id::text <> $1) +
		       (SELECT coalesce(sum(memory_mb), 0) FROM databases WHERE id::text <> $1)`, excludeID).Scan(&n, &usedCPU, &usedMem); err != nil {
		return err
	}
	switch {
	case newService && n+1 > p.MaxServices:
		return quotaError(fmt.Sprintf("Your %s plan allows %d services.", p.Name, p.MaxServices))
	case usedCPU+cpu > p.MaxCPU:
		return quotaError(fmt.Sprintf("CPU limit reached: your plan allows %d millicores in total and %d are already allocated to other services.", p.MaxCPU, usedCPU))
	case usedMem+mem > p.MaxMemory:
		return quotaError(fmt.Sprintf("Memory limit reached: your plan allows %d MB in total and %d MB are already allocated to other services.", p.MaxMemory, usedMem))
	}
	return nil
}

// checkInstances refuses more instances per service than the plan allows.
func (s *Server) checkInstances(r *http.Request, tx pgx.Tx, o *orgInfo, n int) error {
	if n <= 1 {
		return nil
	}
	p, err := s.loadPlan(r, tx, o.PlanID)
	if err != nil {
		return err
	}
	if n > p.MaxInstances {
		return quotaError(fmt.Sprintf("Your %s plan allows %d instance(s) per service.", p.Name, p.MaxInstances))
	}
	return nil
}

// checkDisk refuses growth once persistent storage is above 110% of the plan's limit
// (measured by the agent every 15 minutes). Running apps are not affected.
func (s *Server) checkDisk(r *http.Request, tx pgx.Tx, o *orgInfo) error {
	var used, limit int
	if err := tx.QueryRow(r.Context(), `SELECT o.disk_used_mb, p.max_disk_mb FROM orgs o JOIN plans p ON p.id = o.plan_id WHERE o.id = $1`, o.ID).
		Scan(&used, &limit); err != nil {
		return err
	}
	if used*100 > limit*110 {
		return quotaError(fmt.Sprintf("Disk limit exceeded: your applications and databases store %d MB and your plan allows %d MB. Delete data, or ask for a larger plan, before deploying again.", used, limit))
	}
	return nil
}

type quotaError string

func (e quotaError) Error() string { return string(e) }
