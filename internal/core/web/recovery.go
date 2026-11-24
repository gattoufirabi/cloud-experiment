package web

import (
	"context"

	"github.com/jackc/pgx/v5"

	"platform/internal/lib/db"
	"platform/internal/lib/secure"
)

// createPasswordReset stores a one-hour reset token and returns its link.
func (s *Server) CreatePasswordReset(ctx context.Context, userID, via, ip string) (string, error) {
	token := secure.NewToken()
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM password_resets WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO password_resets (token_hash, user_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')`,
			secure.HashToken(token), userID); err != nil {
			return err
		}
		return db.Audit(ctx, tx, "", userID, "password.reset_requested", "user:"+userID, map[string]string{"via": via}, ip)
	})
	return s.Cfg.BaseURL + "/password/reset/" + token, err
}
