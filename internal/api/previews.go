package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/model"
	"platform/internal/registry"
)

// Preview environments: a temporary copy of a service with another image, typically
// one per pull request, created from the console, the API or platformctl (CI).
// Creating a preview that already exists redeploys it with the new image and renews
// its expiry, so a CI job can simply call "create" on every push.

const (
	maxPreviewsPerService = 5
	defaultPreviewHours   = 72
	maxPreviewHours       = 720
)

var previewNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

type previewView struct {
	Name, Slug, Image, Status, URL string
	Expires                        time.Time
	Created                        time.Time
}

// previewHost: <name>.<service>-<project>-<org>.<apps domain> (each DNS label ≤ 63).
func (s *Server) previewHost(name, svc, proj, org string) string {
	return name + "." + model.DefaultHost(svc, proj, org, s.cfg.AppsDomain)
}

func (s *Server) loadPreviews(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, parent *serviceInfo) ([]previewView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT s.preview_name, s.slug, s.image, coalesce(d.status, 'pending'), s.expires_at, s.created_at
		FROM services s
		LEFT JOIN LATERAL (SELECT status FROM deployments WHERE service_id = s.id ORDER BY revision DESC LIMIT 1) d ON true
		WHERE s.preview_of = $1 ORDER BY s.preview_name`, parent.ID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (previewView, error) {
		var v previewView
		err := row.Scan(&v.Name, &v.Slug, &v.Image, &v.Status, &v.Expires, &v.Created)
		v.URL = s.appURL(s.previewHost(v.Name, parent.Slug, p.Slug, o.Slug))
		return v, err
	})
	return out, err
}

type previewRequest struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Hours int    `json:"ttl_hours"`
	// deferred: create or renew the preview without deploying it; a pull request build
	// deploys it once its image is ready.
	deferred bool
}

type previewResult struct {
	Name, URL, Slug string
	Revision        int
	Expires         time.Time
	Created         bool
}

// upsertPreview creates the preview or, if it exists, redeploys it and renews its expiry.
// Errors meant for the user are apiErr values.
func (s *Server) upsertPreview(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, parent *serviceInfo, req previewRequest) (previewResult, error) {
	var res previewResult
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if !previewNameRe.MatchString(req.Name) {
		return res, apiErr{http.StatusUnprocessableEntity, "Preview name: 1 to 20 characters, lowercase letters, digits and dashes (for example pr-42)."}
	}
	if req.Hours == 0 {
		req.Hours = defaultPreviewHours
	}
	if req.Hours < 1 || req.Hours > maxPreviewHours {
		return res, apiErr{http.StatusUnprocessableEntity, "Lifetime: 1 to 720 hours."}
	}
	if parent.Image == "" && req.Image == "" && !req.deferred {
		return res, apiErr{http.StatusUnprocessableEntity, "Give an image: the service has none yet."}
	}
	spec := parent.spec()
	spec.Ports = nil     // public ports belong to the service itself
	spec.Instances = 1   // a preview runs a single instance
	spec.VolumePath = "" // previews never get persistent storage
	if img := strings.TrimSpace(req.Image); img != "" {
		spec.Image = img
	}
	if !req.deferred { // a deferred preview gets its image from the build
		if err := spec.Validate(); err != nil {
			return res, apiErr{http.StatusUnprocessableEntity, err.Error()}
		}
		if err := registry.CheckNamespace(spec.Image, s.cfg.RegistryHost, o.Slug); err != nil {
			return res, apiErr{http.StatusUnprocessableEntity, err.Error()}
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
		if n >= maxPreviewsPerService {
			return res, apiErr{http.StatusConflict, "At most " + strconv.Itoa(maxPreviewsPerService) + " previews per service: delete one first."}
		}
		if err := s.checkQuota(r, tx, o, "", spec.CPUMillis, spec.MemoryMB, false); err != nil {
			return res, apiErr{http.StatusConflict, err.Error()}
		}
		b := make([]byte, 5)
		_, _ = rand.Read(b)
		slug = "pv-" + hex.EncodeToString(b)
		envJSON, _ := json.Marshal(spec.Env)
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO services (org_id, project_id, slug, image, port, cpu_millis, memory_mb, health_path, volume_path, env,
			                      waf_enabled, rate_limit_rpm, blocked_ips, preview_of, preview_name, expires_at, worker)
			SELECT $1, $2, $3, $4, port, cpu_millis, memory_mb, health_path, '', $5,
			       waf_enabled, rate_limit_rpm, blocked_ips, id, $6, $7, worker
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
			plain, err := s.box.Open(x.enc, "secret:"+parent.ID+":"+x.name)
			if err != nil {
				return res, err
			}
			enc, err := s.box.Seal(plain, "secret:"+id+":"+x.name)
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
	case req.deferred: // keep serving the current image until the build deploys the new one
		if _, err := tx.Exec(r.Context(), `UPDATE services SET expires_at = $2 WHERE id = $1`, id, expires); err != nil {
			return res, err
		}
	default:
		if _, err := tx.Exec(r.Context(), `UPDATE services SET image = $2, expires_at = $3 WHERE id = $1`, id, spec.Image, expires); err != nil {
			return res, err
		}
	}
	rev := 0
	if !req.deferred {
		if rev, err = s.createDeployment(r, tx, o, id, spec); err != nil {
			return res, err
		}
	}
	action := "preview.updated"
	if res.Created {
		action = "preview.created"
	}
	s.audit(r, tx, o.ID, action, "service:"+p.Slug+"/"+parent.Slug, map[string]any{"preview": req.Name, "image": spec.Image, "ttl_hours": req.Hours})
	res.Name, res.Slug, res.Revision, res.Expires = req.Name, slug, rev, expires
	res.URL = s.appURL(s.previewHost(req.Name, parent.Slug, p.Slug, o.Slug))
	return res, nil
}

func (s *Server) deletePreviewTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, parent *serviceInfo, name string) error {
	tag, err := tx.Exec(r.Context(), `DELETE FROM services WHERE preview_of = $1 AND preview_name = $2`, parent.ID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	s.audit(r, tx, o.ID, "preview.deleted", "service:"+p.Slug+"/"+parent.Slug, map[string]any{"preview": name})
	_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

// ---------- console ----------

func (s *Server) createPreviewForm(w http.ResponseWriter, r *http.Request) {
	var res previewResult
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if sv.isPreview() {
			formErr = "A preview cannot have previews."
			return nil
		}
		var err error
		res, err = s.upsertPreview(r, tx, o, p, sv, previewRequest{
			Name: r.PostFormValue("name"), Image: r.PostFormValue("image"), Hours: atoi(r.PostFormValue("ttl_hours")),
		})
		var ae apiErr
		if errors.As(err, &ae) {
			formErr = ae.msg
			return nil
		}
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#previews", "", formErr)
		return
	}
	msg := "Preview " + res.Name + " is deploying at " + res.URL
	if !res.Created {
		msg = "Preview " + res.Name + " is redeploying; it now expires " + res.Expires.UTC().Format("2006-01-02 15:04 UTC")
	}
	back(w, r, svcPath(o, p, sv)+"#previews", msg, "")
}

func (s *Server) deletePreviewForm(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		return s.deletePreviewTx(r, tx, o, p, sv, r.PathValue("name"))
	})
	if !ok {
		return
	}
	if isNoRows(err) {
		back(w, r, svcPath(o, p, sv)+"#previews", "", "No such preview.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#previews", "Preview deleted. Its container is being removed.", "")
}

// ---------- API v1 ----------

type apiPreview struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Image    string    `json:"image"`
	Status   string    `json:"status"`
	Expires  time.Time `json:"expires_at"`
	Revision int       `json:"revision,omitempty"`
}

func (s *Server) apiPreviews(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []apiPreview
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		list, err := s.loadPreviews(r, tx, o, p, sv)
		for _, v := range list {
			out = append(out, apiPreview{Name: v.Name, URL: v.URL, Image: v.Image, Status: v.Status, Expires: v.Expires})
		}
		return err
	}) {
		return
	}
	if out == nil {
		out = []apiPreview{}
	}
	writeJSON(w, out)
}

// apiCreatePreview: POST .../previews {"name": "pr-42", "image": "...", "ttl_hours": 72}
func (s *Server) apiCreatePreview(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var req previewRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"name": "pr-42", "image": "nginx:1.27-alpine"}`)
		return
	}
	var res previewResult
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		if sv.isPreview() {
			return apiErr{http.StatusUnprocessableEntity, "a preview cannot have previews"}
		}
		var err error
		res, err = s.upsertPreview(r, tx, o, p, sv, req)
		return err
	}) {
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiPreview{Name: res.Name, URL: res.URL, Image: req.Image, Status: "pending", Expires: res.Expires, Revision: res.Revision})
}

func (s *Server) apiDeletePreview(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		err := s.deletePreviewTx(r, tx, o, p, sv, r.PathValue("name"))
		if isNoRows(err) {
			return apiErr{http.StatusNotFound, "preview not found"}
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
