package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	platmail "platform/internal/mail"
	"platform/internal/model"
	"platform/internal/secure"
)

// Operator user management (/operator/users): find any account, invite new people
// (into an existing or a new organization), disable or enable an account, end its
// sessions, unlock it, send a password reset, reset two-step login, edit the name and
// email, change memberships, grant or revoke operator rights, delete it. The operator
// never sets a password: new users and resets go through one-time links. Every change
// is audited; no tenant data is shown.

type opUser struct {
	ID, Email, Name                string
	Orgs, Passkeys                 int
	TOTP, Operator, SSOOnly        bool
	Disabled, LastLogin, LockedTil *time.Time
	DisabledReason                 string
	Created                        time.Time
}

func (u opUser) Locked() bool { return u.LockedTil != nil && u.LockedTil.After(time.Now()) }

const opUserCols = `u.id, u.email, u.name,
	(SELECT count(*) FROM memberships m WHERE m.user_id = u.id)::int,
	(SELECT count(*) FROM passkeys p WHERE p.user_id = u.id)::int,
	u.totp_enabled, u.is_operator, u.sso_only, u.disabled_at, u.last_login_at, u.locked_until, u.disabled_reason, u.created_at`

func scanOpUser(row pgx.Row) (opUser, error) {
	var u opUser
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Orgs, &u.Passkeys, &u.TOTP, &u.Operator, &u.SSOOnly,
		&u.Disabled, &u.LastLogin, &u.LockedTil, &u.DisabledReason, &u.Created)
	return u, err
}

type opInvite struct {
	ID, Email, OrgSlug, Role string
	Expires, Created         time.Time
}

func (s *Server) operatorUsersPage(w http.ResponseWriter, r *http.Request) {
	s.renderOperatorUsers(w, r, http.StatusOK, data{})
}

