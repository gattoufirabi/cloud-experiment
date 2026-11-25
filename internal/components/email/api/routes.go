package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.apiConsoleRoutes(m)
	s.apiRoutes(m)
	s.consoleRoutes(m)
}

// apiConsoleRoutes: this component's part of the former apiConsoleRoutes table.
func (s *API) apiConsoleRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/v1/email/attach", s.APIAuth("viewer", s.APIConsole(s.emailAttach, nil)))
}

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	const md = "/api/v1/mail/domains/{domain}"
	m.HandleFunc("GET /api/v1/email/domains", s.APIAuth("viewer", s.apiEmailDomains))
	m.HandleFunc("GET /api/v1/mail/domains", s.APIAuth("viewer", s.apiMailDomains))
	m.HandleFunc("POST /api/v1/mail/domains", s.APIAuth("developer", s.apiAddMailDomain))
	m.HandleFunc("GET "+md, s.APIAuth("viewer", s.apiMailDomain))
	m.HandleFunc("POST "+md+"/activate", s.APIAuth("developer", s.apiActivateMailDomain))
	m.HandleFunc("DELETE "+md, s.APIAuth("developer", s.apiDeleteMailDomain))
	m.HandleFunc("PUT "+md+"/catch-all", s.APIAuth("developer", s.apiSetCatchAll))
	m.HandleFunc("PUT "+md+"/server-name", s.APIAuth("developer", s.apiSetServerName))
	m.HandleFunc("GET "+md+"/mailboxes", s.APIAuth("viewer", s.apiMailboxes))
	m.HandleFunc("POST "+md+"/mailboxes", s.APIAuth("developer", s.apiAddMailbox))
	m.HandleFunc("PATCH "+md+"/mailboxes/{mailbox}", s.APIAuth("developer", s.apiUpdateMailbox))
	m.HandleFunc("DELETE "+md+"/mailboxes/{mailbox}", s.APIAuth("developer", s.apiDeleteMailbox))
	m.HandleFunc("POST "+md+"/mailboxes/{mailbox}/password", s.APIAuth("developer", s.apiMailboxPassword))
	m.HandleFunc("POST "+md+"/mailboxes/{mailbox}/aliases", s.APIAuth("developer", s.apiAddMailAlias))
	m.HandleFunc("DELETE "+md+"/aliases/{alias}", s.APIAuth("developer", s.apiDeleteMailAlias))
	m.HandleFunc("POST /api/v1/email/domains", s.APIAuth("developer", s.apiAddEmailDomain))
	m.HandleFunc("DELETE /api/v1/email/domains/{domain}", s.APIAuth("developer", s.apiDeleteEmailDomain))
	m.HandleFunc("POST /api/v1/email/send", s.APIAuth("developer", s.apiSendEmail))
	m.HandleFunc("GET /api/v1/email/messages/{id}", s.APIAuth("viewer", s.apiEmailMessage))
	m.HandleFunc("GET /api/v1/email/suppressions", s.APIAuth("viewer", s.apiSuppressions))
	m.HandleFunc("POST /api/v1/email/suppressions", s.APIAuth("developer", s.apiAddSuppression))
	m.HandleFunc("DELETE /api/v1/email/suppressions/{address}", s.APIAuth("developer", s.apiDeleteSuppression))
	m.HandleFunc("GET /api/v1/email/webhook", s.APIAuth("viewer", s.apiEmailWebhook))
	m.HandleFunc("PUT /api/v1/email/webhook", s.APIAuth("developer", s.apiSetEmailWebhook))
	m.HandleFunc("DELETE /api/v1/email/webhook", s.APIAuth("developer", s.apiDeleteEmailWebhook))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /o/{org}/email/suppressions", s.Auth(web.AuthFull, s.emailSuppressionAction))
	m.HandleFunc("POST /o/{org}/email/webhook", s.Auth(web.AuthFull, s.emailWebhookAction))
	m.HandleFunc("GET /o/{org}/email", s.Auth(web.AuthFull, s.emailPage))
	m.HandleFunc("GET /o/{org}/mail", s.Auth(web.AuthFull, s.mailPage))
	m.HandleFunc("POST /o/{org}/mail", s.Auth(web.AuthFull, s.addMailDomain))
	m.HandleFunc("GET /o/{org}/mail/{domain}", s.Auth(web.AuthFull, s.mailDomainPage))
	m.HandleFunc("POST /o/{org}/mail/{domain}", s.Auth(web.AuthFull, s.mailAction))
	m.HandleFunc("POST /o/{org}/email/domains", s.Auth(web.AuthFull, s.addEmailDomain))
	m.HandleFunc("POST /o/{org}/email/send", s.Auth(web.AuthFull, s.sendEmailConsole))
	m.HandleFunc("POST /o/{org}/email/attach", s.Auth(web.AuthFull, s.emailAttach))
	m.HandleFunc("GET /o/{org}/email/d/{domain}", s.Auth(web.AuthFull, s.emailDomainPage))
	m.HandleFunc("POST /o/{org}/email/d/{domain}/check", s.Auth(web.AuthFull, s.checkEmailDomain))
	m.HandleFunc("POST /o/{org}/email/d/{domain}/delete", s.Auth(web.AuthFull, s.deleteEmailDomain))
}
