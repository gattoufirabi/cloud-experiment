package api

import (
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
)

func (s *API) queueBackupJob(r *http.Request, tx pgx.Tx, o *web.OrgInfo, sv *web.ServiceInfo, kind, snapshot string) (bool, error) {
	return s.QueueTargetJob(r, tx, o, "service_id", sv.ID, sv.Slug, kind, snapshot)
}

func (s *API) backupNow(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if sv.VolumePath == "" {
			formErr = "This service has no persistent folder, so there is nothing to back up."
			return nil
		}
		busy, err := s.queueBackupJob(r, tx, o, sv, "backup", "")
		if busy {
			formErr = "A backup or restore is already running for this service."
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
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#backups", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#backups", "Backup started.", "")
}

func (s *API) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	var formErr string
	snap := r.PathValue("snap")
	o, p, sv, err, ok := s.WithService(w, r, "admin", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if !web.FullSnapshotRe.MatchString(snap) {
			return pgx.ErrNoRows
		}
		if r.PostFormValue("confirm") != sv.Slug {
			formErr = "Type the service ID exactly to confirm the restore."
			return nil
		}
		var exists bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM backup_snapshots WHERE service_id = $1 AND snapshot_id = $2)`,
			sv.ID, snap).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return pgx.ErrNoRows
		}
		busy, err := s.queueBackupJob(r, tx, o, sv, "restore", snap)
		if busy {
			formErr = "A backup or restore is already running for this service."
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
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#backups", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#backups", "Restore started. The application stops briefly while its data is replaced.", "")
}

func (s *API) backupSettings(w http.ResponseWriter, r *http.Request) {
	enabled := r.PostFormValue("enabled") == "1"
	hour, err := strconv.Atoi(r.PostFormValue("hour"))
	if err != nil || hour < 0 || hour > 23 {
		hour = 3
	}
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if _, err := tx.Exec(r.Context(), `UPDATE services SET backup_enabled = $2, backup_hour = $3 WHERE id = $1`, sv.ID, enabled, hour); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "backup.settings", "service:"+sv.Slug, map[string]any{"enabled": enabled, "hour_utc": hour})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#backups", "Backup schedule saved.", "")
}
