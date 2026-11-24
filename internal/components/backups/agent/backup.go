package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/worker"
)

func (a *Worker) BackupLoop(ctx context.Context) {
	if !a.Cfg.BackupsEnabled {
		return
	}
	if !worker.KeepRe.MatchString(a.Cfg.BackupKeep) {
		a.Log.Error("BACKUP_KEEP contains invalid characters; backups disabled")
		return
	}
	_, _ = a.Pool.Exec(ctx, `
		UPDATE backup_jobs j SET status = 'error', message = 'interrupted by agent restart', finished_at = now()
		FROM projects p
		WHERE j.status = 'running' AND p.id = coalesce((SELECT project_id FROM services WHERE id = j.service_id),
		                                               (SELECT project_id FROM databases WHERE id = j.database_id))
		  AND `+worker.MineSQL, a.MineArgs()...)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if a.IsPrimary() {
			a.scheduleBackups(ctx)
		}
		a.processBackups(ctx)
		select {
		case <-ctx.Done():
			return
		case <-a.BackupWake:
		case <-tick.C:
		}
	}
}

func (a *Worker) PokeBackups() {
	select {
	case a.BackupWake <- struct{}{}:
	default:
	}
}

// scheduleBackups queues the nightly backup of every service with a volume and every
// running database, once a day after its backup hour (UTC). backup_attempted_at prevents
// retry storms on failure.
func (a *Worker) scheduleBackups(ctx context.Context) {
	const due = `extract(hour FROM now() AT TIME ZONE 'UTC') >= t.backup_hour
		AND (t.backup_attempted_at IS NULL OR t.backup_attempted_at <
		     (date_trunc('day', now() AT TIME ZONE 'UTC') + make_interval(hours => t.backup_hour)) AT TIME ZONE 'UTC')`
	_, err := a.Pool.Exec(ctx, `
		WITH d AS (
			UPDATE services t SET backup_attempted_at = now()
			FROM orgs o
			WHERE o.id = t.org_id AND NOT o.suspended AND t.volume_path <> '' AND t.backup_enabled AND `+due+`
			  AND EXISTS (SELECT 1 FROM deployments x WHERE x.service_id = t.id AND x.status = 'live')
			  AND NOT EXISTS (SELECT 1 FROM backup_jobs j WHERE j.service_id = t.id AND j.status IN ('queued', 'running'))
			RETURNING t.id, t.org_id
		)
		INSERT INTO backup_jobs (org_id, service_id, kind, trigger) SELECT org_id, id, 'backup', 'schedule' FROM d`)
	if err != nil {
		a.Log.Error("schedule service backups", "err", err)
	}
	_, err = a.Pool.Exec(ctx, `
		WITH d AS (
			UPDATE databases t SET backup_attempted_at = now()
			FROM orgs o
			WHERE o.id = t.org_id AND NOT o.suspended AND t.status = 'running' AND t.backup_enabled AND t.engine <> 'kafka' AND `+due+`
			  AND NOT EXISTS (SELECT 1 FROM backup_jobs j WHERE j.database_id = t.id AND j.status IN ('queued', 'running'))
			RETURNING t.id, t.org_id
		)
		INSERT INTO backup_jobs (org_id, database_id, kind, trigger) SELECT org_id, id, 'backup', 'schedule' FROM d`)
	if err != nil {
		a.Log.Error("schedule database backups", "err", err)
	}
}

func (a *Worker) processBackups(ctx context.Context) {
	for ctx.Err() == nil {
		var j worker.BackupJob
		found := false
		err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `
				SELECT j.id, j.org_id, coalesce(j.service_id::text, ''), coalesce(j.database_id::text, ''), j.kind, j.snapshot_id
				FROM backup_jobs j JOIN projects p ON p.id = coalesce((SELECT project_id FROM services WHERE id = j.service_id),
				                                                      (SELECT project_id FROM databases WHERE id = j.database_id))
				WHERE j.status = 'queued' AND `+worker.MineSQL+` ORDER BY j.created_at LIMIT 1
				FOR UPDATE OF j SKIP LOCKED`, a.MineArgs()...).Scan(&j.ID, &j.OrgID, &j.ServiceID, &j.DatabaseID, &j.Kind, &j.Snapshot)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			_, err = tx.Exec(ctx, `UPDATE backup_jobs SET status = 'running', message = '' WHERE id = $1`, j.ID)
			return err
		})
		if err != nil {
			a.Log.Error("claim backup job", "err", err)
			return
		}
		if !found {
			return
		}
		host, _ := j.Target()
		a.Log.Info("backup job", "job", j.ID, "kind", j.Kind, "target", host)
		var msg string
		switch {
		case j.DatabaseID != "" && j.Kind == "restore":
			msg, err = a.RestoreDatabase(ctx, j)
		case j.DatabaseID != "":
			msg, err = a.BackupDatabase(ctx, j, "")
		case j.Kind == "restore":
			msg, err = a.restore(ctx, j)
		default:
			msg, err = a.backup(ctx, j, "")
		}
		status, action := "done", "backup."+j.Kind
		targetLabel := "service:" + j.ServiceID
		if j.DatabaseID != "" {
			targetLabel = "database:" + j.DatabaseID
		}
		if err != nil {
			status, msg, action = "error", worker.Truncate(err.Error(), 1500), "backup."+j.Kind+".failed"
			a.Log.Warn("backup job failed", "job", j.ID, "err", err)
			a.AlertEvent(ctx, j.OrgID, j.ServiceID, "backup_failed",
				fmt.Sprintf("The %s job of %s failed: %s", j.Kind, targetLabel, worker.Truncate(worker.FirstLine(err.Error()), 300)))
		}
		_, _ = a.Pool.Exec(ctx, `UPDATE backup_jobs SET status = $2, message = $3, finished_at = now() WHERE id = $1`, j.ID, status, msg)
		_ = worker.AuditAgent(ctx, a.Agent, j.OrgID, action, targetLabel, map[string]string{"job": j.ID, "snapshot": j.Snapshot})
	}
}

func (a *Worker) backup(ctx context.Context, j worker.BackupJob, tag string) (string, error) {
	return a.BackupVolume(ctx, j, worker.VolumeName(j.ServiceID), tag)
}

// restore replaces the service volume with a snapshot: safety snapshot, stop, restore, start.
func (a *Worker) restore(ctx context.Context, j worker.BackupJob) (string, error) {
	if !a.SnapshotBelongs(ctx, j) {
		return "", errors.New("this snapshot does not belong to the service")
	}
	if _, err := a.backup(ctx, j, "pre-restore"); err != nil {
		return "", fmt.Errorf("safety snapshot before restore failed, nothing was changed: %w", err)
	}
	var containerID string
	_ = a.Pool.QueryRow(ctx, `SELECT coalesce(container_id, '') FROM deployments WHERE service_id = $1 AND status = 'live'`, j.ServiceID).Scan(&containerID)
	if containerID != "" {
		if err := a.Docker.ContainerStop(ctx, containerID, 20); err != nil {
			return "", fmt.Errorf("stop the application before restoring: %w", err)
		}
		defer func() {
			if err := a.Docker.ContainerStart(context.Background(), containerID); err != nil {
				a.Log.Error("restart after restore", "container", containerID, "err", err)
			}
		}()
	}
	if err := a.RestoreVolume(ctx, j, worker.VolumeName(j.ServiceID)); err != nil {
		return "", err
	}
	return "restored snapshot " + worker.Short(j.Snapshot) + " (a pre-restore snapshot of the previous data was kept)", nil
}
