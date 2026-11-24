package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"platform/internal/components/storage/garage"
	"platform/internal/components/storage/s3"
	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
)

type storageKeyView struct {
	ID, Name, AccessKeyID, Service string
	Created                        time.Time
	Grants                         []grantView
}

type grantView struct {
	Bucket string
	Write  bool
}

func (s *API) garageClient() *garage.Client {
	return garage.New(s.Cfg.GarageAdminURL, s.Cfg.GarageAdminToken)
}

// storageOff answers when object storage is not configured on this platform.
func (s *API) storageOff(w http.ResponseWriter, r *http.Request) bool {
	if s.Cfg.StorageEnabled() {
		return false
	}
	s.NotFound(w, r)
	return true
}

func storagePath(o *web.OrgInfo) string { return o.Path() + "/storage" }

func loadStorageKeys(ctx context.Context, tx pgx.Tx) ([]storageKeyView, error) {
	rows, err := tx.Query(ctx, `
		SELECT k.id, k.name, k.access_key_id, coalesce(p.slug || '/' || sv.slug, ''), k.created_at,
		       coalesce((SELECT array_agg(b.name ORDER BY b.name) FROM storage_grants g JOIN buckets b ON b.id = g.bucket_id WHERE g.key_id = k.id), '{}'),
		       coalesce((SELECT array_agg(g.can_write ORDER BY b.name) FROM storage_grants g JOIN buckets b ON b.id = g.bucket_id WHERE g.key_id = k.id), '{}')
		FROM storage_keys k LEFT JOIN services sv ON sv.id = k.service_id LEFT JOIN projects p ON p.id = sv.project_id
		ORDER BY k.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storageKeyView
	for rows.Next() {
		var k storageKeyView
		var names []string
		var writes []bool
		if err := rows.Scan(&k.ID, &k.Name, &k.AccessKeyID, &k.Service, &k.Created, &names, &writes); err != nil {
			return nil, err
		}
		for i := range names {
			k.Grants = append(k.Grants, grantView{Bucket: names[i], Write: i < len(writes) && writes[i]})
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

type storageQuota struct {
	MaxBuckets, MaxStorageMB, Buckets, AllocatedMB int
}

func (q storageQuota) FreeMB() int { return max(0, q.MaxStorageMB-q.AllocatedMB) }

func loadStorageQuota(ctx context.Context, tx pgx.Tx, planID, excludeBucket string) (storageQuota, error) {
	var q storageQuota
	err := tx.QueryRow(ctx, `
		SELECT p.max_buckets, p.max_storage_mb, (SELECT count(*) FROM buckets),
		       (SELECT coalesce(sum(quota_mb), 0) FROM buckets WHERE id::text <> $2)
		FROM plans p WHERE p.id = $1`, planID, excludeBucket).Scan(&q.MaxBuckets, &q.MaxStorageMB, &q.Buckets, &q.AllocatedMB)
	return q, err
}

// ---------- pages ----------

func (s *API) storagePage(w http.ResponseWriter, r *http.Request) {
	if s.storageOff(w, r) {
		return
	}
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	s.renderStorage(w, r, o, web.Data{})
}

func (s *API) renderStorage(w http.ResponseWriter, r *http.Request, o *web.OrgInfo, d web.Data) {
	var buckets []*web.BucketInfo
	var keys []storageKeyView
	var q storageQuota
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if buckets, err = web.LoadBuckets(r.Context(), tx); err != nil {
			return err
		}
		if keys, err = loadStorageKeys(r.Context(), tx); err != nil {
			return err
		}
		q, err = loadStorageQuota(r.Context(), tx, o.PlanID, "")
		return err
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	d["Org"], d["Buckets"], d["Keys"], d["Quota"] = o, buckets, keys, q
	d["S3URL"], d["Region"], d["StoragePage"] = s.Cfg.S3PublicURL(), s.Cfg.S3Region, true
	d["CanEdit"], d["CanDelete"] = model.RoleAtLeast(o.Role, "developer"), model.RoleAtLeast(o.Role, "admin")
	s.Render(w, r, "storage", http.StatusOK, d)
}

func (s *API) createBucket(w http.ResponseWriter, r *http.Request) {
	if s.storageOff(w, r) {
		return
	}
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	name := strings.TrimSpace(strings.ToLower(r.PostFormValue("name")))
	quota := web.Atoi(r.PostFormValue("quota_mb"))
	full := o.Slug + "-" + name
	if !web.BucketNameRe.MatchString(name) || strings.HasPrefix(full, "xn--") || strings.HasPrefix(full, "sthree-") {
		web.Back(w, r, storagePath(o), "", "Bucket name: 3 to 40 characters, lowercase letters, digits and dashes, starting and ending with a letter or digit.")
		return
	}
	var formErr string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		q, err := loadStorageQuota(r.Context(), tx, o.PlanID, "")
		if err != nil {
			return err
		}
		if quota == 0 {
			quota = min(1024, q.FreeMB())
		}
		switch {
		case q.Buckets >= q.MaxBuckets:
			formErr = fmt.Sprintf("Your plan allows %d buckets.", q.MaxBuckets)
			return nil
		case quota < 1 || quota > q.FreeMB():
			formErr = fmt.Sprintf("Quota: 1 to %d MB (your plan includes %d MB of storage; %d MB are given to other buckets).", q.FreeMB(), q.MaxStorageMB, q.AllocatedMB)
			return nil
		}
		var exists bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM buckets WHERE name = $1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			formErr = "A bucket with this name already exists."
			return nil
		}
		return s.provisionBucket(r, tx, o, name, full, quota)
	})
	switch {
	case err != nil && isGarageConflict(err):
		web.Back(w, r, storagePath(o), "", "This bucket name is taken; choose another one.")
	case err != nil:
		s.ServerError(w, r, err)
	case formErr != "":
		web.Back(w, r, storagePath(o), "", formErr)
	default:
		web.Back(w, r, storagePath(o)+"/b/"+name, "Bucket "+full+" created.", "")
	}
}

func isGarageConflict(err error) bool {
	var e *garage.Error
	return errors.As(err, &e) && (e.Status == http.StatusConflict || e.Code == "BucketAlreadyExists")
}

// provisionBucket creates the bucket and its console key in Garage, then the row. If the
// transaction then fails, the Garage objects are removed again.
func (s *API) provisionBucket(r *http.Request, tx pgx.Tx, o *web.OrgInfo, name, full string, quotaMB int) (err error) {
	ctx := r.Context()
	g := s.garageClient()
	b, err := g.CreateBucket(ctx, full)
	if err != nil {
		return err
	}
	k, err := g.CreateKey(ctx, "console "+full)
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if k.AccessKeyID != "" {
			_ = g.DeleteKey(c, k.AccessKeyID)
		}
		_ = g.DeleteBucket(c, b.ID)
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	if err != nil {
		return err
	}
	if err = g.SetQuota(ctx, b.ID, int64(quotaMB)<<20); err != nil {
		return err
	}
	if err = g.SetPerms(ctx, b.ID, k.AccessKeyID, garage.Perms{Read: true, Write: true}); err != nil {
		return err
	}
	var id string
	if err = tx.QueryRow(ctx, `
		INSERT INTO buckets (org_id, name, full_name, garage_id, quota_mb, console_key_id, console_secret_enc)
		VALUES ($1, $2, $3, $4, $5, $6, '\x00') RETURNING id`, o.ID, name, full, b.ID, quotaMB, k.AccessKeyID).Scan(&id); err != nil {
		return err
	}
	enc, err := s.Box.Seal([]byte(k.SecretAccessKey), "bucket:"+id)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE buckets SET console_secret_enc = $2 WHERE id = $1`, id, enc); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "bucket.created", "bucket:"+full, map[string]any{"quota_mb": quotaMB})
	return nil
}

