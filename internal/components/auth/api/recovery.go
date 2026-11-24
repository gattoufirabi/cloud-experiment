package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	platmail "platform/internal/lib/mail"
	"platform/internal/lib/secure"
)

// Account recovery without the operator:
//   - recovery codes: 10 one-time codes shown once after two-step setup (or when
//     regenerated); each replaces the 6-digit code for one login;
//   - password reset: a one-hour link sent by email (when SMTP is configured) or
//     printed by the operator (`platform admin reset-password`). Two-step login is
//     still required afterwards, so a stolen mailbox alone does not open the account.

const recoveryCodeCount = 10

// newRecoveryCodes replaces the user's codes and returns the new ones (shown once).
func newRecoveryCodes(ctx context.Context, q db.Execer, userID string) ([]string, error) {
	if _, err := q.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1`, userID); err != nil {
		return nil, err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	codes := make([]string, recoveryCodeCount)
	for i := range codes {
		b := make([]byte, 7)
		_, _ = rand.Read(b)
		c := strings.ToLower(enc.EncodeToString(b))[:10]
		codes[i] = c[:5] + "-" + c[5:]
		if _, err := q.Exec(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
			userID, secure.HashToken(normalizeRecovery(codes[i]))); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

func normalizeRecovery(code string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

// looksLikeRecoveryCode: anything that is not six digits is tried as a recovery code.
func looksLikeRecoveryCode(code string) bool {
	c := normalizeRecovery(code)
	return len(c) == 10 && strings.Trim(c, "0123456789") != ""
}

// useRecoveryCode burns a matching unused code; true if one was found.
func (s *API) useRecoveryCode(ctx context.Context, userID, code string) bool {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE recovery_codes SET used_at = now()
		WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, secure.HashToken(normalizeRecovery(code)))
	return err == nil && tag.RowsAffected() == 1
}

func (s *API) renderRecoveryCodes(w http.ResponseWriter, r *http.Request, codes []string, first bool) {
	s.Render(w, r, "recovery_codes", http.StatusOK, web.Data{"Codes": codes, "First": first})
}

// regenerateRecoveryCodes: Account page button (full session).
func (s *API) regenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	sess := web.SessionFrom(r)
	var codes []string
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		var err error
		if codes, err = newRecoveryCodes(r.Context(), tx, sess.UserID); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, "", sess.UserID, "totp.recovery_codes_created", "user:"+sess.UserID, nil, web.ClientIP(r))
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.renderRecoveryCodes(w, r, codes, false)
}

// ---------- password reset ----------

func (s *API) forgotPage(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, "password_forgot", http.StatusOK, web.Data{"EmailEnabled": platmail.Configured(s.Cfg)})
}

func (s *API) forgotSubmit(w http.ResponseWriter, r *http.Request) {
	ip := web.ClientIP(r)
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	done := func() {
		s.Render(w, r, "password_forgot", http.StatusOK, web.Data{"EmailEnabled": platmail.Configured(s.Cfg), "Sent": true, "Email": email})
	}
	if !platmail.Configured(s.Cfg) {
		s.forgotPage(w, r)
		return
	}
	if !s.Limiter.Allow("forgot-ip:"+ip, 10, time.Hour) || !s.Limiter.Allow("forgot-email:"+email, 3, time.Hour) {
		done() // same answer: do not reveal limits per address
		return
	}
	var userID string
	if err := s.Pool.QueryRow(r.Context(), `SELECT id FROM users WHERE email = $1`, email).Scan(&userID); err != nil {
		done() // unknown address: same answer, nothing sent
		return
	}
	link, err := s.CreatePasswordReset(r.Context(), userID, "email", ip)
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	go func() {
		if err := s.SendMail(r.Context(), email, "Reset your password",
			"Someone (hopefully you) asked to reset the password of your "+s.Cfg.ProductName+" account.\n\n"+
				"Choose a new password here (the link works once, within 1 hour):\n\n"+link+"\n\n"+
				"If this wasn't you, ignore this message: your password stays the same.\n"); err != nil {
			s.Log.Error("send reset email", "err", err)
		}
	}()
	done()
}

func (s *API) resetUser(r *http.Request) (string, string, error) {
	var userID, email string
	err := s.Pool.QueryRow(r.Context(), `
		SELECT pr.user_id, u.email FROM password_resets pr JOIN users u ON u.id = pr.user_id
		WHERE pr.token_hash = $1 AND pr.used_at IS NULL AND pr.expires_at > now()`, secure.HashToken(r.PathValue("token"))).
		Scan(&userID, &email)
	return userID, email, err
}

func (s *API) resetPage(w http.ResponseWriter, r *http.Request) {
	_, email, err := s.resetUser(r)
	if err != nil {
		s.Render(w, r, "error", http.StatusNotFound, web.Data{"Title": "Link expired", "Text": "This reset link was already used or has expired. Ask for a new one."})
		return
	}
	s.Render(w, r, "password_reset", http.StatusOK, web.Data{"Email": email, "Token": r.PathValue("token")})
}

func (s *API) resetSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.Limiter.Allow("reset:"+web.ClientIP(r), 20, 15*time.Minute) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	userID, email, err := s.resetUser(r)
	if err != nil {
		s.resetPage(w, r)
		return
	}
	pw := r.PostFormValue("password")
	again := func(msg string) {
		s.Render(w, r, "password_reset", http.StatusBadRequest, web.Data{"Email": email, "Token": r.PathValue("token"), "Err": msg})
	}
	switch {
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
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE password_resets SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL`,
			secure.HashToken(r.PathValue("token")))
		if err != nil || tag.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		if _, err := tx.Exec(r.Context(), `UPDATE users SET password_hash = $2, failed_logins = 0, locked_until = NULL WHERE id = $1`, userID, hash); err != nil {
			return err
		}
		// Every existing session ends: whoever knew the old password is logged out.
		if _, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, "", userID, "password.reset", "user:"+userID, nil, web.ClientIP(r))
	})
	if web.IsNoRows(err) {
		s.resetPage(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	go func() {
		if platmail.Configured(s.Cfg) {
			_ = s.SendMail(r.Context(), email, "Your password was changed",
				"The password of your "+s.Cfg.ProductName+" account was just changed. If this wasn't you, contact the platform operator now.\n")
		}
	}()
	web.Back(w, r, "/login", "Password changed. Log in with the new one (two-step login is still required).", "")
}
