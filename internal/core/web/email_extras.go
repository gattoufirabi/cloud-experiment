package web

import (
	"context"

	"platform/internal/lib/mail"
)

// sendMail sends one of the platform's own notices: at once through the relay, or queued
// for the agent, which delivers it directly (platform_mail).
func (s *Server) SendMail(ctx context.Context, to, subject, body string) error {
	if !mail.Configured(s.Cfg) {
		return mail.ErrNotConfigured
	}
	if mail.Relay(s.Cfg) {
		return mail.Send(s.Cfg, to, subject, body)
	}
	if _, _, _, err := mail.Plain(s.Cfg, to, subject, body); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO platform_mail (to_addr, subject, body) VALUES ($1, $2, $3)`, to, subject, body); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `SELECT pg_notify('deployments', 'email')`)
	return err
}