func (s *API) withBucket(w http.ResponseWriter, r *http.Request, minRole string,
	fn func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error) (*web.OrgInfo, *web.BucketInfo, error, bool) {
	if s.storageOff(w, r) {
		return nil, nil, nil, false
	}
	o, ok := s.Org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var b *web.BucketInfo
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if b, err = web.ScanBucket(tx.QueryRow(r.Context(), `SELECT `+web.BucketCols+` FROM buckets WHERE name = $1`, r.PathValue("bucket"))); err != nil {
			return err
		}
		return fn(tx, o, b)
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return nil, nil, nil, false
	}
	return o, b, err, true
}

func bucketPath(o *web.OrgInfo, b *web.BucketInfo) string { return storagePath(o) + "/b/" + b.Name }

// consoleClient is an S3 client with the bucket's console key, talking to Garage
// directly on the internal network.
func (s *API) consoleClient(b *web.BucketInfo) (*s3.Client, error) {
	secret, err := s.Box.Open(b.ConsoleSecretEnc, "bucket:"+b.ID)
	if err != nil {
		return nil, err
	}
	return s3.NewClient("http://"+s.Cfg.S3Upstream, s3.Credentials{AccessKey: b.ConsoleKeyID, SecretKey: string(secret), Region: s.Cfg.S3Region}), nil
}

