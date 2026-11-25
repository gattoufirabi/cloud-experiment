package web

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var FullSnapshotRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type BackupView struct {
	Enabled bool
	Hour    int
	Jobs    []backupJobView
	Snaps   []SnapshotView
}

type backupJobView struct {
	Kind, Trigger, Snapshot, Status, Message string
	CreatedAt                                time.Time
	FinishedAt                               *time.Time
}

type SnapshotView struct {
	ID      string
	TakenAt time.Time
	Size    int64
	Files   int64
	Tags    string
}

func (sn SnapshotView) Short() string { return sn.ID[:8] }

func (sn SnapshotView) HumanSize() string {
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

func LoadBackups(r *http.Request, tx pgx.Tx, serviceID string) (*BackupView, error) {
	var enabled bool
	var hour int
	if err := tx.QueryRow(r.Context(), `SELECT backup_enabled, backup_hour FROM services WHERE id = $1`, serviceID).Scan(&enabled, &hour); err != nil {
		return nil, err
	}
	return LoadTargetBackups(r, tx, "service_id", serviceID, enabled, hour)
}

// loadTargetBackups lists jobs and snapshots of a service or a database.
// col is one of two constants ("service_id", "database_id"), never user input.
func LoadTargetBackups(r *http.Request, tx pgx.Tx, col, id string, enabled bool, hour int) (*BackupView, error) {
	b := BackupView{Enabled: enabled, Hour: hour}
	rows, err := tx.Query(r.Context(), `
		SELECT kind, trigger, snapshot_id, status, message, created_at, finished_at
		FROM backup_jobs WHERE `+col+` = $1 ORDER BY created_at DESC LIMIT 6`, id)
	if err != nil {
		return nil, err
	}
	if b.Jobs, err = pgx.CollectRows(rows, pgx.RowToStructByPos[backupJobView]); err != nil {
		return nil, err
	}
	rows, err = tx.Query(r.Context(), `
		SELECT snapshot_id, taken_at, size_bytes, files, tags FROM backup_snapshots
		WHERE `+col+` = $1 ORDER BY taken_at DESC LIMIT 60`, id)
	if err != nil {
		return nil, err
	}
	b.Snaps, err = pgx.CollectRows(rows, pgx.RowToStructByPos[SnapshotView])
	return &b, err
}

// queueTargetJob queues a backup or restore of a service or database unless one runs.
func (s *Server) QueueTargetJob(r *http.Request, tx pgx.Tx, o *OrgInfo, col, id, label, kind, snapshot string) (bool, error) {
	var busy bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM backup_jobs WHERE `+col+` = $1 AND status IN ('queued', 'running'))`,
		id).Scan(&busy); err != nil || busy {
		return busy, err
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO backup_jobs (org_id, `+col+`, kind, snapshot_id, requested_by) VALUES ($1, $2, $3, $4, $5)`,
		o.ID, id, kind, snapshot, SessionFrom(r).UserID); err != nil {
		return false, err
	}
	target := strings.TrimSuffix(col, "_id") + ":" + label
	s.Audit(r, tx, o.ID, "backup."+kind+".requested", target, map[string]string{"snapshot": snapshot})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'backup')`)
	return false, err
}
