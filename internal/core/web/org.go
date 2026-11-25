package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"platform/internal/lib/db"
	platmail "platform/internal/lib/mail"
	"platform/internal/lib/model"
)

func Itoa(n int) string                 { return strconv.Itoa(n) }
func roleAtLeast(role, min string) bool { return model.RoleAtLeast(role, min) }

func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

type OrgInfo struct {
	ID, Slug, Name, Role, PlanID, ScanPolicy string
	// Scoped members (fine-grained access) only reach these projects (slug -> role).
	Scoped        bool
	Projects      map[string]string
	Suspended     bool
	SuspendReason string
	TrialEnds     *time.Time
	Off           map[string]bool // services turned off by the operator (feature switches)
}

// TrialDaysLeft is shown on the organization page during a trial.
func (o *OrgInfo) TrialDaysLeft() int {
	if o.TrialEnds == nil {
		return 0
	}
	d := int(time.Until(*o.TrialEnds).Hours()/24) + 1
	if d < 0 {
		return 0
	}
	return d
}

func (o *OrgInfo) suspendedMessage() string {
	switch o.SuspendReason {
	case "trial_ended":
		return "Your trial has ended: your applications are stopped and changes are blocked. Contact the platform operator to choose a plan."
	case "cpu_anomaly":
		return "This organization was suspended automatically because its applications used all their CPU for a long time (this looks like crypto-mining). Contact the platform operator."
	}
	return "This organization is suspended. Contact the platform operator."
}

func (o *OrgInfo) Path() string { return "/o/" + o.Slug }

// scopedOrgRoutes are the organization pages a member limited to some projects may open.
var scopedOrgRoutes = map[string]bool{
	"GET /o/{org}": true, "GET /o/{org}/projects": true, "GET /o/{org}/activity.json": true, "POST /o/{org}/pins": true,
}

// sees reports whether the user may see a project (always, unless limited to others).
func (o *OrgInfo) Sees(projectSlug string) bool {
	if !o.Scoped {
		return true
	}
	_, ok := o.Projects[projectSlug]
	return ok
}

// org resolves the {org} path segment for the current user and enforces a minimum role.
// Organizations the user does not belong to answer 404, so their existence is not revealed.
func (s *Server) Org(w http.ResponseWriter, r *http.Request, minRole string) (*OrgInfo, bool) {
	if ao, status := apiOrgFrom(r, minRole); status != 0 { // a console action called through the API
		if ao == nil {
			w.WriteHeader(status)
			return nil, false
		}
		return ao, true
	}
	var o OrgInfo
	err := s.Pool.QueryRow(r.Context(), `
		SELECT o.id, o.slug, o.name, m.role, o.plan_id, o.scan_policy, o.suspended, o.suspend_reason, o.trial_ends_at
		FROM orgs o JOIN memberships m ON m.org_id = o.id
		WHERE o.slug = $1 AND m.user_id = $2`, r.PathValue("org"), SessionFrom(r).UserID).
		Scan(&o.ID, &o.Slug, &o.Name, &o.Role, &o.PlanID, &o.ScanPolicy, &o.Suspended, &o.SuspendReason, &o.TrialEnds)
	if err != nil {
		if !IsNoRows(err) {
			s.ServerError(w, r, err)
		} else {
			s.NotFound(w, r)
		}
		return nil, false
	}
	// Fine-grained access: a developer or viewer limited to some projects gets the role
	// of the project in the path, and only the few organization pages listed here.
	if o.Role == "developer" || o.Role == "viewer" {
		rows, err := s.Pool.Query(r.Context(), `SELECT project_slug, role FROM member_project_scope($1, $2)`, o.ID, SessionFrom(r).UserID)
		if err != nil {
			s.ServerError(w, r, err)
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
					s.NotFound(w, r)
					return nil, false
				}
				o.Role = role
			} else if !scopedOrgRoutes[r.Pattern] {
				s.NotFound(w, r)
				return nil, false
			}
		}
	}
	if !model.RoleAtLeast(o.Role, minRole) {
		s.forbidden(w, r)
		return nil, false
	}
	s.loadFeaturesOff(r.Context(), &o)
	if area := areaOf(o.Path(), r.URL.Path); o.Off[area] {
		if p := ProductByID(area); p != nil {
			s.featureOffPage(w, r, p)
			return nil, false
		}
	}
	if o.Suspended && r.Method == http.MethodPost {
		Back(w, r, o.Path(), "", o.suspendedMessage())
		return nil, false
	}
	return &o, true
}