type objectRow struct {
	Key, Name     string
	Size          int64
	Modified      time.Time
	Folder        bool
	DownloadQuery string
}

func (s *API) bucketPage(w http.ResponseWriter, r *http.Request) {
	var services []string
	var keys []storageKeyView
	o, b, err, ok := s.withBucket(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		rows, err := tx.Query(r.Context(), `SELECT p.slug || '/' || s.slug FROM services s JOIN projects p ON p.id = s.project_id WHERE s.preview_of IS NULL ORDER BY 1`)
		if err != nil {
			return err
		}
		if services, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		all, err := loadStorageKeys(r.Context(), tx)
		for _, k := range all {
			for _, g := range k.Grants {
				if g.Bucket == b.Name {
					keys = append(keys, storageKeyView{ID: k.ID, Name: k.Name, AccessKeyID: k.AccessKeyID, Service: k.Service, Created: k.Created, Grants: []grantView{g}})
				}
			}
		}
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	var objects []objectRow
	var listErr string
	var truncated bool
	if cl, err := s.consoleClient(b); err == nil {
		l, err := cl.List(r.Context(), b.FullName, prefix, "/", "", 200)
		if err != nil {
			listErr = "The objects could not be listed right now."
			s.Log.Warn("list bucket", "bucket", b.FullName, "err", err)
		}
		for _, p := range l.Prefixes {
			objects = append(objects, objectRow{Key: p, Name: strings.TrimPrefix(p, prefix), Folder: true})
		}
		for _, obj := range l.Objects {
			objects = append(objects, objectRow{Key: obj.Key, Name: strings.TrimPrefix(obj.Key, prefix), Size: obj.Size, Modified: obj.LastModified})
		}
		truncated = l.Truncated
	}
	var parent string
	if prefix != "" {
		parent = path.Dir(strings.TrimSuffix(prefix, "/")) + "/"
		if parent == "./" {
			parent = ""
		}
	}
	canEdit := model.RoleAtLeast(o.Role, "developer")
	s.Render(w, r, "bucket", http.StatusOK, web.Data{
		"Org": o, "Bucket": b, "Objects": objects, "Prefix": prefix, "Parent": parent, "ListError": listErr, "Truncated": truncated,
		"Services": services, "BucketKeys": keys, "Path": bucketPath(o, b), "StoragePage": true,
		"S3URL": s.Cfg.S3PublicURL(), "Region": s.Cfg.S3Region, "WebsiteURL": s.websiteURL(b),
		"CanEdit": canEdit, "CanDelete": model.RoleAtLeast(o.Role, "admin"), "MaxUploadMB": web.MaxUploadBytes >> 20,
	})
}

func (s *API) websiteURL(b *web.BucketInfo) string {
	scheme := "http"
	port := ""
	if s.Cfg.TLSMode == "acme" {
		scheme = "https"
	} else if _, p, ok := strings.Cut(s.Cfg.S3Host, ":"); ok {
		port = ":" + p // development: same port as the console
	}
	return scheme + "://" + b.FullName + "." + s.Cfg.WebDomain() + port + "/"
}

func (s *API) bucketSettings(w http.ResponseWriter, r *http.Request) {
	quota := web.Atoi(r.PostFormValue("quota_mb"))
	website := r.PostFormValue("website") == "1"
	index := strings.TrimSpace(r.PostFormValue("index_document"))
	errDoc := strings.TrimSpace(r.PostFormValue("error_document"))
	if index == "" {
		index = "index.html"
	}
	var formErr string
	o, b, err, ok := s.withBucket(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		q, err := loadStorageQuota(r.Context(), tx, o.PlanID, b.ID)
		if err != nil {
			return err
		}
		switch {
		case quota < 1 || quota > q.FreeMB():
			formErr = fmt.Sprintf("Quota: 1 to %d MB (your plan includes %d MB of storage; %d MB are given to other buckets).", q.FreeMB(), q.MaxStorageMB, q.AllocatedMB)
			return nil
		case !web.DocumentRe.MatchString(index) || strings.Contains(index, ".."), errDoc != "" && (!web.DocumentRe.MatchString(errDoc) || strings.Contains(errDoc, "..")):
			formErr = "Index and error documents are object keys such as index.html or 404.html."
			return nil
		}
		g := s.garageClient()
		if err := g.SetQuota(r.Context(), b.GarageID, int64(quota)<<20); err != nil {
			return err
		}
		if err := g.SetWebsite(r.Context(), b.GarageID, website, index, errDoc); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE buckets SET quota_mb = $2, website = $3, index_document = $4, error_document = $5 WHERE id = $1`,
			b.ID, quota, website, index, errDoc); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "bucket.updated", "bucket:"+b.FullName, map[string]any{"quota_mb": quota, "website": website})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, bucketPath(o, b)+"#settings", "", formErr)
		return
	}
	msg := "Settings saved."
	if website && !b.Website {
		msg = "Settings saved. The bucket is now public as a website at " + s.websiteURL(b)
	}
	web.Back(w, r, bucketPath(o, b)+"#settings", msg, "")
}

// corsOriginRe: a web origin, scheme and host with an optional port, no path.
var corsOriginRe = regexp.MustCompile(`^https?://[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)

// parseOrigins reads origins separated by commas, spaces or lines; "*" allows every site.
func parseOrigins(v string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, o := range strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		o = strings.TrimSuffix(o, "/")
		if o != "*" && !corsOriginRe.MatchString(o) {
			return nil, web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "origin " + o + ": a site address such as https://shop.example.com (no path), or * for every site"}
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if len(out) > 20 {
		return nil, web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "at most 20 origins"}
	}
	return out, nil
}

// setBucketCORSTx applies the origins on the storage (with the platform's own key for the
// bucket, which needs owner rights for it) and records them.
func (s *API) setBucketCORSTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo, origins []string) error {
	g := s.garageClient()
	if err := g.SetPerms(r.Context(), b.GarageID, b.ConsoleKeyID, garage.Perms{Read: true, Write: true, Owner: true}); err != nil {
		return err
	}
	cl, err := s.consoleClient(b)
	if err != nil {
		return err
	}
	if err := cl.SetCORS(r.Context(), b.FullName, origins); err != nil {
		return web.APIErr{Status: http.StatusBadGateway, Msg: "the storage refused the rule: " + err.Error()}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE buckets SET cors_origins = $2 WHERE id = $1`, b.ID, origins); err != nil {
		return err
	}
	b.CORSOrigins = origins
	s.Audit(r, tx, o.ID, "bucket.cors", "bucket:"+b.FullName, map[string]any{"origins": origins})
	return nil
}

func (s *API) bucketCORS(w http.ResponseWriter, r *http.Request) {
	origins, perr := parseOrigins(r.PostFormValue("origins"))
	o, b, err, ok := s.withBucket(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		if perr != nil {
			return perr
		}
		return s.setBucketCORSTx(r, tx, o, b, origins)
	})
	if !ok {
		return
	}
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, bucketPath(o, b)+"#settings", "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	case len(origins) == 0:
		web.Back(w, r, bucketPath(o, b)+"#settings", "Browser access removed.", "")
	default:
		web.Back(w, r, bucketPath(o, b)+"#settings", "Browser access saved: pages of "+strings.Join(origins, ", ")+" can use the bucket with presigned URLs.", "")
	}
}

func (s *API) deleteBucket(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, b, err, ok := s.withBucket(w, r, "admin", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		if r.PostFormValue("confirm") != b.Name {
			formErr = "Type the bucket name exactly to confirm deletion."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM buckets WHERE id = $1`, b.ID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "bucket.deleted", "bucket:"+b.FullName, map[string]any{"objects": b.Objects})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'storage')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, bucketPath(o, b)+"#settings", "", formErr)
		return
	}
	web.Back(w, r, storagePath(o), "Bucket "+b.FullName+" deleted. Its objects are being removed.", "")
}

