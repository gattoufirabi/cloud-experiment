package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	platmail "platform/internal/lib/mail"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

// ---------- home and organization ----------

func (s *API) home(w http.ResponseWriter, r *http.Request) {
	type row struct{ Slug, Name, Role string }
	rows, err := s.Pool.Query(r.Context(), `
		SELECT o.slug, o.name, m.role FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.user_id = $1 ORDER BY o.name`, web.SessionFrom(r).UserID)
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if len(orgs) == 1 && r.URL.Query().Get("msg") == "" {
		http.Redirect(w, r, "/o/"+orgs[0].Slug, http.StatusSeeOther)
		return
	}
	s.Render(w, r, "home", http.StatusOK, web.Data{"Orgs": orgs})
}

func (s *API) orgPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	s.RenderOrg(w, r, o, http.StatusOK, web.Data{})
}

func (s *API) createProject(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = slug
	}
	if !model.ValidSlug(slug) || len(name) > 100 {
		web.Back(w, r, o.Path()+"/projects#create", "", "Project ID: 2 to 20 characters, lowercase letters, digits and dashes, starting with a letter or digit.")
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $3)`, o.ID, slug, name); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "project.created", "project:"+slug, nil)
		return nil
	})
	if web.IsUniqueViolation(err) {
		web.Back(w, r, o.Path()+"/projects#create", "", "A project with this ID already exists.")
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/p/"+slug, "Project created.", "")
}

func (s *API) createInvite(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	role := r.PostFormValue("role")
	if !strings.Contains(email, "@") || len(email) > 254 || !model.ValidRole(role) {
		web.Back(w, r, o.Path()+"/iam#members", "", "Enter a valid email address and role.")
		return
	}
	if role == "owner" && o.Role != "owner" {
		web.Back(w, r, o.Path()+"/iam#members", "", "Only owners can invite other owners.")
		return
	}
	token := secure.NewToken()
	if _, err := s.Pool.Exec(r.Context(), `
		INSERT INTO invites (org_id, email, role, token_hash, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + interval '7 days')`,
		o.ID, email, role, secure.HashToken(token), web.SessionFrom(r).UserID); err != nil {
		s.ServerError(w, r, err)
		return
	}
	link := s.Cfg.BaseURL + "/invite/" + token
	emailed := false
	if r.PostFormValue("send_email") == "1" && platmail.Configured(s.Cfg) {
		inviter := web.SessionFrom(r).Name
		if inviter == "" {
			inviter = web.SessionFrom(r).Email
		}
		body := inviter + " invited you to join " + o.Name + " on the platform, as " + role + ".\n\n" +
			"Accept the invitation and create your account (or sign in with " + email + " if you already have one):\n\n" +
			link + "\n\nThe link works once and expires in 7 days. If you did not expect this invitation, ignore this email."
		if err := s.SendMail(r.Context(), email, inviter+" invited you to "+o.Name, body); err != nil {
			s.Log.Warn("invitation email", "err", err)
		} else {
			emailed = true
		}
	}
	s.Audit(r, s.Pool, o.ID, "invite.created", "email:"+email, map[string]any{"role": role, "emailed": emailed})
	// Shown once, never stored in clear: the admin can also send it over a channel they trust.
	s.RenderOrg(w, r, o, http.StatusOK, web.Data{"InviteLink": link, "InviteEmail": email, "InviteEmailed": emailed, "Tab": "members", "Page": "iam"})
}

func (s *API) removeMember(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	userID := r.PathValue("user")
	var role string
	if err := s.Pool.QueryRow(r.Context(), `SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2`, o.ID, userID).Scan(&role); err != nil {
		web.Back(w, r, o.Path()+"/iam#members", "", "Member not found.")
		return
	}
	if role == "owner" {
		var owners int
		_ = s.Pool.QueryRow(r.Context(), `SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner'`, o.ID).Scan(&owners)
		if o.Role != "owner" || owners <= 1 {
			web.Back(w, r, o.Path()+"/iam#members", "", "Only an owner can remove an owner, and the last owner cannot be removed.")
			return
		}
	}
	if _, err := s.Pool.Exec(r.Context(), `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, o.ID, userID); err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.Audit(r, s.Pool, o.ID, "member.removed", "user:"+userID, nil)
	web.Back(w, r, o.Path()+"/iam#members", "Member removed.", "")
}

func (s *API) auditPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	type event struct {
		At                          time.Time
		Who, Action, Target, Detail string
		IP                          string
	}
	var events []event
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "audit", http.StatusOK, web.Data{"Org": o, "Events": events, "AuditPage": true})
}

func (s *API) projectPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
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
	var p *web.ProjectInfo
	var services []svc
	var dbs []dbRow
	var vars []web.ProjectVar
	var others []resource // domains and scheduled jobs of the project
	var peers []web.PeerView
	var otherProjects []string
	var pmap *web.ProjectMap
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.LoadProject(r, tx); err != nil {
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
		if vars, err = s.LoadProjectVars(r, tx, p.ID); err != nil {
			return err
		}
		if peers, err = web.LoadPeers(r.Context(), tx, p.ID); err != nil {
			return err
		}
		prow, err := tx.Query(r.Context(), `SELECT slug FROM projects WHERE id <> $1 ORDER BY slug`, p.ID)
		if err != nil {
			return err
		}
		if otherProjects, err = pgx.CollectRows(prow, pgx.RowTo[string]); err != nil {
			return err
		}
		pmap, err = s.LoadProjectMap(r.Context(), tx, o, p)
		return err
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	live := 0
	for _, sv := range services {
		if sv.Status == "live" {
			live++
		}
	}
	s.Render(w, r, "project", http.StatusOK, web.Data{"Org": o, "Project": p, "Services": services, "Databases": dbs, "Variables": vars, "BaseURL": s.Cfg.BaseURL,
		"Engines": model.Engines, "Others": others, "LiveServices": live, "Peers": peers, "OtherProjects": otherProjects, "Map": pmap})
}

func (s *API) deleteProject(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	slug := r.PathValue("project")
	if r.PostFormValue("confirm") != slug {
		web.Back(w, r, o.Path()+"/p/"+slug+"#project-settings", "", "Type the project ID exactly to confirm deletion.")
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM projects WHERE slug = $1`, slug)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		s.Audit(r, tx, o.ID, "project.deleted", "project:"+slug, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if err != nil && !web.IsNoRows(err) {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/projects", "Project deleted. Its applications are being stopped.", "")
}
