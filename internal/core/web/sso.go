package web

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

// Single sign-on per organization with OpenID Connect. An admin sets the provider
// (issuer, client ID and secret), the email domains that belong to the organization
// and how provider groups map to roles. Members then sign in from the login page with
// their work email; accounts are created on first sign-in. With "enforce", password
// sign-in is refused for emails of those domains (owners keep it, to recover from a
// broken provider). The callback address to register at the provider is /sso/callback.

type SSOConfig struct {
	Enabled, Enforce, HasSecret                         bool
	Issuer, ClientID, DefaultRole, GroupsClaim          string
	Domains, AdminGroups, DeveloperGroups, ViewerGroups []string
	SecretEnc                                           []byte
	Updated                                             *time.Time
}

func (c *SSOConfig) DomainsText() string   { return strings.Join(c.Domains, ", ") }
func (c *SSOConfig) AdminText() string     { return strings.Join(c.AdminGroups, ", ") }
func (c *SSOConfig) DeveloperText() string { return strings.Join(c.DeveloperGroups, ", ") }
func (c *SSOConfig) ViewerText() string    { return strings.Join(c.ViewerGroups, ", ") }

func (s *Server) LoadSSO(ctx context.Context, orgID string) (*SSOConfig, error) {
	var c SSOConfig
	err := s.Pool.QueryRow(ctx, `
		SELECT enabled, enforce, client_secret_enc IS NOT NULL, issuer, client_id, default_role, groups_claim,
		       domains, admin_groups, developer_groups, viewer_groups, coalesce(client_secret_enc, '\x'::bytea), updated_at
		FROM org_sso WHERE org_id = $1`, orgID).
		Scan(&c.Enabled, &c.Enforce, &c.HasSecret, &c.Issuer, &c.ClientID, &c.DefaultRole, &c.GroupsClaim,
			&c.Domains, &c.AdminGroups, &c.DeveloperGroups, &c.ViewerGroups, &c.SecretEnc, &c.Updated)
	if IsNoRows(err) {
		return nil, nil
	}
	return &c, err
}

func SSOCallback(s *Server) string { return s.Cfg.BaseURL + "/sso/callback" }

var ErrRollback = errors.New("rollback")

// roleFor maps the provider's groups to a role ("" when no group list matches).
func (c *SSOConfig) RoleFor(groups []string) string {
	has := func(list []string) bool {
		for _, g := range groups {
			if slices.Contains(list, g) {
				return true
			}
		}
		return false
	}
	switch {
	case has(c.AdminGroups):
		return "admin"
	case has(c.DeveloperGroups):
		return "developer"
	case has(c.ViewerGroups):
		return "viewer"
	}
	return ""
}

func (c *SSOConfig) MapsGroups() bool {
	return len(c.AdminGroups)+len(c.DeveloperGroups)+len(c.ViewerGroups) > 0
}

func TruncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