// ---------- objects ----------

func (s *API) uploadObject(w http.ResponseWriter, r *http.Request) {
	var formErr, key string
	o, b, err, ok := s.withBucket(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		f, hdr, err := r.FormFile("file")
		if err != nil {
			formErr = fmt.Sprintf("Choose a file of at most %d MB.", web.MaxUploadBytes>>20)
			return nil
		}
		defer f.Close()
		var buf bytes.Buffer
		n, err := io.Copy(&buf, io.LimitReader(f, web.MaxUploadBytes+1))
		if err != nil {
			return err
		}
		if n > web.MaxUploadBytes {
			formErr = fmt.Sprintf("Console uploads are limited to %d MB; use the S3 API for larger files.", web.MaxUploadBytes>>20)
			return nil
		}
		key = strings.TrimLeft(r.PostFormValue("prefix")+path.Base(strings.ReplaceAll(hdr.Filename, "\\", "/")), "/")
		if key == "" || len(key) > 1024 || strings.Contains(key, "..") {
			formErr = "Invalid file name."
			return nil
		}
		ctype := hdr.Header.Get("Content-Type")
		if ctype == "" || ctype == "application/octet-stream" {
			if t := mime.TypeByExtension(path.Ext(key)); t != "" {
				ctype = t
			}
		}
		cl, err := s.consoleClient(b)
		if err != nil {
			return err
		}
		if err := cl.Put(r.Context(), b.FullName, key, &buf, n, ctype); err != nil {
			var se *s3.Error
			if errors.As(err, &se) && se.Code == "QuotaExceeded" {
				formErr = "The bucket's quota is reached: delete objects or raise its quota."
				return nil
			}
			return err
		}
		s.Audit(r, tx, o.ID, "object.uploaded", "bucket:"+b.FullName, map[string]any{"key": key, "bytes": n})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, bucketPath(o, b)+"?prefix="+url.QueryEscape(r.PostFormValue("prefix"))+"#objects", web.Ternary(formErr == "", "Uploaded "+key+".", ""), formErr)
}

