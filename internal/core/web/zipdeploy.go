package web

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Zip deployment: upload an application's source as a zip; it is built like a git
// build (its Dockerfile, or Railpack without one) and deployed.

const MaxZipDeployBytes = 100 << 20

// checkSourceZip refuses anything that is not a readable zip with at least one file.
func checkSourceZip(b []byte) error {
	if len(b) == 0 {
		return APIErr{http.StatusUnprocessableEntity, "the archive is empty"}
	}
	if len(b) > MaxZipDeployBytes {
		return APIErr{http.StatusRequestEntityTooLarge, fmt.Sprintf("the archive is larger than %d MB", MaxZipDeployBytes>>20)}
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return APIErr{http.StatusUnprocessableEntity, "not a valid zip archive"}
	}
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			return nil
		}
	}
	return APIErr{http.StatusUnprocessableEntity, "the archive has no files"}
}

// queueZipBuild stores the archive with a new build of the service.
func (s *Server) QueueZipBuild(r *http.Request, tx pgx.Tx, o *OrgInfo, serviceID string, archive []byte, name string, deploy bool, userID string) (int, error) {
	if !s.Cfg.BuildsEnabled || s.Cfg.RegistryHost == "" {
		return 0, APIErr{http.StatusConflict, "Builds are not enabled on this platform."}
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
		return 0, APIErr{http.StatusForbidden, "Builds are not included in your plan (trial). Push an image instead, or upgrade."}
	}
	var active int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM builds WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
		return 0, err
	}
	if active >= MaxActiveBuildsPerOrg {
		return 0, APIErr{http.StatusTooManyRequests, "Too many builds waiting for this organization; try again when one finishes."}
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
	s.Audit(r, tx, o.ID, "build.queued", "service:"+serviceID, map[string]any{"build": n, "trigger": "zip", "archive": name, "bytes": len(archive)})
	s.BuildWake.Signal() // a build server may take it
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'build')`); err != nil {
		return 0, err
	}
	return n, nil
}
