package api

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/registry"
	"platform/internal/lib/secure"
)

// registryToken implements the Docker registry token endpoint. "docker login" and every
// push/pull send HTTP Basic credentials here; the answer is a short-lived signed token
// whose access list never leaves the namespace of the credential's organization.
func (s *API) registryToken(w http.ResponseWriter, r *http.Request) {
	if s.RegKey == nil {
		http.NotFound(w, r)
		return
	}
	deny := func() {
		w.Header().Set("WWW-Authenticate", `Basic realm="platform registry"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	if svc := r.URL.Query().Get("service"); svc != "" && svc != registry.Service {
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}
	ip := web.ClientIP(r)
	if !s.Limiter.Allow("registry-token:"+ip, 300, time.Minute) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || pass == "" {
		deny()
		return
	}

	var slug, subject, onlyRepo string
	canPush, canDelete := false, false
	if strings.HasPrefix(user, registry.AgentUserPrefix) {
		// Platform-side pull (agent, scanner): org-scoped, pull only.
		slug = strings.TrimPrefix(user, registry.AgentUserPrefix)
		want := registry.InternalPassword(s.Cfg.RegistryInternalSecret, slug)
		if s.Cfg.RegistryInternalSecret == "" || subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 {
			deny()
			return
		}
		subject = user
	} else if strings.HasPrefix(user, registry.BuilderUserPrefix) {
		// Agent pushing an image it built: org-scoped, push allowed.
		slug = strings.TrimPrefix(user, registry.BuilderUserPrefix)
		want := registry.BuilderPassword(s.Cfg.RegistryInternalSecret, slug)
		if s.Cfg.RegistryInternalSecret == "" || subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 {
			deny()
			return
		}
		subject, canPush, canDelete = user, true, true // the agent's daily cleanup deletes old tags
	} else if user == registry.BuildServerUser {
		// A build server pushing the image of the build it runs: that repository only.
		err := s.Pool.QueryRow(r.Context(), `SELECT org_slug, repository FROM build_push_check($1)`,
			secure.HashToken(pass)).Scan(&slug, &onlyRepo)
		if err != nil {
			if !s.Limiter.Allow("registry-fail:"+ip, 20, 10*time.Minute) {
				http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
				return
			}
			deny()
			return
		}
		subject, canPush = "build:"+onlyRepo, true
	} else {
		var orgID string
		err := s.Pool.QueryRow(r.Context(), `SELECT org_id, org_slug, can_push FROM registry_token_check($1)`,
			secure.HashToken(pass)).Scan(&orgID, &slug, &canPush)
		if err != nil {
			if !s.Limiter.Allow("registry-fail:"+ip, 20, 10*time.Minute) {
				http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
				return
			}
			deny()
			return
		}
		subject = "org:" + slug
	}

	access := []registry.Access{}
	for _, raw := range r.URL.Query()["scope"] {
		for _, sc := range strings.Fields(raw) {
			typ, name, actions, ok := registry.ParseScope(sc)
			if !ok || typ != "repository" || !strings.HasPrefix(name, slug+"/") || (onlyRepo != "" && name != onlyRepo) {
				continue // other namespaces, catalog listing, etc. are never granted
			}
			var granted []string
			for _, a := range actions {
				if a == "pull" || (a == "push" && canPush) || (a == "delete" && canDelete) {
					granted = append(granted, a)
				}
			}
			if len(granted) > 0 {
				access = append(access, registry.Access{Type: "repository", Name: name, Actions: granted})
			}
		}
	}
	tok, err := registry.Sign(s.RegKey, subject, access, 5*time.Minute)
	if err != nil {
		s.Log.Error("sign registry token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	web.WriteJSON(w, map[string]any{
		"token": tok, "access_token": tok, "expires_in": 300,
		"issued_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// registryEvents receives the registry's push notifications (internal listener only)
// and queues a vulnerability scan for every pushed tag.
func (s *API) registryEvents(w http.ResponseWriter, r *http.Request) {
	want := "Bearer " + s.Cfg.RegistryEventsSecret
	if s.Cfg.RegistryEventsSecret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var env struct {
		Events []struct {
			Action string `json:"action"`
			Target struct {
				MediaType  string `json:"mediaType"`
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
			} `json:"target"`
		} `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&env); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	for _, e := range env.Events {
		if e.Action != "push" || e.Target.Tag == "" || !strings.Contains(e.Target.MediaType, "manifest") {
			continue
		}
		slug, _, found := strings.Cut(e.Target.Repository, "/")
		if !found {
			continue
		}
		image := s.Cfg.RegistryHost + "/" + e.Target.Repository + ":" + e.Target.Tag
		if _, err := s.Pool.Exec(r.Context(), `SELECT scan_enqueue_push($1, $2)`, slug, image); err != nil {
			s.Log.Error("queue scan", "image", image, "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---------- console: registry tokens ----------

func (s *API) createRegistryToken(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	push := r.PostFormValue("push") == "1"
	if name == "" || len(name) > 60 {
		web.Back(w, r, o.Path()+"/registry#tokens", "", "Give the token a name of up to 60 characters (for example: ci-pipeline).")
		return
	}
	token := web.RegistryTokenPrefix + secure.NewToken()
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO registry_tokens (org_id, name, token_hash, can_push, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + interval '1 year')`,
			o.ID, name, secure.HashToken(token), push, web.SessionFrom(r).UserID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "registry_token.created", "token:"+name, map[string]bool{"push": push})
		return nil
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.RenderOrg(w, r, o, http.StatusOK, web.Data{"RegistryToken": token, "RegistryTokenName": name, "Page": "registry"})
}

func (s *API) deleteRegistryToken(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `DELETE FROM registry_tokens WHERE id = $1 RETURNING name`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "registry_token.revoked", "token:"+name, nil)
		return nil
	})
	if err != nil && !web.IsNoRows(err) {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/registry#tokens", "Token revoked. It stops working immediately.", "")
}