func (s *API) deleteObject(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	o, b, err, ok := s.withBucket(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		if key == "" {
			return pgx.ErrNoRows
		}
		cl, err := s.consoleClient(b)
		if err != nil {
			return err
		}
		if err := cl.Delete(r.Context(), b.FullName, key); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "object.deleted", "bucket:"+b.FullName, map[string]any{"key": key})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, bucketPath(o, b)+"?prefix="+url.QueryEscape(r.PostFormValue("prefix"))+"#objects", "Deleted "+key+".", "")
}

// downloadObject redirects to a short-lived presigned URL on the public S3 endpoint.
func (s *API) downloadObject(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	var link string
	_, _, err, ok := s.withBucket(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		if key == "" {
			return pgx.ErrNoRows
		}
		secret, err := s.Box.Open(b.ConsoleSecretEnc, "bucket:"+b.ID)
		if err != nil {
			return err
		}
		link, err = s3.Presign(s3.Credentials{AccessKey: b.ConsoleKeyID, SecretKey: string(secret), Region: s.Cfg.S3Region},
			http.MethodGet, s3.ObjectURL(s.Cfg.S3PublicURL(), b.FullName, key), 5*time.Minute, time.Now())
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	http.Redirect(w, r, link, http.StatusSeeOther)
}

// ---------- access keys ----------

// createStorageKey creates an access key with the chosen permission on each bucket
// ("perm_<bucket name>" = "" | "read" | "write") and shows its secret once.
func (s *API) createStorageKey(w http.ResponseWriter, r *http.Request) {
	if s.storageOff(w, r) {
		return
	}
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || len(name) > 60 {
		web.Back(w, r, storagePath(o)+"#keys", "", "Give the key a name (up to 60 characters).")
		return
	}
	var key garage.Key
	var formErr string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		buckets, err := web.LoadBuckets(r.Context(), tx)
		if err != nil {
			return err
		}
		perms := map[*web.BucketInfo]string{}
		for _, b := range buckets {
			if p := r.PostFormValue("perm_" + b.Name); p == "read" || p == "write" {
				perms[b] = p
			}
		}
		if len(perms) == 0 {
			formErr = "Give the key access to at least one bucket."
			return nil
		}
		key, err = s.newStorageKey(r, tx, o, name, nil, perms)
		return err
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, storagePath(o)+"#keys", "", formErr)
		return
	}
	s.renderStorage(w, r, o, web.Data{"NewKey": key, "Tab": "keys"})
}

