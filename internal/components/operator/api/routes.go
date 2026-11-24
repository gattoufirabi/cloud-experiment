package api

import (
	"net/http"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.consoleRoutes(m)
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /operator", s.operatorOnly(s.operatorPage))
	m.HandleFunc("POST /operator/orgs/{slug}/plan", s.operatorOnly(s.operatorSetPlan))
	m.HandleFunc("POST /operator/orgs/{slug}/suspend", s.operatorOnly(s.operatorSuspend))
	m.HandleFunc("POST /operator/invoices/{number}", s.operatorOnly(s.operatorInvoice))
	m.HandleFunc("POST /operator/nodes/{id}", s.operatorOnly(s.operatorSetNode))
	m.HandleFunc("POST /operator/nodes/{id}/drain", s.operatorOnly(s.operatorDrainNode))
	m.HandleFunc("POST /operator/orgs/{slug}/cluster", s.operatorOnly(s.operatorOrgCluster))
	m.HandleFunc("GET /operator/orgs/{slug}", s.operatorOnly(s.operatorOrgPage))
	m.HandleFunc("GET /operator/settings", s.operatorOnly(s.operatorSettingsPage))
	m.HandleFunc("GET /operator/servers", s.operatorOnly(s.operatorServersPage))
	m.HandleFunc("GET /operator/mail", s.operatorOnly(s.operatorMailPage))
	m.HandleFunc("POST /operator/mail/unban", s.operatorOnly(s.operatorUnban))
	m.HandleFunc("GET /operator/dns", s.operatorOnly(s.operatorDNSPage))
	m.HandleFunc("POST /operator/servers/restart", s.operatorOnly(s.operatorRestart))
	m.HandleFunc("GET /operator/features", s.operatorOnly(s.OperatorFeaturesPage))
	m.HandleFunc("POST /operator/features", s.operatorOnly(s.OperatorSaveFeatures))
	m.HandleFunc("POST /operator/orgs/{slug}/features", s.operatorOnly(s.OperatorOrgFeatures))
	m.HandleFunc("POST /operator/settings", s.operatorOnly(s.operatorSaveSettings))
	m.HandleFunc("GET /operator/plans", s.operatorOnly(s.operatorPlansPage))
	m.HandleFunc("POST /operator/plans", s.operatorOnly(s.operatorCreatePlan))
	m.HandleFunc("POST /operator/plans/{id}", s.operatorOnly(s.operatorUpdatePlan))
	m.HandleFunc("POST /operator/plans/{id}/delete", s.operatorOnly(s.operatorDeletePlan))
	m.HandleFunc("GET /operator/resources", s.operatorOnly(s.operatorResourcesPage))
	m.HandleFunc("GET /operator/health", s.operatorOnly(s.operatorHealthPage))
	m.HandleFunc("POST /operator/invoices/generate", s.operatorOnly(s.operatorGenerateInvoices))
	m.HandleFunc("GET /operator/users", s.operatorOnly(s.operatorUsersPage))
	m.HandleFunc("POST /operator/users", s.operatorOnly(s.operatorCreateUser))
	m.HandleFunc("POST /operator/invites/{id}/revoke", s.operatorOnly(s.operatorRevokeInvite))
	m.HandleFunc("GET /operator/users/{id}", s.operatorOnly(s.operatorUserPage))
	m.HandleFunc("POST /operator/users/{id}/disable", s.operatorOnly(s.operatorUserDisable))
	m.HandleFunc("POST /operator/users/{id}/signout", s.operatorOnly(s.operatorUserSignout))
	m.HandleFunc("POST /operator/users/{id}/unlock", s.operatorOnly(s.operatorUserUnlock))
	m.HandleFunc("POST /operator/users/{id}/reset-password", s.operatorOnly(s.operatorUserResetPassword))
	m.HandleFunc("POST /operator/users/{id}/reset-2fa", s.operatorOnly(s.operatorUserReset2FA))
	m.HandleFunc("POST /operator/users/{id}/profile", s.operatorOnly(s.operatorUserProfile))
	m.HandleFunc("POST /operator/users/{id}/operator", s.operatorOnly(s.operatorUserOperator))
	m.HandleFunc("POST /operator/users/{id}/membership", s.operatorOnly(s.operatorUserMembership))
	m.HandleFunc("POST /operator/users/{id}/delete", s.operatorOnly(s.operatorUserDelete))
}
