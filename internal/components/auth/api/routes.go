package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.passkeyRoutes(m)
	s.consoleRoutes(m)
}

// passkeyRoutes: this component's part of the former passkeyRoutes table.
func (s *API) passkeyRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /account", s.Auth(web.AuthFull, s.accountPage))
	m.HandleFunc("POST /account/recovery-codes", s.Auth(web.AuthFull, s.regenerateRecoveryCodes))
	m.HandleFunc("POST /account/passkeys/begin", s.Auth(web.AuthFull, s.passkeyRegisterBegin))
	m.HandleFunc("POST /account/passkeys/finish", s.Auth(web.AuthFull, s.passkeyRegisterFinish))
	m.HandleFunc("POST /account/passkeys/{id}/delete", s.Auth(web.AuthFull, s.passkeyDelete))
	m.HandleFunc("POST /login/passkey/begin", s.passkeyLoginBegin)
	m.HandleFunc("POST /login/passkey/finish", s.passkeyLoginFinish)
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /signup", s.signupPage)
	m.HandleFunc("POST /signup", s.signupSubmit)
	m.HandleFunc("GET /signup/verify/{token}", s.signupVerifyPage)
	m.HandleFunc("POST /signup/verify/{token}", s.signupVerifySubmit)
	m.HandleFunc("GET /password/forgot", s.forgotPage)
	m.HandleFunc("POST /password/forgot", s.forgotSubmit)
	m.HandleFunc("GET /password/reset/{token}", s.resetPage)
	m.HandleFunc("POST /password/reset/{token}", s.resetSubmit)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.loginSubmit)
	m.HandleFunc("POST /sso/start", s.ssoStart)
	m.HandleFunc("GET /sso/callback", s.ssoCallback)
	m.HandleFunc("POST /logout", s.Auth(web.AuthAny, s.logout))
	m.HandleFunc("GET /totp/setup", s.Auth(web.AuthPassword, s.totpSetupPage))
	m.HandleFunc("POST /totp/setup", s.Auth(web.AuthPassword, s.totpSetupSubmit))
	m.HandleFunc("GET /totp/verify", s.Auth(web.AuthPassword, s.totpVerifyPage))
	m.HandleFunc("POST /totp/verify", s.Auth(web.AuthPassword, s.totpVerifySubmit))
	m.HandleFunc("GET /invite/{token}", s.invitePage)
	m.HandleFunc("POST /invite/{token}", s.inviteSubmit)
	m.HandleFunc("POST /o/{org}/sso", s.Auth(web.AuthFull, s.saveSSO))
}