// newStorageKey creates a key in Garage with its bucket permissions, then its rows; the
// Garage key is deleted again if that fails.
func (s *API) newStorageKey(r *http.Request, tx pgx.Tx, o *web.OrgInfo, name string, serviceID *string, perms map[*web.BucketInfo]string) (k garage.Key, err error) {
	ctx := r.Context()
	g := s.garageClient()
	if k, err = g.CreateKey(ctx, o.Slug+" "+name); err != nil {
		return k, err
	}
	defer func() {
		if err != nil {
			c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = g.DeleteKey(c, k.AccessKeyID)
		}
	}()
	var id string
	if err = tx.QueryRow(ctx, `INSERT INTO storage_keys (org_id, name, access_key_id, service_id, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		o.ID, name, k.AccessKeyID, serviceID, web.NullableUser(r)).Scan(&id); err != nil {
		return k, err
	}
	detail := map[string]string{}
	for b, p := range perms {
		write := p == "write"
		if !o.Suspended {
			if err = g.SetPerms(ctx, b.GarageID, k.AccessKeyID, garage.Perms{Read: true, Write: write}); err != nil {
				return k, err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO storage_grants (org_id, key_id, bucket_id, can_write) VALUES ($1, $2, $3, $4)`, o.ID, id, b.ID, write); err != nil {
			return k, err
		}
		detail[b.Name] = p
	}
	s.Audit(r, tx, o.ID, "storage_key.created", "storage_key:"+name, map[string]any{"access_key_id": k.AccessKeyID, "buckets": detail})
	return k, nil
}

func (s *API) deleteStorageKey(w http.ResponseWriter, r *http.Request) {
	if s.storageOff(w, r) {
		return
	}
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var name, akid string
		if err := tx.QueryRow(r.Context(), `DELETE FROM storage_keys WHERE id = $1 RETURNING name, access_key_id`, r.PathValue("id")).Scan(&name, &akid); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "storage_key.deleted", "storage_key:"+name, map[string]string{"access_key_id": akid})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'storage')`)
		return err
	})
	if err != nil && !web.IsNoRows(err) {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, storagePath(o)+"#keys", "Access key deleted. It stops working within a few seconds.", "")
}

// attachBucket creates a key for one service with read-write access to the bucket and
// stores the usual AWS variables as encrypted secrets of that service.
func (s *API) attachBucket(w http.ResponseWriter, r *http.Request) {
	target := r.PostFormValue("service") // "project/service"
	var formErr, svcSlug string
	o, b, err, ok := s.withBucket(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, b *web.BucketInfo) error {
		ps, ss, _ := strings.Cut(target, "/")
		var serviceID string
		if err := tx.QueryRow(r.Context(), `SELECT s.id FROM services s JOIN projects p ON p.id = s.project_id WHERE p.slug = $1 AND s.slug = $2 AND s.preview_of IS NULL`,
			ps, ss).Scan(&serviceID); err != nil {
			formErr = "Choose a service."
			return nil
		}
		svcSlug = target
		k, err := s.newStorageKey(r, tx, o, "service "+target+" on "+b.Name, &serviceID, map[*web.BucketInfo]string{b: "write"})
		if err != nil {
			return err
		}
		// S3_* for your own code; the conventional names many S3 libraries read by themselves too
		vars := map[string]string{
			"S3_ENDPOINT": s.Cfg.S3Endpoint(), "S3_REGION": s.Cfg.S3Region, "S3_ACCESS_KEY_ID": k.AccessKeyID,
			"S3_SECRET_ACCESS_KEY": k.SecretAccessKey, "S3_BUCKET": b.FullName,
			"AWS_ACCESS_KEY_ID": k.AccessKeyID, "AWS_SECRET_ACCESS_KEY": k.SecretAccessKey, "AWS_REGION": s.Cfg.S3Region,
			"AWS_DEFAULT_REGION": s.Cfg.S3Region, "AWS_ENDPOINT_URL": s.Cfg.S3Endpoint(), "AWS_ENDPOINT_URL_S3": s.Cfg.S3Endpoint(),
		}
		for name, value := range vars {
			enc, err := s.Box.Seal([]byte(value), "secret:"+serviceID+":"+name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO secrets (org_id, service_id, name, value_enc, source) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, source = EXCLUDED.source, created_at = now()`,
				o.ID, serviceID, name, enc, "bucket:"+b.ID); err != nil {
				return err
			}
		}
		s.Audit(r, tx, o.ID, "bucket.attached", "bucket:"+b.FullName, map[string]string{"service": target, "access_key_id": k.AccessKeyID})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, bucketPath(o, b)+"#connect", "", formErr)
		return
	}
	web.Back(w, r, bucketPath(o, b)+"#connect", "Secrets S3_ENDPOINT, S3_REGION, S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY and S3_BUCKET added to "+svcSlug+". Redeploy it to apply them.", "")
}