func (s *Server) renderOperatorUsers(w http.ResponseWriter, r *http.Request, status int, d data) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	filter := r.URL.Query().Get("filter")
	where := []string{"($1 = '' OR u.email ILIKE '%' || $1 || '%' OR u.name ILIKE '%' || $1 || '%')"}
	switch filter {
	case "operators":
		where = append(where, "u.is_operator")
	case "disabled":
		where = append(where, "u.disabled_at IS NOT NULL")
	case "no2fa":
		where = append(where, "NOT u.totp_enabled AND NOT u.sso_only")
	case "locked":
		where = append(where, "u.locked_until > now()")
	default:
		filter = ""
	}
	rows, err := s.pool.Query(r.Context(), `SELECT `+opUserCols+` FROM users u WHERE `+strings.Join(where, " AND ")+` ORDER BY u.created_at DESC LIMIT 300`, q)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (opUser, error) { return scanOpUser(row) })
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err = s.pool.Query(r.Context(), `
		SELECT i.id, i.email, o.slug, i.role, i.expires_at, i.created_at FROM invites i JOIN orgs o ON o.id = i.org_id
		WHERE i.accepted_at IS NULL AND i.expires_at > now() ORDER BY i.created_at DESC LIMIT 200`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	invites, err := pgx.CollectRows(rows, pgx.RowToStructByPos[opInvite])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var orgs, plans []string
	for _, x := range []struct {
		sql string
		out *[]string
	}{{`SELECT slug FROM orgs ORDER BY slug`, &orgs}, {`SELECT id FROM plans ORDER BY max_cpu_millis, id`, &plans}} {
		rows, err := s.pool.Query(r.Context(), x.sql)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if *x.out, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	d["Users"], d["Invites"], d["Orgs"], d["Plans"], d["Q"], d["Filter"] = users, invites, orgs, plans, q, filter
	if _, ok := d["Mail"]; !ok {
		d["Mail"] = platmail.Configured(s.cfg)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "operator_users", status, d)
}

func opAudit(r *http.Request, s *Server, orgID, action, target string, detail any) {
	_ = db.Audit(r.Context(), s.pool, orgID, sessionFrom(r).UserID, action, target, detail, clientIP(r))
}

// operatorCreateUser invites a person into an organization (an existing one, or a new
// one they will own). Someone who already has an account is added directly.
func (s *Server) operatorCreateUser(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	role := r.PostFormValue("role")
	fail := func(msg string) { back(w, r, "/operator/users#create", "", msg) }
	if _, addr, err := platmail.ParseAddress(email); err != nil || addr != email || len(email) > 254 {
		fail("Enter a valid email address.")
		return
	}
	newOrg := r.PostFormValue("org_mode") == "new"
	slug := strings.TrimSpace(r.PostFormValue("org_slug"))
	if newOrg {
		slug = strings.ToLower(strings.TrimSpace(r.PostFormValue("new_slug")))
		role = "owner"
		if !model.ValidSlug(slug) || strings.TrimSpace(r.PostFormValue("new_name")) == "" {
			fail("A new organization needs an ID (2 to 20 lowercase letters, digits and dashes) and a name.")
			return
		}
	}
	if !model.ValidRole(role) {
		fail("Choose a role.")
		return
	}
	var link, orgID, existingUser string
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		if newOrg {
			plan := r.PostFormValue("plan")
			if err := tx.QueryRow(r.Context(), `
				INSERT INTO orgs (slug, name, plan_id, trial_ends_at) SELECT $1, $2, id, CASE WHEN id = 'trial' THEN now() + interval '14 days' END
				FROM plans WHERE id = $3 RETURNING id`, slug, strings.TrimSpace(r.PostFormValue("new_name")), plan).Scan(&orgID); err != nil {
				if isUniqueViolation(err) {
					return apiErr{http.StatusConflict, "An organization with this ID already exists."}
				}
				if isNoRows(err) {
					return apiErr{http.StatusUnprocessableEntity, "Choose a plan."}
				}
				return err
			}
			if err := db.Audit(r.Context(), tx, orgID, sessionFrom(r).UserID, "org.created", "org:"+slug, map[string]any{"plan": plan, "by_operator": true}, clientIP(r)); err != nil {
				return err
			}
		} else if err := tx.QueryRow(r.Context(), `SELECT id FROM orgs WHERE slug = $1`, slug).Scan(&orgID); err != nil {
			return apiErr{http.StatusUnprocessableEntity, "Choose an organization."}
		}
		_ = tx.QueryRow(r.Context(), `SELECT id FROM users WHERE email = $1`, email).Scan(&existingUser)
		if existingUser != "" {
			tag, err := tx.Exec(r.Context(), `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, orgID, existingUser, role)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return apiErr{http.StatusConflict, email + " is already a member of " + slug + "."}
			}
			return db.Audit(r.Context(), tx, orgID, sessionFrom(r).UserID, "member.added_by_operator", "user:"+existingUser, map[string]string{"role": role}, clientIP(r))
		}
		token := secure.NewToken()
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO invites (org_id, email, role, token_hash, created_by, expires_at) VALUES ($1, $2, $3, $4, $5, now() + interval '7 days')`,
			orgID, email, role, secure.HashToken(token), sessionFrom(r).UserID); err != nil {
			return err
		}
		link = s.cfg.BaseURL + "/invite/" + token
		return db.Audit(r.Context(), tx, orgID, sessionFrom(r).UserID, "invite.created", "email:"+email, map[string]any{"role": role, "by_operator": true}, clientIP(r))
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		fail(ae.msg)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	case existingUser != "":
		back(w, r, "/operator/users/"+existingUser, email+" already had an account: added to "+slug+" as "+role+".", "")
		return
	}
	mailed := false
	if r.PostFormValue("send_email") == "1" && platmail.Configured(s.cfg) {
		mailed = true
		go func(to, link, org, role string) {
			if err := platmail.Send(s.cfg, to, "You are invited to "+org,
				"You have been invited to the organization "+org+" ("+role+") on the platform.\n\n"+
					"Create your account here (the link works once, within 7 days):\n\n"+link+"\n"); err != nil {
				s.log.Error("send invite email", "err", err)
			}
		}(email, link, slug, role)
	}
	s.renderOperatorUsers(w, r, http.StatusOK, data{"Link": link, "LinkFor": email, "LinkKind": "invite", "Mailed": mailed,
		"Msg": "Invitation created for " + email + " (" + role + " of " + slug + ")."})
}

func (s *Server) operatorRevokeInvite(w http.ResponseWriter, r *http.Request) {
	var orgID, email string
	err := s.pool.QueryRow(r.Context(), `DELETE FROM invites WHERE id::text = $1 AND accepted_at IS NULL RETURNING org_id, email`, r.PathValue("id")).Scan(&orgID, &email)
	if err != nil {
		back(w, r, "/operator/users#invites", "", "No pending invitation.")
		return
	}
	opAudit(r, s, orgID, "invite.revoked", "email:"+email, map[string]bool{"by_operator": true})
	back(w, r, "/operator/users#invites", "Invitation for "+email+" revoked.", "")
}

// ---------- one user ----------

type opMembership struct {
	OrgSlug, OrgName, Role string
	Created                time.Time
}

type opSession struct {
	IP                string
	TOTPOK            bool
	Created, LastSeen time.Time
}

func (s *Server) loadOpUser(ctx context.Context, id string) (opUser, error) {
	return scanOpUser(s.pool.QueryRow(ctx, `SELECT `+opUserCols+` FROM users u WHERE u.id::text = $1`, id))
}

func (s *Server) operatorUserPage(w http.ResponseWriter, r *http.Request) {
	s.renderOperatorUser(w, r, http.StatusOK, data{})
}

func (s *Server) renderOperatorUser(w http.ResponseWriter, r *http.Request, status int, d data) {
	u, err := s.loadOpUser(r.Context(), r.PathValue("id"))
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT o.slug, o.name, m.role, m.created_at FROM memberships m JOIN orgs o ON o.id = m.org_id WHERE m.user_id = $1 ORDER BY o.slug`, u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	members, err := pgx.CollectRows(rows, pgx.RowToStructByPos[opMembership])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err = s.pool.Query(r.Context(), `
		SELECT coalesce(ip, ''), totp_ok, created_at, last_seen FROM sessions
		WHERE user_id = $1 AND expires_at > now() AND last_seen > now() - interval '2 hours' ORDER BY last_seen DESC`, u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sessions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[opSession])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	type ev struct {
		At                          time.Time
		OrgSlug, Action, Target, IP string
	}
	rows, err = s.pool.Query(r.Context(), `SELECT * FROM operator_user_audit($1)`, u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ev])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err = s.pool.Query(r.Context(), `SELECT slug FROM orgs WHERE id NOT IN (SELECT org_id FROM memberships WHERE user_id = $1) ORDER BY slug`, u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d["U"], d["Memberships"], d["Sessions"], d["Events"], d["OtherOrgs"] = u, members, sessions, events, others
	d["Self"] = u.ID == sessionFrom(r).UserID
	d["Roles"] = []string{"owner", "admin", "developer", "viewer"}
	d["Mail"] = platmail.Configured(s.cfg)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "operator_user", status, d)
}

// withOpUser loads the user of the path for an action; refuse self lists actions an
// operator may not take on their own account.
func (s *Server) withOpUser(w http.ResponseWriter, r *http.Request, refuseSelf bool) (opUser, bool) {
	u, err := s.loadOpUser(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return u, false
	}
	if refuseSelf && u.ID == sessionFrom(r).UserID {
		back(w, r, "/operator/users/"+u.ID, "", "You cannot do this to your own account.")
		return u, false
	}
	return u, true
}

func (s *Server) operatorUserDisable(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, true)
	if !ok {
		return
	}
	on := r.PostFormValue("on") == "1"
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if len(reason) > 200 {
		reason = reason[:200]
	}
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		if on {
			if _, err := tx.Exec(r.Context(), `UPDATE users SET disabled_at = now(), disabled_reason = $2 WHERE id = $1`, u.ID, reason); err != nil {
				return err
			}
			_, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id = $1`, u.ID)
			return err
		}
		_, err := tx.Exec(r.Context(), `UPDATE users SET disabled_at = NULL, disabled_reason = '', failed_logins = 0, locked_until = NULL WHERE id = $1`, u.ID)
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if on {
		opAudit(r, s, "", "user.disabled", "user:"+u.ID, map[string]string{"reason": reason})
		back(w, r, "/operator/users/"+u.ID, u.Email+" disabled: signed out everywhere and cannot sign in.", "")
		return
	}
	opAudit(r, s, "", "user.enabled", "user:"+u.ID, nil)
	back(w, r, "/operator/users/"+u.ID, u.Email+" can sign in again.", "")
}

