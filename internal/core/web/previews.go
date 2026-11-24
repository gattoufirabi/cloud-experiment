package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/lib/model"
	"platform/internal/lib/registry"
)

// Preview environments: a temporary copy of a service with another image, typically
// one per pull request, created from the console, the API or platformctl (CI).
// Creating a preview that already exists redeploys it with the new image and renews
// its expiry, so a CI job can simply call "create" on every push.

const (
	MaxPreviewsPerService = 5
	defaultPreviewHours   = 72
	maxPreviewHours       = 720
)

var previewNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

// previewHost: <name>.<service>-<project>-<org>.<apps domain> (each DNS label ≤ 63).
func (s *Server) PreviewHost(name, svc, proj, org string) string {
	return name + "." + model.DefaultHost(svc, proj, org, s.Cfg.AppsDomain)
}

type PreviewRequest struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Hours int    `json:"ttl_hours"`
	// deferred: create or renew the preview without deploying it; a pull request build
	// deploys it once its image is ready.
	Deferred bool `json:"-" db:"-"`
}

type PreviewResult struct {
	Name, URL, Slug string
	Revision        int
	Expires         time.Time
	Created         bool
}

// upsertPreview creates the preview or, if it exists, redeploys it and renews its expiry.
// Errors meant for the user are apiErr values.
func (s *Server) UpsertPreview(r *http.Request, tx pgx.Tx, o *OrgInfo, p *ProjectInfo, parent *ServiceInfo, req PreviewRequest) (PreviewResult, error) {
	var res PreviewResult
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if !previewNameRe.MatchString(req.Name) {
		return res, APIErr{http.StatusUnprocessableEntity, "Preview name: 1 to 20 characters, lowercase letters, digits and dashes (for example pr-42)."}
	}
	if req.Hours == 0 {
		req.Hours = defaultPreviewHours
	}
	if req.Hours < 1 || req.Hours > maxPreviewHours {
		return res, APIErr{http.StatusUnprocessableEntity, "Lifetime: 1 to 720 hours."}
	}
	if parent.Image == "" && req.Image == "" && !req.Deferred {
		return res, APIErr{http.StatusUnprocessableEntity, "Give an image: the service has none yet."}
	}
	spec := parent.Spec()
	spec.Ports = nil     // public ports belong to the service itself
	spec.Instances = 1   // a preview runs a single instance
	spec.VolumePath = "" // previews never get persistent storage
	if img := strings.TrimSpace(req.Image); img != "" {
		spec.Image = img
	}
	if !req.Deferred { // a deferred preview gets its image from the build
		if err := spec.Validate(); err != nil {
			return res, APIErr{http.StatusUnprocessableEntity, err.Error()}
		}
		if err := registry.CheckNamespace(spec.Image, s.Cfg.RegistryHost, o.Slug); err != nil {
			return res, APIErr{http.StatusUnprocessableEntity, err.Error()}
		}
	}
	expires := time.Now().Add(time.Duration(req.Hours) * time.Hour)

	var id, slug string
	err := tx.QueryRow(r.Context(), `SELECT id, slug FROM services WHERE preview_of = $1 AND preview_name = $2`, parent.ID, req.Name).Scan(&id, &slug)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM services WHERE preview_of = $1`, parent.ID).Scan(&n); err != nil {
			return res, err
		}
		if n >= MaxPreviewsPerService {
			return res, APIErr{http.StatusConflict, "At most " + strconv.Itoa(MaxPreviewsPerService) + " previews per service: delete one first."}
		}
		if err := s.CheckQuota(r, tx, o, "", spec.CPUMillis, spec.MemoryMB, false); err != nil {
			return res, APIErr{http.StatusConflict, err.Error()}
		}
		b := make([]byte, 5)
		_, _ = rand.Read(b)
		slug = "pv-" + hex.EncodeToString(b)
		envJSON, _ := json.Marshal(spec.Env)
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO services (org_id, project_id, slug, image, port, cpu_millis, memory_mb, health_path, volume_path, env,
			                      waf_enabled, rate_limit_rpm, max_inflight, blocked_ips, preview_of, preview_name, expires_at, worker)
			SELECT $1, $2, $3, $4, port, cpu_millis, memory_mb, health_path, '', $5,
			       waf_enabled, rate_limit_rpm, max_inflight, blocked_ips, id, $6, $7, worker
			FROM services WHERE id = $8
			RETURNING id`, o.ID, p.ID, slug, spec.Image, envJSON, req.Name, expires, parent.ID).Scan(&id); err != nil {
			return res, err
		}
		// Secrets are re-encrypted for the preview: each ciphertext is bound to its row.
		rows, err := tx.Query(r.Context(), `SELECT name, value_enc FROM secrets WHERE service_id = $1`, parent.ID)
		if err != nil {
			return res, err
		}
		type sec struct {
			name string
			enc  []byte
		}
		var secs []sec
		for rows.Next() {
			var x sec
			if err := rows.Scan(&x.name, &x.enc); err != nil {
				rows.Close()
				return res, err
			}
			secs = append(secs, x)
		}
		rows.Close()
		for _, x := range secs {
			plain, err := s.Box.Open(x.enc, "secret:"+parent.ID+":"+x.name)
			if err != nil {
				return res, err
			}
			enc, err := s.Box.Seal(plain, "secret:"+id+":"+x.name)
			if err != nil {
				return res, err
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO secrets (org_id, service_id, name, value_enc) VALUES ($1, $2, $3, $4)`,
				o.ID, id, x.name, enc); err != nil {
				return res, err
			}
		}
		res.Created = true
	case err != nil:
		return res, err
	case req.Deferred: // keep serving the current image until the build deploys the new one
		if _, err := tx.Exec(r.Context(), `UPDATE services SET expires_at = $2 WHERE id = $1`, id, expires); err != nil {
			return res, err
		}
	default:
		if _, err := tx.Exec(r.Context(), `UPDATE services SET image = $2, expires_at = $3 WHERE id = $1`, id, spec.Image, expires); err != nil {
			return res, err
		}
	}
	rev := 0
	if !req.Deferred {
		if rev, err = s.CreateDeployment(r, tx, o, id, spec); err != nil {
			return res, err
		}
	}
	action := "preview.updated"
	if res.Created {
		action = "preview.created"
	}
	s.Audit(r, tx, o.ID, action, "service:"+p.Slug+"/"+parent.Slug, map[string]any{"preview": req.Name, "image": spec.Image, "ttl_hours": req.Hours})
	res.Name, res.Slug, res.Revision, res.Expires = req.Name, slug, rev, expires
	res.URL = s.AppURL(s.PreviewHost(req.Name, parent.Slug, p.Slug, o.Slug))
	return res, nil
}

func (s *Server) DeletePreviewTx(r *http.Request, tx pgx.Tx, o *OrgInfo, p *ProjectInfo, parent *ServiceInfo, name string) error {
	tag, err := tx.Exec(r.Context(), `DELETE FROM services WHERE preview_of = $1 AND preview_name = $2`, parent.ID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	s.Audit(r, tx, o.ID, "preview.deleted", "service:"+p.Slug+"/"+parent.Slug, map[string]any{"preview": name})
	_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}
