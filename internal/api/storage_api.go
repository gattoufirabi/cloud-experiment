package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// REST API for object storage: buckets and access keys (same rules as the console).

type apiBucket struct {
	Name     string    `json:"name"`
	S3Name   string    `json:"s3_name"`
	QuotaMB  int       `json:"quota_mb"`
	Objects  int64     `json:"objects"`
	Bytes    int64     `json:"bytes"`
	Website  string    `json:"website_url,omitempty"`
	Endpoint string    `json:"endpoint"`
	Region   string    `json:"region"`
	Created  time.Time `json:"created_at"`
}

func (s *Server) apiBucketView(b *bucketInfo) apiBucket {
	v := apiBucket{Name: b.Name, S3Name: b.FullName, QuotaMB: b.QuotaMB, Objects: b.Objects, Bytes: b.Bytes,
		Endpoint: s.cfg.S3PublicURL(), Region: s.cfg.S3Region, Created: b.CreatedAt}
	if b.Website {
		v.Website = s.websiteURL(b)
	}
	return v
}

// apiStorage runs fn in the organization's transaction and answers its apiErr, if any.
func (s *Server) apiStorage(w http.ResponseWriter, r *http.Request, o *orgInfo, fn func(tx pgx.Tx) error) bool {
	if !s.cfg.StorageEnabled() {
		apiError(w, http.StatusNotFound, "object storage is not enabled on this platform")
		return false
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, fn)
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		apiError(w, ae.status, ae.msg)
	case isNoRows(err):
		apiError(w, http.StatusNotFound, "not found")
	case err != nil && isGarageConflict(err):
		apiError(w, http.StatusConflict, "this bucket name is taken")
	case err != nil:
		s.log.Error("storage api", "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	default:
		return true
	}
	return false
}

func (s *Server) apiBuckets(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []apiBucket
	if s.apiStorage(w, r, o, func(tx pgx.Tx) error {
		bs, err := loadBuckets(r.Context(), tx)
		for _, b := range bs {
			out = append(out, s.apiBucketView(b))
		}
		return err
	}) {
		if out == nil {
			out = []apiBucket{}
		}
		writeJSON(w, out)
	}
}

func (s *Server) apiCreateBucket(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Name    string `json:"name"`
		QuotaMB int    `json:"quota_mb"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"name": "uploads", "quota_mb": 1024}`)
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.Name))
	full := o.Slug + "-" + name
	var created *bucketInfo
	if s.apiStorage(w, r, o, func(tx pgx.Tx) error {
		if !bucketNameRe.MatchString(name) || strings.HasPrefix(full, "xn--") || strings.HasPrefix(full, "sthree-") {
			return apiErr{http.StatusUnprocessableEntity, "bucket name: 3 to 40 characters, lowercase letters, digits and dashes"}
		}
		q, err := loadStorageQuota(r.Context(), tx, o.PlanID, "")
		if err != nil {
			return err
		}
		if body.QuotaMB == 0 {
			body.QuotaMB = min(1024, q.FreeMB())
		}
		if q.Buckets >= q.MaxBuckets {
			return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d buckets", q.MaxBuckets)}
		}
		if body.QuotaMB < 1 || body.QuotaMB > q.FreeMB() {
			return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("quota_mb must be between 1 and %d", q.FreeMB())}
		}
		var exists bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM buckets WHERE name = $1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return apiErr{http.StatusConflict, "a bucket with this name already exists"}
		}
		if err := s.provisionBucket(r, tx, o, name, full, body.QuotaMB); err != nil {
			return err
		}
		created, err = scanBucket(tx.QueryRow(r.Context(), `SELECT `+bucketCols+` FROM buckets WHERE name = $1`, name))
		return err
	}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(s.apiBucketView(created))
	}
}

func (s *Server) apiDeleteBucket(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiStorage(w, r, o, func(tx pgx.Tx) error {
		var full string
		var objects int64
		if err := tx.QueryRow(r.Context(), `DELETE FROM buckets WHERE name = $1 RETURNING full_name, objects`, r.PathValue("bucket")).Scan(&full, &objects); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "bucket.deleted", "bucket:"+full, map[string]any{"objects": objects})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'storage')`)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

type apiStorageKey struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	AccessKeyID string            `json:"access_key_id"`
	Secret      string            `json:"secret_access_key,omitempty"`
	Service     string            `json:"service,omitempty"`
	Buckets     map[string]string `json:"buckets"`
	Created     time.Time         `json:"created_at"`
}

func (s *Server) apiStorageKeys(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	out := []apiStorageKey{}
	if s.apiStorage(w, r, o, func(tx pgx.Tx) error {
		keys, err := loadStorageKeys(r.Context(), tx)
		for _, k := range keys {
			v := apiStorageKey{ID: k.ID, Name: k.Name, AccessKeyID: k.AccessKeyID, Service: k.Service, Created: k.Created, Buckets: map[string]string{}}
			for _, g := range k.Grants {
				v.Buckets[g.Bucket] = ternary(g.Write, "write", "read")
			}
			out = append(out, v)
		}
		return err
	}) {
		writeJSON(w, out)
	}
}

// apiCreateStorageKey: {"name": "ci", "buckets": {"uploads": "write", "site": "read"}};
// the answer holds the secret, shown only this once.
func (s *Server) apiCreateStorageKey(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Name    string            `json:"name"`
		Buckets map[string]string `json:"buckets"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"name": "ci", "buckets": {"uploads": "write"}}`)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	var out apiStorageKey
	if s.apiStorage(w, r, o, func(tx pgx.Tx) error {
		if body.Name == "" || len(body.Name) > 60 {
			return apiErr{http.StatusUnprocessableEntity, "name: 1 to 60 characters"}
		}
		buckets, err := loadBuckets(r.Context(), tx)
		if err != nil {
			return err
		}
		byName := map[string]*bucketInfo{}
		for _, b := range buckets {
			byName[b.Name] = b
		}
		perms := map[*bucketInfo]string{}
		for name, p := range body.Buckets {
			b := byName[name]
			if b == nil {
				return apiErr{http.StatusUnprocessableEntity, "no bucket named " + name}
			}
			if p != "read" && p != "write" {
				return apiErr{http.StatusUnprocessableEntity, `bucket access is "read" or "write"`}
			}
			perms[b] = p
		}
		if len(perms) == 0 {
			return apiErr{http.StatusUnprocessableEntity, "give the key access to at least one bucket"}
		}
		k, err := s.newStorageKey(r, tx, o, body.Name, nil, perms)
		if err != nil {
			return err
		}
		out = apiStorageKey{Name: body.Name, AccessKeyID: k.AccessKeyID, Secret: k.SecretAccessKey, Buckets: body.Buckets, Created: time.Now().UTC()}
		return tx.QueryRow(r.Context(), `SELECT id FROM storage_keys WHERE access_key_id = $1`, k.AccessKeyID).Scan(&out.ID)
	}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	}
}

func (s *Server) apiDeleteStorageKey(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiStorage(w, r, o, func(tx pgx.Tx) error {
		var name, akid string
		// by ID or by access key ID
		if err := tx.QueryRow(r.Context(), `DELETE FROM storage_keys WHERE id::text = $1 OR access_key_id = $1 RETURNING name, access_key_id`,
			r.PathValue("id")).Scan(&name, &akid); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "storage_key.deleted", "storage_key:"+name, map[string]string{"access_key_id": akid})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'storage')`)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}
