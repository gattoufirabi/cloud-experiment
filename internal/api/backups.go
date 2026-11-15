package api

import (
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

var fullSnapshotRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type backupView struct {
	Enabled bool
	Hour    int
	Jobs    []backupJobView
	Snaps   []snapshotView
}

type backupJobView struct {
	Kind, Trigger, Snapshot, Status, Message string
	CreatedAt                                time.Time
	FinishedAt                               *time.Time
}

type snapshotView struct {
	ID      string
	TakenAt time.Time
	Size    int64
	Files   int64
	Tags    string
}

func (sn snapshotView) Short() string { return sn.ID[:8] }

func (sn snapshotView) HumanSize() string {
	if sn.Size == 0 {
		return "—"
	}
	f := float64(sn.Size)
	for _, u := range []string{"B", "KiB", "MiB", "GiB"} {
		if f < 1024 {
			return strconv.FormatFloat(f, 'f', 1, 64) + " " + u
		}
		f /= 1024
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " TiB"
}

func loadBackups(r *http.Request, tx pgx.Tx, serviceID string) (*backupView, error) {
	var b backupView
	if err := tx.QueryRow(r.Context(), `SELECT backup_enabled, backup_hour FROM services WHERE id = $1`, serviceID).Scan(&b.Enabled, &b.Hour); err != nil {
		return nil, err
	}
	rows, err := tx.Query(r.Context(), `
		SELECT kind, trigger, snapshot_id, status, message, created_at, finished_at
		FROM backup_jobs WHERE service_id = $1 ORDER BY created_at DESC LIMIT 6`, serviceID)
	if err != nil {
		return nil, err
	}
	if b.Jobs, err = pgx.CollectRows(rows, pgx.RowToStructByPos[backupJobView]); err != nil {
		return nil, err
	}
	rows, err = tx.Query(r.Context(), `
		SELECT snapshot_id, taken_at, size_bytes, files, tags FROM backup_snapshots
		WHERE service_id = $1 ORDER BY taken_at DESC LIMIT 60`, serviceID)
	if err != nil {
		return nil, err
	}
	b.Snaps, err = pgx.CollectRows(rows, pgx.RowToStructByPos[snapshotView])
	return &b, err
}

func (s *Server) queueBackupJob(r *http.Request, tx pgx.Tx, o *orgInfo, sv *serviceInfo, kind, snapshot string) (bool, error) {
	var busy bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM backup_jobs WHERE service_id = $1 AND status IN ('queued', 'running'))`,
		sv.ID).Scan(&busy); err != nil || busy {
		return busy, err
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO backup_jobs (org_id, service_id, kind, snapshot_id, requested_by) VALUES ($1, $2, $3, $4, $5)`,
		o.ID, sv.ID, kind, snapshot, sessionFrom(r).UserID); err != nil {
		return false, err
	}
	s.audit(r, tx, o.ID, "backup."+kind+".requested", "service:"+sv.Slug, map[string]string{"snapshot": snapshot})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'backup')`)
	return false, err
}

func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
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
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#backups", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#backups", "Backup started.", "")
}

func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	var formErr string
	snap := r.PathValue("snap")
	o, p, sv, err, ok := s.withService(w, r, "admin", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if !fullSnapshotRe.MatchString(snap) {
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
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, svcPath(o, p, sv)+"#backups", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#backups", "Restore started. The application stops briefly while its data is replaced.", "")
}

func (s *Server) backupSettings(w http.ResponseWriter, r *http.Request) {
	enabled := r.PostFormValue("enabled") == "1"
	hour, err := strconv.Atoi(r.PostFormValue("hour"))
	if err != nil || hour < 0 || hour > 23 {
		hour = 3
	}
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if _, err := tx.Exec(r.Context(), `UPDATE services SET backup_enabled = $2, backup_hour = $3 WHERE id = $1`, sv.ID, enabled, hour); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "backup.settings", "service:"+sv.Slug, map[string]any{"enabled": enabled, "hour_utc": hour})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#backups", "Backup schedule saved.", "")
}
