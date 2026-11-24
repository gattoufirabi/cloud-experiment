package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Zip deployment: upload an application's source as a zip; it is built like a git
// build (its Dockerfile, or Railpack without one) and deployed.

const maxZipDeployBytes = 100 << 20

// checkSourceZip refuses anything that is not a readable zip with at least one file.
func checkSourceZip(b []byte) error {
	if len(b) == 0 {
		return apiErr{http.StatusUnprocessableEntity, "the archive is empty"}
	}
	if len(b) > maxZipDeployBytes {
		return apiErr{http.StatusRequestEntityTooLarge, fmt.Sprintf("the archive is larger than %d MB", maxZipDeployBytes>>20)}
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return apiErr{http.StatusUnprocessableEntity, "not a valid zip archive"}
	}
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			return nil
		}
	}
	return apiErr{http.StatusUnprocessableEntity, "the archive has no files"}
}

// queueZipBuild stores the archive with a new build of the service.
func (s *Server) queueZipBuild(r *http.Request, tx pgx.Tx, o *orgInfo, serviceID string, archive []byte, name string, deploy bool, userID string) (int, error) {
	if !s.cfg.BuildsEnabled || s.cfg.RegistryHost == "" {
		return 0, apiErr{http.StatusConflict, "Builds are not enabled on this platform."}
	}
	if err := checkSourceZip(archive); err != nil {
		return 0, err
	}
	var allowed bool
	if err := tx.QueryRow(r.Context(), `
		SELECT p.allow_builds FROM services s JOIN orgs o ON o.id = s.org_id JOIN plans p ON p.id = o.plan_id WHERE s.id = $1`, serviceID).Scan(&allowed); err != nil {
		return 0, err
	}
	if !allowed {
		return 0, apiErr{http.StatusForbidden, "Builds are not included in your plan (trial). Push an image instead, or upgrade."}
	}
	var active int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM builds WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
		return 0, err
	}
	if active >= maxActiveBuildsPerOrg {
		return 0, apiErr{http.StatusTooManyRequests, "Too many builds waiting for this organization; try again when one finishes."}
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == "" {
		name = "source.zip"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	sum := sha256.Sum256(archive)
	var n int
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO builds (org_id, service_id, number, trigger, deploy, created_by, source_zip, source_sha256, source_name)
		VALUES ($1, $2, (SELECT coalesce(max(number), 0) + 1 FROM builds WHERE service_id = $2), 'zip', $3, nullif($4, '')::uuid, $5, $6, $7)
		RETURNING number`, o.ID, serviceID, deploy, userID, archive, hex.EncodeToString(sum[:]), name).Scan(&n); err != nil {
		return 0, err
	}
	s.audit(r, tx, o.ID, "build.queued", "service:"+serviceID, map[string]any{"build": n, "trigger": "zip", "archive": name, "bytes": len(archive)})
	s.buildWake.signal() // a build server may take it
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'build')`); err != nil {
		return 0, err
	}
	return n, nil
}

// zipDeployForm: the service's Builds tab uploads an archive.
func (s *Server) zipDeployForm(w http.ResponseWriter, r *http.Request) {
	var n int
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		up, hdr, ferr := r.FormFile("archive")
		if ferr != nil {
			formErr = "Choose a zip archive of your application's source."
			return nil
		}
		defer up.Close()
		b, err := io.ReadAll(io.LimitReader(up, maxZipDeployBytes+1))
		if err != nil {
			return err
		}
		n, err = s.queueZipBuild(r, tx, o, sv.ID, b, hdr.Filename, r.PostFormValue("deploy") == "1", sessionFrom(r).UserID)
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
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			back(w, r, svcPath(o, p, sv)+"#builds", "", fmt.Sprintf("The archive is larger than %d MB.", maxZipDeployBytes>>20))
			return
		}
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"/builds/"+itoa(n), "Archive uploaded: build #"+itoa(n)+" queued.", "")
}

// apiZipDeploy: POST the archive as the body (Content-Type application/zip);
// ?deploy=false only builds and pushes the image; ?filename= names it in the log.
func (s *Server) apiZipDeploy(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxZipDeployBytes+1))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the archive is larger than %d MB", maxZipDeployBytes>>20))
		return
	}
	deploy := r.URL.Query().Get("deploy") != "false"
	var n int
	var statusPath string
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		var err error
		n, err = s.queueZipBuild(r, tx, o, sv.ID, b, r.URL.Query().Get("filename"), deploy, sessionFrom(r).UserID)
		statusPath = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/builds/" + itoa(n)
		return err
	}) {
		return
	}
	w.Header().Set("Location", statusPath)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "status": "queued", "status_url": statusPath})
}
