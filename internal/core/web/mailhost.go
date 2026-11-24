package web

import (
	"platform/internal/lib/stalwart"
)

func (s *Server) MailHostingEnabled() bool { return s.Cfg.MailHostingEnabled() }

func (s *Server) MailServer() *stalwart.Client {
	return stalwart.New(s.Cfg.StalwartURL, s.Cfg.StalwartAdmin)
}
