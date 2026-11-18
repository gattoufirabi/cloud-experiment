package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/bits"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	platmail "platform/internal/mail"
	"platform/internal/model"
	"platform/internal/secure"
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

// ---------- proof of work ----------

type powStore struct {
	key  []byte
	mu   sync.Mutex
	used map[string]time.Time
}

func newPowStore() *powStore {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &powStore{key: k, used: map[string]time.Time{}}
}

func (p *powStore) sign(s string) string {
	m := hmac.New(sha256.New, p.key)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

// challenge: "<expires>.<salt>.<bits>.<hmac>", valid 10 minutes.
func (p *powStore) challenge(bitsNeeded int) string {
	salt := make([]byte, 12)
	_, _ = rand.Read(salt)
	body := fmt.Sprintf("%d.%s.%d", time.Now().Add(10*time.Minute).Unix(), hex.EncodeToString(salt), bitsNeeded)
	return body + "." + p.sign(body)
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(x)
	}
	return n
}

// verify checks the signature, expiry, difficulty and the work, and burns the challenge.
func (p *powStore) verify(ch, nonce string, minBits int) bool {
	parts := strings.Split(ch, ".")
	if len(parts) != 4 || len(nonce) == 0 || len(nonce) > 32 {
		return false
	}
	body := strings.Join(parts[:3], ".")
	if !hmac.Equal([]byte(p.sign(body)), []byte(parts[3])) {
		return false
	}
	exp, err1 := strconv.ParseInt(parts[0], 10, 64)
	need, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || time.Now().Unix() > exp || need < minBits {
		return false
	}
	sum := sha256.Sum256([]byte(ch + ":" + nonce))
	if leadingZeroBits(sum[:]) < need {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, t := range p.used {
		if now.After(t) {
			delete(p.used, k)
		}
	}
	if _, seen := p.used[ch]; seen {
		return false
	}
	p.used[ch] = time.Unix(exp, 0)
	return true
}

// ---------- pages ----------

func (s *Server) signupPage(w http.ResponseWriter, r *http.Request) {
	if !s.settings(r.Context()).SignupEnabled {
		s.notFound(w, r)
		return
	}
	s.renderSignup(w, r, http.StatusOK, data{})
}

func (s *Server) renderSignup(w http.ResponseWriter, r *http.Request, status int, d data) {
	d["Challenge"] = s.pow.challenge(s.cfg.SignupPowBits)
	d["TrialDays"] = s.settings(r.Context()).SignupTrialDays
	s.render(w, r, "signup", status, d)
}

func emailDomain(email string) string { return email[strings.LastIndex(email, "@")+1:] }

func (s *Server) signupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.settings(r.Context()).SignupEnabled {
		s.notFound(w, r)
		return
	}
	ip := clientIP(r)
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	name := strings.TrimSpace(r.PostFormValue("name"))
	orgName := strings.TrimSpace(r.PostFormValue("org_name"))
	slug := strings.ToLower(strings.TrimSpace(r.PostFormValue("org_slug")))
	pw := r.PostFormValue("password")
	form := data{"Email": email, "Name": name, "OrgName": orgName, "OrgSlug": slug}
	again := func(msg string) {
		form["Err"] = msg
		s.renderSignup(w, r, http.StatusBadRequest, form)
	}
	sent := func() { s.render(w, r, "signup_sent", http.StatusOK, data{"Email": email}) }

	if r.PostFormValue("website") != "" { // honeypot: bots fill every field
		sent()
		return
	}
	if !s.limiter.allow("signup:"+ip, s.cfg.SignupsPerHour, time.Hour) {
		again("Too many sign-ups from your network; try again in an hour.")
		return
	}
	if !s.pow.verify(r.PostFormValue("challenge"), r.PostFormValue("nonce"), s.cfg.SignupPowBits) {
		again("The browser check did not complete (JavaScript is needed). Please submit the form again.")
		return
	}
	addr, err := mail.ParseAddress(email)
	switch {
	case err != nil || addr.Address != email || len(email) > 200 || !strings.Contains(emailDomain(email), "."):
		again("Please enter a valid email address.")
		return
	case disposableDomains[emailDomain(email)] || contains(s.settings(r.Context()).SignupBlocked, emailDomain(email)):
		again("Please use a permanent email address.")
		return
	case len(s.settings(r.Context()).SignupAllowed) > 0 && !contains(s.settings(r.Context()).SignupAllowed, emailDomain(email)):
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
	if err := s.pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM orgs WHERE slug = $1)`, slug).Scan(&taken); err != nil {
		s.serverError(w, r, err)
		return
	}
	if taken {
		again("This organization ID is taken; choose another.")
		return
	}
	var today int
	if err := s.pool.QueryRow(r.Context(), `SELECT count(*) FROM signups WHERE created_at > now() - interval '1 day'`).Scan(&today); err != nil {
		s.serverError(w, r, err)
		return
	}
	if today >= s.settings(r.Context()).SignupDailyLimit {
		again("Sign-ups are paused for today; please try again tomorrow.")
		return
	}
	if !platmail.Configured(s.cfg) {
		again("Sign-up is not available right now (email is not configured).")
		return
	}
	var exists bool
	_ = s.pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists)
	if exists {
		// Same answer on screen; the owner of the address learns it already has an account.
		go func() {
			_ = s.sendMail(r.Context(), email, "Your Platform account",
				"Someone (maybe you) tried to sign up with this address, but it already has an account.\n\n"+
					"Log in at "+s.cfg.BaseURL+"/login\n\nIf this wasn't you, you can ignore this message.\n")
		}()
		sent()
		return
	}
	hash, err := secure.HashPassword(pw)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	token := secure.NewToken()
	var id string
	if err := s.pool.QueryRow(r.Context(), `
		INSERT INTO signups (email, name, password_hash, org_slug, org_name, token_hash, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + interval '24 hours') RETURNING id`,
		email, name, hash, slug, orgName, secure.HashToken(token), ip).Scan(&id); err != nil {
		s.serverError(w, r, err)
		return
	}
	body := "Hello " + name + ",\n\nConfirm your email address to create the organization \"" + orgName + "\":\n\n" +
		s.cfg.BaseURL + "/signup/verify/" + token + "\n\nThe link works once, within 24 hours. " +
		"If you did not sign up, ignore this message; nothing will be created.\n"
	if err := s.sendMail(r.Context(), email, "Confirm your email address", body); err != nil {
		s.log.Error("send signup email", "err", err)
		_, _ = s.pool.Exec(r.Context(), `DELETE FROM signups WHERE id = $1`, id)
		again("We could not send the confirmation email; please try again later.")
		return
	}
	sent()
}

func (s *Server) loadSignup(r *http.Request) (id, email, name, hash, slug, orgName string, err error) {
	err = s.pool.QueryRow(r.Context(), `
		SELECT id, email, name, password_hash, org_slug, org_name FROM signups
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`, secure.HashToken(r.PathValue("token"))).
		Scan(&id, &email, &name, &hash, &slug, &orgName)
	return
}

// signupVerifyPage shows a confirm button: opening the link alone changes nothing.
func (s *Server) signupVerifyPage(w http.ResponseWriter, r *http.Request) {
	_, email, _, _, _, orgName, err := s.loadSignup(r)
	if err != nil {
		s.render(w, r, "error", http.StatusNotFound, data{"Title": "Link expired", "Text": "This confirmation link was already used or has expired. Sign up again if needed."})
		return
	}
	s.render(w, r, "signup_verify", http.StatusOK, data{"Email": email, "OrgName": orgName, "Token": r.PathValue("token")})
}

func (s *Server) signupVerifySubmit(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("signup-verify:"+clientIP(r), 20, 15*time.Minute) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	signupID, email, name, hash, slug, orgName, err := s.loadSignup(r)
	if err != nil {
		s.signupVerifyPage(w, r)
		return
	}
	var userID, orgID string
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
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
			RETURNING id`, slug, orgName, s.settings(r.Context()).SignupTrialDays).Scan(&orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, orgID, userID); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, orgID, userID, "org.signup", "org:"+slug,
			map[string]any{"plan": "trial", "trial_days": s.settings(r.Context()).SignupTrialDays}, clientIP(r))
	})
	switch {
	case isUniqueViolation(err):
		back(w, r, "/login", "", "This email address already has an account: log in instead.")
		return
	case isNoRows(err):
		s.signupVerifyPage(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if err := s.startSession(w, r, userID, false); err != nil {
		s.sessionError(w, r, err)
		return
	}
	http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