func (s *Server) Audit(r *http.Request, q db.Execer, orgID, action, target string, detail any) {
	// Actions made with an API token are recorded under the token's creator, with the token named.
	if tok, ok := r.Context().Value(APITokenKey).(string); ok && tok != "" {
		detail = map[string]any{"via_api_token": tok, "detail": detail}
	}
	if err := db.Audit(r.Context(), q, orgID, SessionFrom(r).UserID, action, target, detail, ClientIP(r)); err != nil {
		s.Log.Error("audit", "action", action, "err", err)
	}
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

func (s *Server) RenderOrg(w http.ResponseWriter, r *http.Request, o *OrgInfo, status int, d Data) {
	d["SuspendedMessage"] = o.suspendedMessage()
	d["EmailEnabled"] = platmail.Configured(s.Cfg)
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
	var scans []ScanSummary
	var channels []channelView
	var incidents []IncidentView
	var alarms []AlarmView
	type apiToken struct {
		ID, Name, Role string
		Created        time.Time
		LastUsed       *time.Time
		Expires        *time.Time
	}
	var apiTokens []apiToken
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
		if scans, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ScanSummary]); err != nil {
			return err
		}
		if channels, err = loadChannels(r, tx); err != nil {
			return err
		}
		if incidents, err = LoadIncidents(r, tx, "", 12); err != nil {
			return err
		}
		if alarms, err = LoadAlarms(r.Context(), tx); err != nil {
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
		s.ServerError(w, r, err)
		return
	}
	type member struct {
		ID, Name, Email, Role string
		Scope                 map[string]string // project slug -> role (empty: the whole organization)
	}
	rows, err := s.Pool.Query(r.Context(), `
		SELECT u.id, u.name, u.email, m.role FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 ORDER BY u.name`, o.ID)
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (member, error) {
		var m member
		err := row.Scan(&m.ID, &m.Name, &m.Email, &m.Role)
		m.Scope = map[string]string{}
		return m, err
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	_ = db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
			if o.Sees(p.Slug) {
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
		rows, err := s.Pool.Query(r.Context(), `
			SELECT email, role, expires_at FROM invites
			WHERE org_id = $1 AND accepted_at IS NULL AND expires_at > now() ORDER BY created_at DESC`, o.ID)
		if err != nil {
			s.ServerError(w, r, err)
			return
		}
		if invites, err = pgx.CollectRows(rows, pgx.RowToStructByPos[pending]); err != nil {
			s.ServerError(w, r, err)
			return
		}
	}
	d["Org"], d["Projects"], d["Members"], d["Invites"], d["Usage"], d["Plan"] = o, projects, members, invites, u, p
	d["RegistryHost"], d["RegistryTokens"], d["Scans"] = s.Cfg.RegistryHost, tokens, scans
	if o.Scoped { // a member limited to some projects sees none of the organization-wide lists
		for _, k := range []string{"Members", "Invites", "Channels", "Incidents", "Alarms", "APITokens", "RegistryTokens", "Scans"} {
			delete(d, k)
		}
	}
	d["Channels"], d["Incidents"], d["AlertKinds"], d["Alarms"] = channels, incidents, AlertKinds, alarms
	d["APITokens"], d["BaseURL"] = apiTokens, s.Cfg.BaseURL
	if model.RoleAtLeast(o.Role, "admin") {
		sso, err := s.LoadSSO(r.Context(), o.ID)
		if err != nil {
			s.ServerError(w, r, err)
			return
		}
		d["SSO"], d["SSOCallback"] = sso, SSOCallback(s)
	}
	page, _ := d["Page"].(string)
	if page == "" {
		page = "console"
	}
	s.Render(w, r, page, status, d)
}

// ---------- projects ----------

