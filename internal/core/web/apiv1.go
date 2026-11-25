package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

// REST API v1, for CI pipelines and the platformctl tool. Authentication is a bearer
// API token ("pla_…") created by an organization admin. Every request is limited to the
// token's organization (database row-level security, as in the console) and to the
// token's role. Bearer tokens are never sent automatically by browsers, so no CSRF check
// is needed. Changes are audited under the token's creator, with the token named.

const APITokenPrefix = "pla_"

const APITokenKey ctxKey = 2

func APIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// apiAuth resolves the bearer token and enforces the minimum role.
type queueScopeKeyT struct{}

// apiQueueScopeKey holds the queue a token is limited to ("" for organization tokens).
var APIQueueScopeKey = queueScopeKeyT{}

func (s *Server) APIAuth(minRole string, h func(http.ResponseWriter, *http.Request, *OrgInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ip := ClientIP(r)
		if !s.Limiter.Allow("api:"+ip, 600, time.Minute) {
			APIError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || !strings.HasPrefix(tok, APITokenPrefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="platform"`)
			APIError(w, http.StatusUnauthorized, "missing or malformed API token (Authorization: Bearer pla_...)")
			return
		}
		var o OrgInfo
		var name, role string
		var createdBy, queueScope, projectScope *string
		var emailOnly bool
		err := s.Pool.QueryRow(r.Context(), `
			SELECT token_name, role, created_by::text, org_id, org_slug, org_name, plan_id, scan_policy, queue_id::text, email_only, project_slug FROM api_token_check($1)`,
			secure.HashToken(tok)).Scan(&name, &role, &createdBy, &o.ID, &o.Slug, &o.Name, &o.PlanID, &o.ScanPolicy, &queueScope, &emailOnly, &projectScope)
		if err != nil {
			if !s.Limiter.Allow("api-fail:"+ip, 30, 10*time.Minute) {
				APIError(w, http.StatusTooManyRequests, "too many failed attempts")
				return
			}
			APIError(w, http.StatusUnauthorized, "invalid, expired or revoked API token")
			return
		}
		o.Role = role
		if !model.RoleAtLeast(role, minRole) {
			APIError(w, http.StatusForbidden, "this token's role ("+role+") cannot do this; it needs "+minRole)
			return
		}
		s.loadFeaturesOff(r.Context(), &o)
		if area := apiArea(r.URL.Path); o.Off[area] {
			APIError(w, http.StatusForbidden, ProductByID(area).Name+" is turned off for this organization")
			return
		}
		actor := ""
		if createdBy != nil {
			actor = *createdBy
		}
		ctx := context.WithValue(r.Context(), SessKey, &Session{UserID: actor})
		ctx = context.WithValue(ctx, APITokenKey, name)
		if queueScope != nil { // a queue token only sends, receives and deletes messages of its queue
			p := r.URL.Path
			if !strings.HasPrefix(p, "/api/v1/queues/") || !(strings.HasSuffix(p, "/messages") || strings.HasSuffix(p, "/receive") || strings.Contains(p, "/messages/")) {
				APIError(w, http.StatusForbidden, "this token only works for its queue's messages")
				return
			}
			ctx = context.WithValue(ctx, APIQueueScopeKey, *queueScope)
		}
		if projectScope != nil { // a project token only reaches that project
			p := r.URL.Path
			if p != "/api/v1/me" && p != "/api/v1/projects/"+*projectScope && !strings.HasPrefix(p, "/api/v1/projects/"+*projectScope+"/") {
				APIError(w, http.StatusForbidden, "this token only works for project "+*projectScope)
				return
			}
			o.Scoped, o.Projects = true, map[string]string{*projectScope: role}
		}
		if emailOnly && !(r.Method == http.MethodPost && r.URL.Path == "/api/v1/email/send") &&
			!(r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/email/messages/")) {
			APIError(w, http.StatusForbidden, "this token only sends email")
			return
		}
		h(w, r.WithContext(ctx), &o)
	}
}

// apiService resolves {project} and {service} inside the token's organization.
func (s *Server) APIService(w http.ResponseWriter, r *http.Request, o *OrgInfo,
	fn func(tx pgx.Tx, p *ProjectInfo, sv *ServiceInfo) error) bool {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.LoadProject(r, tx)
		if err != nil {
			return err
		}
		sv, err := s.LoadService(r, tx, p.ID)
		if err != nil {
			return err
		}
		return fn(tx, p, sv)
	})
	var ae APIErr
	switch {
	case IsNoRows(err):
		APIError(w, http.StatusNotFound, "project or service not found")
	case errors.As(err, &ae):
		APIError(w, ae.Status, ae.Msg)
	case err != nil:
		s.Log.Error("api", "path", r.URL.Path, "err", err)
		APIError(w, http.StatusInternalServerError, "internal error")
	default:
		return true
	}
	return false
}

type APIErr struct {
	Status int
	Msg    string
}

func (e APIErr) Error() string { return e.Msg }
