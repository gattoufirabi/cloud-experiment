package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/secure"
)

const genericLoginError = "Invalid email or password, or the account is temporarily locked."

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if sess := s.loadSession(r); sess != nil && sess.TOTPOK {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", http.StatusOK, nil)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	pw := r.PostFormValue("password")
	fail := func(status int, msg string) {
		s.render(w, r, "login", status, data{"Err": msg, "Email": email})
	}
	if !s.limiter.allow("login-ip:"+ip, 20, 15*time.Minute) || !s.limiter.allow("login-email:"+email, 10, 15*time.Minute) {
		fail(http.StatusTooManyRequests, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	var id, hash string
	var locked *time.Time
	err := s.pool.QueryRow(r.Context(), `SELECT id, password_hash, locked_until FROM users WHERE email = $1`, email).Scan(&id, &hash, &locked)
	if isNoRows(err) {
		secure.VerifyPassword(pw, s.dummyHash) // same timing as a real user
		fail(http.StatusUnauthorized, genericLoginError)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if locked != nil && locked.After(time.Now()) {
		fail(http.StatusUnauthorized, genericLoginError)
		return
	}
	if !secure.VerifyPassword(pw, hash) {
		_, _ = s.pool.Exec(r.Context(), `
			UPDATE users SET failed_logins = failed_logins + 1,
			       locked_until = CASE WHEN failed_logins + 1 >= 5 THEN now() + interval '15 minutes' ELSE locked_until END
			WHERE id = $1`, id)
		_ = db.Audit(r.Context(), s.pool, "", id, "login.failed", "user:"+id, nil, ip)
		fail(http.StatusUnauthorized, genericLoginError)
		return
	}
	_, _ = s.pool.Exec(r.Context(), `UPDATE users SET failed_logins = 0, locked_until = NULL WHERE id = $1`, id)
	if err := s.startSession(w, r, id, false); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/totp/verify", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	_, _ = s.pool.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, sess.TokenHash)
	s.clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- two-step codes (mandatory for everyone) ----------

func (s *Server) totpSetupPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	if sess.TOTPEnabled {
		http.Redirect(w, r, "/totp/verify", http.StatusSeeOther)
		return
	}
	secret, err := s.pendingTOTPSecret(r, sess.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	uri := secure.TOTPURI("Platform", sess.Email, secret)
	qr, err := qrSVG(uri, "QR code: scan it with your authenticator app")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "totp_setup", http.StatusOK, data{"Secret": secret, "URI": uri, "QR": qr})
}

// pendingTOTPSecret returns the stored (not yet confirmed) secret, creating one if needed.
func (s *Server) pendingTOTPSecret(r *http.Request, userID string) (string, error) {
	var enc []byte
	if err := s.pool.QueryRow(r.Context(), `SELECT totp_secret_enc FROM users WHERE id = $1`, userID).Scan(&enc); err != nil {
		return "", err
	}
	if enc != nil {
		plain, err := s.box.Open(enc, "totp:"+userID)
		return string(plain), err
	}
	secret, err := secure.NewTOTPSecret()
	if err != nil {
		return "", err
	}
	enc, err = s.box.Seal([]byte(secret), "totp:"+userID)
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(r.Context(), `UPDATE users SET totp_secret_enc = $2 WHERE id = $1 AND NOT totp_enabled`, userID, enc)
	return secret, err
}

func (s *Server) totpSetupSubmit(w http.ResponseWriter, r *http.Request) {
	s.checkTOTP(w, r, true)
}

func (s *Server) totpVerifyPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	if !sess.TOTPEnabled {
		http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
		return
	}
	if sess.TOTPOK {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "totp_verify", http.StatusOK, nil)
}

func (s *Server) totpVerifySubmit(w http.ResponseWriter, r *http.Request) {
	s.checkTOTP(w, r, false)
}

func (s *Server) checkTOTP(w http.ResponseWriter, r *http.Request, enrolling bool) {
	sess := sessionFrom(r)
	page := "totp_verify"
	if enrolling {
		page = "totp_setup"
	}
	again := func(status int, msg string) {
		d := data{"Err": msg}
		if enrolling {
			if secret, err := s.pendingTOTPSecret(r, sess.UserID); err == nil {
				d["Secret"], d["URI"] = secret, secure.TOTPURI("Platform", sess.Email, secret)
			}
		}
		s.render(w, r, page, status, d)
	}
	if !s.limiter.allow("totp:"+sess.UserID, 10, 15*time.Minute) {
		again(http.StatusTooManyRequests, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	var enc []byte
	var enabled bool
	var lastStep int64
	err := s.pool.QueryRow(r.Context(), `SELECT totp_secret_enc, totp_enabled, totp_last_step FROM users WHERE id = $1`, sess.UserID).
		Scan(&enc, &enabled, &lastStep)
	if err != nil || enc == nil || enabled == enrolling {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	secret, err := s.box.Open(enc, "totp:"+sess.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	step, ok := secure.VerifyTOTP(string(secret), r.PostFormValue("code"), lastStep, time.Now())
	if !ok {
		_ = db.Audit(r.Context(), s.pool, "", sess.UserID, "totp.failed", "user:"+sess.UserID, nil, clientIP(r))
		again(http.StatusUnauthorized, "That code is not valid. Check the time on your phone and try the newest code.")
		return
	}
	// Rotate the session on privilege change: the old token dies, a new fully authenticated one is issued.
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `UPDATE users SET totp_enabled = true, totp_last_step = $2 WHERE id = $1`, sess.UserID, step); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, sess.TokenHash)
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.startSession(w, r, sess.UserID, true); err != nil {
		s.serverError(w, r, err)
		return
	}
	action := "login.success"
	if enrolling {
		action = "totp.enabled"
	}
	_ = db.Audit(r.Context(), s.pool, "", sess.UserID, action, "user:"+sess.UserID, nil, clientIP(r))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- invitations (the only way to get an account) ----------

type invite struct {
	ID, OrgID, OrgName, Email, Role string
	UserExists                      bool
}

func (s *Server) loadInvite(r *http.Request) (*invite, error) {
	var inv invite
	err := s.pool.QueryRow(r.Context(), `
		SELECT i.id, i.org_id, o.name, i.email, i.role, EXISTS (SELECT 1 FROM users u WHERE u.email = i.email)
		FROM invites i JOIN orgs o ON o.id = i.org_id
		WHERE i.token_hash = $1 AND i.accepted_at IS NULL AND i.expires_at > now()`,
		secure.HashToken(r.PathValue("token"))).Scan(&inv.ID, &inv.OrgID, &inv.OrgName, &inv.Email, &inv.Role, &inv.UserExists)
	return &inv, err
}

func (s *Server) invitePage(w http.ResponseWriter, r *http.Request) {
	inv, err := s.loadInvite(r)
	if err != nil {
		s.render(w, r, "error", http.StatusNotFound, data{"Title": "Invitation not valid", "Text": "This invitation link has expired, was already used, or does not exist. Ask for a new one."})
		return
	}
	s.render(w, r, "invite", http.StatusOK, data{"Invite": inv})
}

func (s *Server) inviteSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("invite:"+clientIP(r), 20, 15*time.Minute) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	inv, err := s.loadInvite(r)
	if err != nil {
		s.invitePage(w, r)
		return
	}
	again := func(msg string) {
		s.render(w, r, "invite", http.StatusBadRequest, data{"Invite": inv, "Err": msg})
	}
	pw := r.PostFormValue("password")
	var userID string
	if inv.UserExists {
		var hash string
		if err := s.pool.QueryRow(r.Context(), `SELECT id, password_hash FROM users WHERE email = $1`, inv.Email).Scan(&userID, &hash); err != nil {
			s.serverError(w, r, err)
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
			s.serverError(w, r, err)
			return
		}
		if err := s.pool.QueryRow(r.Context(), `INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`,
			inv.Email, name, hash).Scan(&userID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE invites SET accepted_at = now() WHERE id = $1 AND accepted_at IS NULL`, inv.ID)
		if err != nil || tag.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (org_id, user_id) DO NOTHING`, inv.OrgID, userID, inv.Role); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, inv.OrgID, userID, "member.joined", "user:"+userID, map[string]string{"role": inv.Role}, clientIP(r))
	})
	if err != nil {
		s.invitePage(w, r)
		return
	}
	if err := s.startSession(w, r, userID, false); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
