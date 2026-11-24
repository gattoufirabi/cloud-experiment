package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	platmail "platform/internal/lib/mail"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

// Self-signup (SIGNUP_ENABLED). Abuse controls, cheapest first:
//   - honeypot field and per-IP / per-day limits;
//   - a proof-of-work challenge solved by the browser (no third-party CAPTCHA);
//   - disposable email domains refused (built-in list + SIGNUP_BLOCKED_DOMAINS), or
//     only SIGNUP_ALLOWED_DOMAINS accepted;
//   - the email address must be confirmed; the confirm link needs a click on a button,
//     so mail scanners that open links do not consume it;
//   - new organizations start on the Trial plan (small, no custom domains, no builds,
//     time-limited), and the agent suspends trials that look like crypto-mining.
// The answer is the same whether or not the address already has an account.

var disposableDomains = map[string]bool{
	"mailinator.com": true, "guerrillamail.com": true, "guerrillamail.net": true, "sharklasers.com": true,
	"10minutemail.com": true, "10minutemail.net": true, "tempmail.com": true, "temp-mail.org": true,
	"yopmail.com": true, "yopmail.net": true, "trashmail.com": true, "trashmail.de": true, "getnada.com": true,
	"dispostable.com": true, "maildrop.cc": true, "throwawaymail.com": true, "mintemail.com": true,
	"mohmal.com": true, "fakeinbox.com": true, "mailnesia.com": true, "emailondeck.com": true, "tempail.com": true,
	"moakt.com": true, "burnermail.io": true, "spamgourmet.com": true, "mytemp.email": true, "tempr.email": true,
}

func NewPowStore() *web.PowStore {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &web.PowStore{Key: k, Used: map[string]time.Time{}}
}

// ---------- pages ----------

func (s *API) signupPage(w http.ResponseWriter, r *http.Request) {
	if !s.Settings(r.Context()).SignupEnabled {
		s.NotFound(w, r)
		return
	}
	s.renderSignup(w, r, http.StatusOK, web.Data{})
}

func (s *API) renderSignup(w http.ResponseWriter, r *http.Request, status int, d web.Data) {
	d["Challenge"] = s.Pow.Challenge(s.Cfg.SignupPowBits)
	d["TrialDays"] = s.Settings(r.Context()).SignupTrialDays
	s.Render(w, r, "signup", status, d)
}

func emailDomain(email string) string { return email[strings.LastIndex(email, "@")+1:] }

