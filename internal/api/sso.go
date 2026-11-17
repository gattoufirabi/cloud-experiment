package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/netguard"
	"platform/internal/oidc"
	"platform/internal/secure"
)

// Single sign-on per organization with OpenID Connect. An admin sets the provider
// (issuer, client ID and secret), the email domains that belong to the organization
// and how provider groups map to roles. Members then sign in from the login page with
// their work email; accounts are created on first sign-in. With "enforce", password
// sign-in is refused for emails of those domains (owners keep it, to recover from a
// broken provider). The callback address to register at the provider is /sso/callback.

type ssoConfig struct {
	Enabled, Enforce, HasSecret                         bool
	Issuer, ClientID, DefaultRole, GroupsClaim          string
	Domains, AdminGroups, DeveloperGroups, ViewerGroups []string
	SecretEnc                                           []byte
	Updated                                             *time.Time
}

func (c *ssoConfig) DomainsText() string   { return strings.Join(c.Domains, ", ") }
func (c *ssoConfig) AdminText() string     { return strings.Join(c.AdminGroups, ", ") }
func (c *ssoConfig) DeveloperText() string { return strings.Join(c.DeveloperGroups, ", ") }
func (c *ssoConfig) ViewerText() string    { return strings.Join(c.ViewerGroups, ", ") }

func (s *Server) loadSSO(ctx context.Context, orgID string) (*ssoConfig, error) {
	var c ssoConfig
	err := s.pool.QueryRow(ctx, `
		SELECT enabled, enforce, client_secret_enc IS NOT NULL, issuer, client_id, default_role, groups_claim,
		       domains, admin_groups, developer_groups, viewer_groups, coalesce(client_secret_enc, '\x'::bytea), updated_at
		FROM org_sso WHERE org_id = $1`, orgID).
		Scan(&c.Enabled, &c.Enforce, &c.HasSecret, &c.Issuer, &c.ClientID, &c.DefaultRole, &c.GroupsClaim,
			&c.Domains, &c.AdminGroups, &c.DeveloperGroups, &c.ViewerGroups, &c.SecretEnc, &c.Updated)
	if isNoRows(err) {
		return nil, nil
	}
	return &c, err
}

func (s *Server) oidcClient() *oidc.Client {
	s.oidcOnce.Do(func() { s.oidc = oidc.NewClient(netguard.Client(s.cfg.OIDCAllowInsecure)) })
	return s.oidc
}

func ssoCallback(s *Server) string { return s.cfg.BaseURL + "/sso/callback" }

func splitList(v string) []string {
	out := []string{} // never NULL in the database
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == ';' }) {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// saveSSO stores the organization's provider settings, checking that discovery works.
func (s *Server) saveSSO(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	dest := o.Path() + "/iam#sso"
	issuer := strings.TrimRight(strings.TrimSpace(r.PostFormValue("issuer")), "/")
	clientID := strings.TrimSpace(r.PostFormValue("client_id"))
	secret := r.PostFormValue("client_secret")
	domains := splitList(strings.ToLower(r.PostFormValue("domains")))
	role := r.PostFormValue("default_role")
	claim := strings.TrimSpace(r.PostFormValue("groups_claim"))
	if claim == "" {
		claim = "groups"
	}
	enabled, enforce := r.PostFormValue("enabled") == "1", r.PostFormValue("enforce") == "1"
	fail := func(msg string) { back(w, r, dest, "", msg) }
	switch {
	case issuer == "" || clientID == "":
		fail("Give the provider's issuer URL and the client ID.")
		return
	case netguard.ValidateURL(issuer, !s.cfg.OIDCAllowInsecure, s.cfg.OIDCAllowInsecure) != nil:
		fail("Issuer: an https:// URL on a public address, such as https://accounts.google.com.")
		return
	case role != "viewer" && role != "developer" && role != "admin":
		fail("Choose the role of new members.")
		return
	case enforce && (!enabled || len(domains) == 0):
		fail("To require single sign-on, turn it on and list your email domains.")
		return
	}
	for _, d := range domains {
		if !model.ValidHostname(d) || !strings.Contains(d, ".") {
			fail("Domain " + d + " is not a domain name.")
			return
		}
	}
	if enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		_, err := s.oidcClient().Discover(ctx, issuer)
		cancel()
		if err != nil {
			fail("The provider could not be reached: " + err.Error())
			return
		}
	}
	var formErr string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var secretEnc any
		if secret != "" {
			enc, err := s.box.Seal([]byte(secret), "sso:"+o.ID)
			if err != nil {
				return err
			}
			secretEnc = enc
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO org_sso (org_id, enabled, issuer, client_id, client_secret_enc, domains, default_role, groups_claim,
			                     admin_groups, developer_groups, viewer_groups, enforce, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
			ON CONFLICT (org_id) DO UPDATE SET enabled = $2, issuer = $3, client_id = $4,
			       client_secret_enc = coalesce($5, org_sso.client_secret_enc), domains = $6, default_role = $7, groups_claim = $8,
			       admin_groups = $9, developer_groups = $10, viewer_groups = $11, enforce = $12, updated_at = now()`,
			o.ID, enabled, issuer, clientID, secretEnc, domains, role, claim,
			splitList(r.PostFormValue("admin_groups")), splitList(r.PostFormValue("developer_groups")), splitList(r.PostFormValue("viewer_groups")), enforce); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM org_sso_domains WHERE org_id = $1`, o.ID); err != nil {
			return err
		}
		for _, d := range domains {
			if _, err := tx.Exec(r.Context(), `INSERT INTO org_sso_domains (domain, org_id) VALUES ($1, $2)`, d, o.ID); err != nil {
				if isUniqueViolation(err) {
					formErr = "The domain " + d + " already belongs to another organization."
					return errRollback
				}
				return err
			}
		}
		s.audit(r, tx, o.ID, "sso.configured", "org:"+o.Slug, map[string]any{"enabled": enabled, "issuer": issuer, "domains": domains, "enforce": enforce})
		return nil
	})
	switch {
	case formErr != "":
		fail(formErr)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, dest, "Single sign-on settings saved.", "")
	}
}