func (s *Server) operatorUserSignout(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, true)
	if !ok {
		return
	}
	tag, err := s.pool.Exec(r.Context(), `DELETE FROM sessions WHERE user_id = $1`, u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	opAudit(r, s, "", "user.signed_out_by_operator", "user:"+u.ID, map[string]int64{"sessions": tag.RowsAffected()})
	back(w, r, "/operator/users/"+u.ID, u.Email+" signed out everywhere.", "")
}

func (s *Server) operatorUserUnlock(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, false)
	if !ok {
		return
	}
	if _, err := s.pool.Exec(r.Context(), `UPDATE users SET failed_logins = 0, locked_until = NULL WHERE id = $1`, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	opAudit(r, s, "", "user.unlocked", "user:"+u.ID, nil)
	back(w, r, "/operator/users/"+u.ID, u.Email+" unlocked.", "")
}

func (s *Server) operatorUserResetPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, false)
	if !ok {
		return
	}
	link, err := s.createPasswordReset(r.Context(), u.ID, "operator", clientIP(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	opAudit(r, s, "", "user.password_reset_by_operator", "user:"+u.ID, nil)
	mailed := false
	if r.PostFormValue("send_email") == "1" && platmail.Configured(s.cfg) {
		mailed = true
		go func(to, link string) {
			if err := platmail.Send(s.cfg, to, "Reset your password",
				"The platform's support created a password reset for your account.\n\n"+
					"Choose a new password here (the link works once, within 1 hour):\n\n"+link+"\n\n"+
					"If you did not ask for this, contact the support.\n"); err != nil {
				s.log.Error("send reset email", "err", err)
			}
		}(u.Email, link)
	}
	s.renderOperatorUser(w, r, http.StatusOK, data{"Link": link, "LinkKind": "reset", "Mailed": mailed, "Msg": "Password reset link created."})
}

func (s *Server) operatorUserReset2FA(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, true)
	if !ok {
		return
	}
	passkeys := r.PostFormValue("passkeys") == "1"
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`UPDATE users SET totp_enabled = false, totp_secret_enc = NULL, totp_last_step = 0 WHERE id = $1`,
			`DELETE FROM recovery_codes WHERE user_id = $1`,
			`DELETE FROM sessions WHERE user_id = $1`,
		} {
			if _, err := tx.Exec(r.Context(), q, u.ID); err != nil {
				return err
			}
		}
		if passkeys {
			if _, err := tx.Exec(r.Context(), `DELETE FROM passkeys WHERE user_id = $1`, u.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	opAudit(r, s, "", "totp.reset_by_operator", "user:"+u.ID, map[string]bool{"passkeys_removed": passkeys})
	back(w, r, "/operator/users/"+u.ID, "Two-step login reset for "+u.Email+": they set it up again at their next sign-in.", "")
}

func (s *Server) operatorUserProfile(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, false)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	if _, addr, err := platmail.ParseAddress(email); err != nil || addr != email || name == "" || len(name) > 100 {
		back(w, r, "/operator/users/"+u.ID, "", "Enter a name (up to 100 characters) and a valid email address.")
		return
	}
	_, err := s.pool.Exec(r.Context(), `UPDATE users SET name = $2, email = $3 WHERE id = $1`, u.ID, name, email)
	if isUniqueViolation(err) {
		back(w, r, "/operator/users/"+u.ID, "", "Another account already uses "+email+".")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	detail := map[string]string{}
	if email != u.Email {
		detail["email_from"], detail["email_to"] = u.Email, email
	}
	if name != u.Name {
		detail["name"] = name
	}
	opAudit(r, s, "", "user.updated_by_operator", "user:"+u.ID, detail)
	back(w, r, "/operator/users/"+u.ID, "Profile saved.", "")
}

func (s *Server) operatorUserOperator(w http.ResponseWriter, r *http.Request) {
	on := r.PostFormValue("on") == "1"
	u, ok := s.withOpUser(w, r, !on)
	if !ok {
		return
	}
	if on && !u.TOTP && !u.SSOOnly {
		back(w, r, "/operator/users/"+u.ID, "", "Operators need two-step login: "+u.Email+" has not set it up.")
		return
	}
	if _, err := s.pool.Exec(r.Context(), `UPDATE users SET is_operator = $2 WHERE id = $1`, u.ID, on); err != nil {
		s.serverError(w, r, err)
		return
	}
	action, msg := "operator.granted", u.Email+" is now an operator."
	if !on {
		action, msg = "operator.revoked", u.Email+" is no longer an operator."
	}
	opAudit(r, s, "", action, "user:"+u.ID, nil)
	back(w, r, "/operator/users/"+u.ID, msg, "")
}

// operatorUserMembership adds the user to an organization, changes their role or
// removes them; an organization always keeps at least one owner.
func (s *Server) operatorUserMembership(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, false)
	if !ok {
		return
	}
	slug, role, action := r.PostFormValue("org"), r.PostFormValue("role"), r.PostFormValue("action")
	if action != "remove" && !model.ValidRole(role) {
		back(w, r, "/operator/users/"+u.ID+"#orgs", "", "Choose a role.")
		return
	}
	var orgID string
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT id FROM orgs WHERE slug = $1 FOR UPDATE`, slug).Scan(&orgID); err != nil {
			return apiErr{http.StatusNotFound, "Unknown organization."}
		}
		var current string
		_ = tx.QueryRow(r.Context(), `SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, u.ID).Scan(&current)
		if action == "add" {
			if current != "" {
				return apiErr{http.StatusConflict, u.Email + " is already a member of " + slug + "."}
			}
			_, err := tx.Exec(r.Context(), `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`, orgID, u.ID, role)
			return err
		}
		if current == "" {
			return apiErr{http.StatusNotFound, u.Email + " is not a member of " + slug + "."}
		}
		if current == "owner" && (action == "remove" || role != "owner") {
			var owners int
			if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner'`, orgID).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return apiErr{http.StatusConflict, u.Email + " is the only owner of " + slug + ": make someone else owner first."}
			}
		}
		if action == "remove" {
			_, err := tx.Exec(r.Context(), `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, u.ID)
			return err
		}
		_, err := tx.Exec(r.Context(), `UPDATE memberships SET role = $3 WHERE org_id = $1 AND user_id = $2`, orgID, u.ID, role)
		return err
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, "/operator/users/"+u.ID+"#orgs", "", ae.msg)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	msg, act := "Role in "+slug+" set to "+role+".", "member.role_changed_by_operator"
	switch action {
	case "add":
		msg, act = "Added to "+slug+" as "+role+".", "member.added_by_operator"
	case "remove":
		msg, act = "Removed from "+slug+".", "member.removed_by_operator"
	}
	opAudit(r, s, orgID, act, "user:"+u.ID, map[string]string{"role": role})
	back(w, r, "/operator/users/"+u.ID+"#orgs", msg, "")
}

// operatorUserDelete deletes an account that owns no organization alone.
func (s *Server) operatorUserDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := s.withOpUser(w, r, true)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != u.Email {
		back(w, r, "/operator/users/"+u.ID+"#danger", "", "Type the email address to delete the account.")
		return
	}
	var sole []string
	rows, err := s.pool.Query(r.Context(), `
		SELECT o.slug FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.user_id = $1 AND m.role = 'owner'
		  AND NOT EXISTS (SELECT 1 FROM memberships x WHERE x.org_id = m.org_id AND x.role = 'owner' AND x.user_id <> m.user_id)`, u.ID)
	if err == nil {
		sole, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(sole) > 0 {
		back(w, r, "/operator/users/"+u.ID+"#danger", "", u.Email+" is the only owner of "+strings.Join(sole, ", ")+": give the organization another owner first.")
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM users WHERE id = $1`, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	opAudit(r, s, "", "user.deleted", "user:"+u.ID, map[string]string{"email": u.Email})
	back(w, r, "/operator/users", "Account "+u.Email+" deleted.", "")
}