func (s *API) signupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.Settings(r.Context()).SignupEnabled {
		s.NotFound(w, r)
		return
	}
	ip := web.ClientIP(r)
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	name := strings.TrimSpace(r.PostFormValue("name"))
	orgName := strings.TrimSpace(r.PostFormValue("org_name"))
	slug := strings.ToLower(strings.TrimSpace(r.PostFormValue("org_slug")))
	pw := r.PostFormValue("password")
	form := web.Data{"Email": email, "Name": name, "OrgName": orgName, "OrgSlug": slug}
	again := func(msg string) {
		form["Err"] = msg
		s.renderSignup(w, r, http.StatusBadRequest, form)
	}
	sent := func() { s.Render(w, r, "signup_sent", http.StatusOK, web.Data{"Email": email}) }

	if r.PostFormValue("website") != "" { // honeypot: bots fill every field
		sent()
		return
	}
	if !s.Limiter.Allow("signup:"+ip, s.Cfg.SignupsPerHour, time.Hour) {
		again("Too many sign-ups from your network; try again in an hour.")
		return
	}
	if !s.Pow.Verify(r.PostFormValue("challenge"), r.PostFormValue("nonce"), s.Cfg.SignupPowBits) {
		again("The browser check did not complete (JavaScript is needed). Please submit the form again.")
		return
	}
	addr, err := mail.ParseAddress(email)
	switch {
	case err != nil || addr.Address != email || len(email) > 200 || !strings.Contains(emailDomain(email), "."):
		again("Please enter a valid email address.")
		return
	case disposableDomains[emailDomain(email)] || web.Contains(s.Settings(r.Context()).SignupBlocked, emailDomain(email)):
		again("Please use a permanent email address.")
		return
	case len(s.Settings(r.Context()).SignupAllowed) > 0 && !web.Contains(s.Settings(r.Context()).SignupAllowed, emailDomain(email)):
		again("Sign-up is limited to approved email domains.")
		return
	case name == "" || len(name) > 100:
		again("Please enter your name (up to 100 characters).")
		return
	case len(pw) < 12 || len(pw) > 256:
		again("The password must be between 12 and 256 characters.")
		return
	case pw != r.PostFormValue("password2"):
		again("The two passwords do not match.")
		return
	case orgName == "" || len(orgName) > 100:
		again("Please name your organization (up to 100 characters).")
		return
	case !model.ValidSlug(slug):
		again("Organization ID: 2 to 20 characters, lowercase letters, digits and dashes.")
		return
	}
	var taken bool
	if err := s.Pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM orgs WHERE slug = $1)`, slug).Scan(&taken); err != nil {
		s.ServerError(w, r, err)
		return
	}
	if taken {
		again("This organization ID is taken; choose another.")
		return
	}
	var today int
	if err := s.Pool.QueryRow(r.Context(), `SELECT count(*) FROM signups WHERE created_at > now() - interval '1 day'`).Scan(&today); err != nil {
		s.ServerError(w, r, err)
		return
	}
	if today >= s.Settings(r.Context()).SignupDailyLimit {
		again("Sign-ups are paused for today; please try again tomorrow.")
		return
	}
	if !platmail.Configured(s.Cfg) {
		again("Sign-up is not available right now (email is not configured).")
		return
	}
	var exists bool
	_ = s.Pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists)
	if exists {
		// Same answer on screen; the owner of the address learns it already has an account.
		go func() {
			_ = s.SendMail(r.Context(), email, "Your "+s.Cfg.ProductName+" account",
				"Someone (maybe you) tried to sign up with this address, but it already has an account.\n\n"+
					"Log in at "+s.Cfg.BaseURL+"/login\n\nIf this wasn't you, you can ignore this message.\n")
		}()
		sent()
		return
	}
	hash, err := secure.HashPassword(pw)
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	token := secure.NewToken()
	var id string
	if err := s.Pool.QueryRow(r.Context(), `
		INSERT INTO signups (email, name, password_hash, org_slug, org_name, token_hash, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + interval '24 hours') RETURNING id`,
		email, name, hash, slug, orgName, secure.HashToken(token), ip).Scan(&id); err != nil {
		s.ServerError(w, r, err)
		return
	}
	body := "Hello " + name + ",\n\nConfirm your email address to create the organization \"" + orgName + "\":\n\n" +
		s.Cfg.BaseURL + "/signup/verify/" + token + "\n\nThe link works once, within 24 hours. " +
		"If you did not sign up, ignore this message; nothing will be created.\n"
	if err := s.SendMail(r.Context(), email, "Confirm your email address", body); err != nil {
		s.Log.Error("send signup email", "err", err)
		_, _ = s.Pool.Exec(r.Context(), `DELETE FROM signups WHERE id = $1`, id)
		again("We could not send the confirmation email; please try again later.")
		return
	}
	sent()
}

func (s *API) loadSignup(r *http.Request) (id, email, name, hash, slug, orgName string, err error) {
	err = s.Pool.QueryRow(r.Context(), `
		SELECT id, email, name, password_hash, org_slug, org_name FROM signups
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`, secure.HashToken(r.PathValue("token"))).
		Scan(&id, &email, &name, &hash, &slug, &orgName)
	return
}

// signupVerifyPage shows a confirm button: opening the link alone changes nothing.
func (s *API) signupVerifyPage(w http.ResponseWriter, r *http.Request) {
	_, email, _, _, _, orgName, err := s.loadSignup(r)
	if err != nil {
		s.Render(w, r, "error", http.StatusNotFound, web.Data{"Title": "Link expired", "Text": "This confirmation link was already used or has expired. Sign up again if needed."})
		return
	}
	s.Render(w, r, "signup_verify", http.StatusOK, web.Data{"Email": email, "OrgName": orgName, "Token": r.PathValue("token")})
}

func (s *API) signupVerifySubmit(w http.ResponseWriter, r *http.Request) {
	if !s.Limiter.Allow("signup-verify:"+web.ClientIP(r), 20, 15*time.Minute) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	signupID, email, name, hash, slug, orgName, err := s.loadSignup(r)
	if err != nil {
		s.signupVerifyPage(w, r)
		return
	}
	var userID, orgID string
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE signups SET used_at = now() WHERE id = $1 AND used_at IS NULL`, signupID)
		if err != nil || tag.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		if err := tx.QueryRow(r.Context(), `INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`,
			email, name, hash).Scan(&userID); err != nil {
			return err // includes "email already registered" (unique)
		}
		var taken bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM orgs WHERE slug = $1)`, slug).Scan(&taken); err != nil {
			return err
		}
		if taken { // claimed since the form was sent
			suffix := make([]byte, 2)
			_, _ = rand.Read(suffix)
			slug = strings.TrimRight(slug[:min(len(slug), 15)], "-") + "-" + hex.EncodeToString(suffix)
		}
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO orgs (slug, name, plan_id, trial_ends_at) VALUES ($1, $2, 'trial', now() + make_interval(days => $3))
			RETURNING id`, slug, orgName, s.Settings(r.Context()).SignupTrialDays).Scan(&orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, orgID, userID); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, orgID, userID, "org.signup", "org:"+slug,
			map[string]any{"plan": "trial", "trial_days": s.Settings(r.Context()).SignupTrialDays}, web.ClientIP(r))
	})
	switch {
	case web.IsUniqueViolation(err):
		web.Back(w, r, "/login", "", "This email address already has an account: log in instead.")
		return
	case web.IsNoRows(err):
		s.signupVerifyPage(w, r)
		return
	case err != nil:
		s.ServerError(w, r, err)
		return
	}
	if err := s.StartSession(w, r, userID, false); err != nil {
		s.SessionError(w, r, err)
		return
	}
	http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
}