type ProjectInfo struct {
	ID, Slug, Name string
	StatusPublic   bool
}

func (s *Server) LoadProject(r *http.Request, tx pgx.Tx) (*ProjectInfo, error) {
	var p ProjectInfo
	err := tx.QueryRow(r.Context(), `SELECT id, slug, name, status_public FROM projects WHERE slug = $1`, r.PathValue("project")).
		Scan(&p.ID, &p.Slug, &p.Name, &p.StatusPublic)
	return &p, err
}

// checkQuota verifies the plan allows the change. excludeID skips the service being edited.
func (s *Server) CheckQuota(r *http.Request, tx pgx.Tx, o *OrgInfo, excludeID string, cpu, mem int, newService bool) error {
	if err := s.CheckDisk(r, tx, o); err != nil {
		return err
	}
	p, err := s.loadPlan(r, tx, o.PlanID)
	if err != nil {
		return err
	}
	var n, usedCPU, usedMem int
	// Services and managed databases share the plan's CPU and memory; excludeID skips
	// the service or database being edited.
	// Applications on the organization's own servers use its hardware, not the plan's.
	if err := tx.QueryRow(r.Context(), `
		WITH own AS (SELECT p.id FROM projects p JOIN nodes n ON n.id = p.node_id WHERE n.role = 'customer'),
		     svc AS (SELECT * FROM services WHERE project_id NOT IN (SELECT id FROM own))
		SELECT (SELECT count(*) FROM svc WHERE id::text <> $1 AND preview_of IS NULL),
		       (SELECT coalesce(sum(cpu_millis * instances), 0) FROM svc WHERE id::text <> $1) +
		       (SELECT coalesce(sum(cpu_millis), 0) FROM databases WHERE id::text <> $1),
		       (SELECT coalesce(sum(memory_mb * instances), 0) FROM svc WHERE id::text <> $1) +
		       (SELECT coalesce(sum(memory_mb), 0) FROM databases WHERE id::text <> $1)`, excludeID).Scan(&n, &usedCPU, &usedMem); err != nil {
		return err
	}
	switch {
	case newService && n+1 > p.MaxServices:
		return QuotaError(fmt.Sprintf("Your %s plan allows %d services.", p.Name, p.MaxServices))
	case usedCPU+cpu > p.MaxCPU:
		return QuotaError(fmt.Sprintf("CPU limit reached: your plan allows %d millicores in total and %d are already allocated to other services.", p.MaxCPU, usedCPU))
	case usedMem+mem > p.MaxMemory:
		return QuotaError(fmt.Sprintf("Memory limit reached: your plan allows %d MB in total and %d MB are already allocated to other services.", p.MaxMemory, usedMem))
	}
	return nil
}

// checkInstances refuses more instances per service than the plan allows.
func (s *Server) CheckInstances(r *http.Request, tx pgx.Tx, o *OrgInfo, n int) error {
	if n <= 1 {
		return nil
	}
	p, err := s.loadPlan(r, tx, o.PlanID)
	if err != nil {
		return err
	}
	if n > p.MaxInstances {
		return QuotaError(fmt.Sprintf("Your %s plan allows %d instance(s) per service.", p.Name, p.MaxInstances))
	}
	return nil
}

// checkDisk refuses growth once persistent storage is above 110% of the plan's limit
// (measured by the agent every 15 minutes). Running apps are not affected.
func (s *Server) CheckDisk(r *http.Request, tx pgx.Tx, o *OrgInfo) error {
	var used, limit int
	if err := tx.QueryRow(r.Context(), `SELECT o.disk_used_mb, p.max_disk_mb FROM orgs o JOIN plans p ON p.id = o.plan_id WHERE o.id = $1`, o.ID).
		Scan(&used, &limit); err != nil {
		return err
	}
	if used*100 > limit*110 {
		return QuotaError(fmt.Sprintf("Disk limit exceeded: your applications and databases store %d MB and your plan allows %d MB. Delete data, or ask for a larger plan, before deploying again.", used, limit))
	}
	return nil
}

type QuotaError string

func (e QuotaError) Error() string { return string(e) }