var errRollback = errors.New("rollback")

// ssoStart begins a sign-in from the login page: by work email (its domain names the
// organization) or by organization ID.
func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) {
	who := strings.ToLower(strings.TrimSpace(r.PostFormValue("who")))
	fail := func(status int, msg string) { s.render(w, r, "login", status, data{"SSOErr": msg, "Who": who}) }
	if !s.limiter.allow("sso-ip:"+clientIP(r), 30, 15*time.Minute) {
		fail(http.StatusTooManyRequests, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	var orgID, hint string
	var err error
	if _, domain, ok := strings.Cut(who, "@"); ok {
		hint = who
		err = s.pool.QueryRow(r.Context(), `SELECT org_id FROM org_sso_domains WHERE domain = $1`, domain).Scan(&orgID)
	} else {
		err = s.pool.QueryRow(r.Context(), `SELECT id FROM orgs WHERE slug = $1`, who).Scan(&orgID)
	}
	var c *ssoConfig
	if err == nil {
		c, err = s.loadSSO(r.Context(), orgID)
	}
	if err != nil || c == nil || !c.Enabled {
		fail(http.StatusNotFound, "No single sign-on is set up for this email or organization. Sign in with your password, or ask your administrator.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := s.oidcClient().Discover(ctx, c.Issuer)
	if err != nil {
		s.log.Warn("sso discovery", "org", orgID, "err", err)
		fail(http.StatusBadGateway, "Your organization's sign-in provider cannot be reached right now.")
		return
	}
	state, nonce, verifier := oidc.Random(), oidc.Random(), oidc.Random()
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM sso_logins WHERE created_at < now() - interval '10 minutes'`); err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := s.pool.Exec(r.Context(), `INSERT INTO sso_logins (state, org_id, nonce, code_verifier) VALUES ($1, $2, $3, $4)`,
		state, orgID, nonce, verifier); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, oidc.AuthURL(p, c.ClientID, ssoCallback(s), state, nonce, verifier, hint), http.StatusSeeOther)
}

// roleFor maps the provider's groups to a role ("" when no group list matches).
func (c *ssoConfig) roleFor(groups []string) string {
	has := func(list []string) bool {
		for _, g := range groups {
			if slices.Contains(list, g) {
				return true
			}
		}
		return false
	}
	switch {
	case has(c.AdminGroups):
		return "admin"
	case has(c.DeveloperGroups):
		return "developer"
	case has(c.ViewerGroups):
		return "viewer"
	}
	return ""
}

func (c *ssoConfig) mapsGroups() bool {
	return len(c.AdminGroups)+len(c.DeveloperGroups)+len(c.ViewerGroups) > 0
}

// ssoCallback completes a sign-in: checks the ID token, finds or creates the account,
// gives it its membership and role, and opens a session (the provider did the
// authentication, including its own two-step checks).
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) { s.render(w, r, "login", http.StatusForbidden, data{"SSOErr": msg}) }
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail("Sign-in was cancelled or refused by your provider (" + e + ").")
		return
	}
	var orgID, nonce, verifier string
	err := s.pool.QueryRow(r.Context(), `DELETE FROM sso_logins WHERE state = $1 AND created_at > now() - interval '10 minutes' RETURNING org_id, nonce, code_verifier`,
		q.Get("state")).Scan(&orgID, &nonce, &verifier)
	if err != nil {
		fail("This sign-in link expired or was already used. Start again.")
		return
	}
	c, err := s.loadSSO(r.Context(), orgID)
	if err != nil || c == nil || !c.Enabled {
		fail("Single sign-on is not set up for this organization any more.")
		return
	}
	var secret string
	if len(c.SecretEnc) > 0 {
		b, err := s.box.Open(c.SecretEnc, "sso:"+orgID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		secret = string(b)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cli := s.oidcClient()
	p, err := cli.Discover(ctx, c.Issuer)
	var raw string
	if err == nil {
		raw, err = cli.Exchange(ctx, p, c.ClientID, secret, ssoCallback(s), q.Get("code"), verifier)
	}
	var claims oidc.Claims
	if err == nil {
		claims, err = cli.Verify(ctx, p, c.ClientID, raw, nonce)
	}
	if err != nil {
		s.log.Warn("sso callback", "org", orgID, "err", err)
		fail("Your provider's answer could not be verified. Try again, or ask your administrator to check the single sign-on settings.")
		return
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	_, domain, _ := strings.Cut(email, "@")
	owned := slices.Contains(c.Domains, domain)
	switch {
	case email == "" || domain == "":
		fail("Your provider did not share your email address.")
		return
	case len(c.Domains) > 0 && !owned:
		fail("Your email is not in this organization's domains.")
		return
	case !claims.EmailVerified && !owned:
		fail("Your provider has not verified your email address.")
		return
	}
	mapped := c.roleFor(claims.Groups(c.GroupsClaim))
	var userID, orgSlug string
	err = db.WithOrg(r.Context(), s.pool, orgID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT slug FROM orgs WHERE id = $1 AND NOT suspended`, orgID).Scan(&orgSlug); err != nil {
			return err
		}
		err := tx.QueryRow(r.Context(), `SELECT user_id FROM user_identities WHERE issuer = $1 AND subject = $2`, p.Issuer, claims.Subject).Scan(&userID)
		if isNoRows(err) {
			err = tx.QueryRow(r.Context(), `SELECT id FROM users WHERE email = $1`, email).Scan(&userID)
			if isNoRows(err) {
				name := claims.Name
				if name == "" {
					name, _, _ = strings.Cut(email, "@")
				}
				hash, herr := secure.HashPassword(secure.NewToken())
				if herr != nil {
					return herr
				}
				err = tx.QueryRow(r.Context(), `INSERT INTO users (email, name, password_hash, sso_only) VALUES ($1, $2, $3, true) RETURNING id`,
					email, truncateStr(name, 100), hash).Scan(&userID)
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO user_identities (issuer, subject, user_id, org_id) VALUES ($1, $2, $3, $4)`,
				p.Issuer, claims.Subject, userID, orgID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		var current string
		err = tx.QueryRow(r.Context(), `SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID).Scan(&current)
		switch {
		case isNoRows(err):
			role := mapped
			if role == "" {
				role = c.DefaultRole
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`, orgID, userID, role); err != nil {
				return err
			}
			_ = db.Audit(r.Context(), tx, orgID, userID, "member.joined", "user:"+email, map[string]string{"role": role, "via": "sso"}, clientIP(r))
		case err != nil:
			return err
		case mapped != "" && c.mapsGroups() && current != "owner" && current != mapped: // the provider's groups decide the role
			if _, err := tx.Exec(r.Context(), `UPDATE memberships SET role = $3 WHERE org_id = $1 AND user_id = $2`, orgID, userID, mapped); err != nil {
				return err
			}
			_ = db.Audit(r.Context(), tx, orgID, userID, "member.role_changed", "user:"+email, map[string]string{"from": current, "to": mapped, "via": "sso groups"}, clientIP(r))
		}
		if _, err := tx.Exec(r.Context(), `UPDATE user_identities SET last_login = now() WHERE issuer = $1 AND subject = $2`, p.Issuer, claims.Subject); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, orgID, userID, "login.sso", "user:"+email, map[string]string{"issuer": p.Issuer}, clientIP(r))
	})
	if err != nil {
		s.log.Warn("sso sign-in", "org", orgID, "err", err)
		fail("Your account could not be opened. Ask your administrator.")
		return
	}
	if err := s.startSession(w, r, userID, true); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/o/"+orgSlug, http.StatusSeeOther)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ssoRequired names the organization that requires single sign-on for this email
// (password sign-in refused), unless the user owns that organization.
func (s *Server) ssoRequired(ctx context.Context, userID, email string) string {
	_, domain, _ := strings.Cut(email, "@")
	var name string
	err := s.pool.QueryRow(ctx, `
		SELECT o.name FROM org_sso_domains d JOIN org_sso c ON c.org_id = d.org_id JOIN orgs o ON o.id = d.org_id
		WHERE d.domain = $1 AND c.enabled AND c.enforce
		  AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.org_id = d.org_id AND m.user_id = $2 AND m.role = 'owner')`,
		domain, userID).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

var _ = fmt.Sprintf
