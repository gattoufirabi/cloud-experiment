package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/secure"
)

const genericLoginError = "Invalid email or password, or the account is temporarily locked."

func (s *API) loginPage(w http.ResponseWriter, r *http.Request) {
	if sess := s.LoadSession(r); sess != nil && sess.TOTPOK {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.Render(w, r, "login", http.StatusOK, nil)
}

func (s *API) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := web.ClientIP(r)
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	pw := r.PostFormValue("password")
	fail := func(status int, msg string) {
		s.Render(w, r, "login", status, web.Data{"Err": msg, "Email": email})
	}
	if !s.Limiter.Allow("login-ip:"+ip, 20, 15*time.Minute) || !s.Limiter.Allow("login-email:"+email, 10, 15*time.Minute) {
		fail(http.StatusTooManyRequests, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	var id, hash string
	var locked *time.Time
	err := s.Pool.QueryRow(r.Context(), `SELECT id, password_hash, locked_until FROM users WHERE email = $1`, email).Scan(&id, &hash, &locked)
	if web.IsNoRows(err) {
		secure.VerifyPassword(pw, s.DummyHash) // same timing as a real user
		fail(http.StatusUnauthorized, genericLoginError)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if locked != nil && locked.After(time.Now()) {
		fail(http.StatusUnauthorized, genericLoginError)
		return
	}
	if !secure.VerifyPassword(pw, hash) {
		_, _ = s.Pool.Exec(r.Context(), `
			UPDATE users SET failed_logins = failed_logins + 1,
			       locked_until = CASE WHEN failed_logins + 1 >= 5 THEN now() + interval '15 minutes' ELSE locked_until END
			WHERE id = $1`, id)
		_ = db.Audit(r.Context(), s.Pool, "", id, "login.failed", "user:"+id, nil, ip)
		fail(http.StatusUnauthorized, genericLoginError)
		return
	}
	_, _ = s.Pool.Exec(r.Context(), `UPDATE users SET failed_logins = 0, locked_until = NULL WHERE id = $1`, id)
	if org := s.ssoRequired(r.Context(), id, email); org != "" {
		_ = db.Audit(r.Context(), s.Pool, "", id, "login.refused_sso_required", "user:"+id, nil, ip)
		s.Render(w, r, "login", http.StatusForbidden, web.Data{"SSOErr": org + " requires single sign-on: sign in with your work email below.", "Who": email})
		return
	}
	if err := s.StartSession(w, r, id, false); err != nil {
		s.SessionError(w, r, err)
		return
	}
	http.Redirect(w, r, "/totp/verify", http.StatusSeeOther)
}

func (s *API) logout(w http.ResponseWriter, r *http.Request) {
	sess := web.SessionFrom(r)
	_, _ = s.Pool.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, sess.TokenHash)
	s.ClearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- two-step codes (mandatory for everyone) ----------

func (s *API) totpSetupPage(w http.ResponseWriter, r *http.Request) {
	sess := web.SessionFrom(r)
	if sess.TOTPEnabled {
		http.Redirect(w, r, "/totp/verify", http.StatusSeeOther)
		return
	}
	secret, err := s.pendingTOTPSecret(r, sess.UserID)
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	uri := secure.TOTPURI(s.Cfg.ProductName, sess.Email, secret)
	qr, err := web.QrSVG(uri, "QR code: scan it with your authenticator app")
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "totp_setup", http.StatusOK, web.Data{"Secret": secret, "URI": uri, "QR": qr})
}

// pendingTOTPSecret returns the stored (not yet confirmed) secret, creating one if needed.
func (s *API) pendingTOTPSecret(r *http.Request, userID string) (string, error) {
	var enc []byte
	if err := s.Pool.QueryRow(r.Context(), `SELECT totp_secret_enc FROM users WHERE id = $1`, userID).Scan(&enc); err != nil {
		return "", err
	}
	if enc != nil {
		plain, err := s.Box.Open(enc, "totp:"+userID)
		return string(plain), err
	}
	secret, err := secure.NewTOTPSecret()
	if err != nil {
		return "", err
	}
	enc, err = s.Box.Seal([]byte(secret), "totp:"+userID)
	if err != nil {
		return "", err
	}
	_, err = s.Pool.Exec(r.Context(), `UPDATE users SET totp_secret_enc = $2 WHERE id = $1 AND NOT totp_enabled`, userID, enc)
	return secret, err
}

func (s *API) totpSetupSubmit(w http.ResponseWriter, r *http.Request) {
	s.checkTOTP(w, r, true)
}

func (s *API) totpVerifyPage(w http.ResponseWriter, r *http.Request) {
	sess := web.SessionFrom(r)
	if !sess.TOTPEnabled {
		http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
		return
	}
	if sess.TOTPOK {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.Render(w, r, "totp_verify", http.StatusOK, nil)
}

func (s *API) totpVerifySubmit(w http.ResponseWriter, r *http.Request) {
	s.checkTOTP(w, r, false)
}

func (s *API) checkTOTP(w http.ResponseWriter, r *http.Request, enrolling bool) {
	sess := web.SessionFrom(r)
	page := "totp_verify"
	if enrolling {
		page = "totp_setup"
	}
	again := func(status int, msg string) {
		d := web.Data{"Err": msg}
		if enrolling {
			if secret, err := s.pendingTOTPSecret(r, sess.UserID); err == nil {
				d["Secret"], d["URI"] = secret, secure.TOTPURI(s.Cfg.ProductName, sess.Email, secret)
			}
		}
		s.Render(w, r, page, status, d)
	}
	if !s.Limiter.Allow("totp:"+sess.UserID, 10, 15*time.Minute) {
		again(http.StatusTooManyRequests, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	var enc []byte
	var enabled bool
	var lastStep int64
	err := s.Pool.QueryRow(r.Context(), `SELECT totp_secret_enc, totp_enabled, totp_last_step FROM users WHERE id = $1`, sess.UserID).
		Scan(&enc, &enabled, &lastStep)
	if err != nil || enc == nil || enabled == enrolling {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	secret, err := s.Box.Open(enc, "totp:"+sess.UserID)
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	code := r.PostFormValue("code")
	usedRecovery := false
	step, ok := lastStep, false
	if !enrolling && looksLikeRecoveryCode(code) {
		ok = s.useRecoveryCode(r.Context(), sess.UserID, code)
		usedRecovery = ok
	} else {
		step, ok = secure.VerifyTOTP(string(secret), code, lastStep, time.Now())
	}
	if !ok {
		_ = db.Audit(r.Context(), s.Pool, "", sess.UserID, "totp.failed", "user:"+sess.UserID, nil, web.ClientIP(r))
		again(http.StatusUnauthorized, "That code is not valid. Check the time on your phone and try the newest code.")
		return
	}
	// Rotate the session on privilege change: the old token dies, a new fully authenticated one is issued.
	var codes []string
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `UPDATE users SET totp_enabled = true, totp_last_step = $2 WHERE id = $1`, sess.UserID, step); err != nil {
			return err
		}
		if enrolling { // recovery codes are created with two-step login and shown once
			var err error
			if codes, err = newRecoveryCodes(r.Context(), tx, sess.UserID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, sess.TokenHash)
		return err
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if err := s.StartSession(w, r, sess.UserID, true); err != nil {
		s.SessionError(w, r, err)
		return
	}
	action := "login.success"
	switch {
	case enrolling:
		action = "totp.enabled"
	case usedRecovery:
		action = "login.recovery_code"
	}
	_ = db.Audit(r.Context(), s.Pool, "", sess.UserID, action, "user:"+sess.UserID, nil, web.ClientIP(r))
	if enrolling {
		s.renderRecoveryCodes(w, r, codes, true)
		return
	}
	if usedRecovery {
		var left int
		_ = s.Pool.QueryRow(r.Context(), `SELECT count(*) FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL`, sess.UserID).Scan(&left)
		web.Back(w, r, "/account", "Logged in with a recovery code ("+web.Itoa(left)+" left). If you lost your phone, set up two-step login again from a new device soon.", "")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- invitations (the only way to get an account) ----------

type invite struct {
	ID, OrgID, OrgName, Email, Role string
	UserExists                      bool
}

func (s *API) loadInvite(r *http.Request) (*invite, error) {
	var inv invite
	err := s.Pool.QueryRow(r.Context(), `
		SELECT i.id, i.org_id, o.name, i.email, i.role, EXISTS (SELECT 1 FROM users u WHERE u.email = i.email)
		FROM invites i JOIN orgs o ON o.id = i.org_id
		WHERE i.token_hash = $1 AND i.accepted_at IS NULL AND i.expires_at > now()`,
		secure.HashToken(r.PathValue("token"))).Scan(&inv.ID, &inv.OrgID, &inv.OrgName, &inv.Email, &inv.Role, &inv.UserExists)
	return &inv, err
}

func (s *API) invitePage(w http.ResponseWriter, r *http.Request) {
	inv, err := s.loadInvite(r)
	if err != nil {
		s.Render(w, r, "error", http.StatusNotFound, web.Data{"Title": "Invitation not valid", "Text": "This invitation link has expired, was already used, or does not exist. Ask for a new one."})
		return
	}
	s.Render(w, r, "invite", http.StatusOK, web.Data{"Invite": inv})
}

func (s *API) inviteSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.Limiter.Allow("invite:"+web.ClientIP(r), 20, 15*time.Minute) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	inv, err := s.loadInvite(r)
	if err != nil {
		s.invitePage(w, r)
		return
	}
	again := func(msg string) {
		s.Render(w, r, "invite", http.StatusBadRequest, web.Data{"Invite": inv, "Err": msg})
	}
	pw := r.PostFormValue("password")
	var userID string
	if inv.UserExists {
		var hash string
		if err := s.Pool.QueryRow(r.Context(), `SELECT id, password_hash FROM users WHERE email = $1`, inv.Email).Scan(&userID, &hash); err != nil {
			s.ServerError(w, r, err)
			return
		}
		if !secure.VerifyPassword(pw, hash) {
			again("Wrong password for this account.")
			return
		}
	} else {
		name := strings.TrimSpace(r.PostFormValue("name"))
		switch {
		case name == "" || len(name) > 100:
			again("Please enter your name (up to 100 characters).")
			return
		case len(pw) < 12 || len(pw) > 256:
			again("The password must be between 12 and 256 characters.")
			return
		case pw != r.PostFormValue("password2"):
			again("The two passwords do not match.")
			return
		}
		hash, err := secure.HashPassword(pw)
		if err != nil {
			s.ServerError(w, r, err)
			return
		}
		if err := s.Pool.QueryRow(r.Context(), `INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`,
			inv.Email, name, hash).Scan(&userID); err != nil {
			s.ServerError(w, r, err)
			return
		}
	}
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE invites SET accepted_at = now() WHERE id = $1 AND accepted_at IS NULL`, inv.ID)
		if err != nil || tag.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (org_id, user_id) DO NOTHING`, inv.OrgID, userID, inv.Role); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, inv.OrgID, userID, "member.joined", "user:"+userID, map[string]string{"role": inv.Role}, web.ClientIP(r))
	})
	if err != nil {
		s.invitePage(w, r)
		return
	}
	if err := s.StartSession(w, r, userID, false); err != nil {
		s.SessionError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
