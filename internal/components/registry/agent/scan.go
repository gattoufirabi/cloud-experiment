package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------- background scans: pushes and daily rescans ----------

func (a *Worker) ScanLoop(ctx context.Context) {
	if !a.Cfg.ScannerEnabled || !a.IsPrimary() {
		return
	}
	_, _ = a.Pool.Exec(ctx, `UPDATE image_scans SET status = 'queued' WHERE status = 'running' AND source <> 'deploy'`)
	_, _ = a.Pool.Exec(ctx, `UPDATE image_scans SET status = 'error', error = 'interrupted by agent restart' WHERE status = 'running'`)
	tick := time.NewTicker(time.Minute)
	daily := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	defer daily.Stop()
	for {
		a.processScans(ctx)
		select {
		case <-ctx.Done():
			return
		case <-a.ScanWake:
		case <-tick.C:
		case <-daily.C:
			// New vulnerabilities are published every day: rescan what is running.
			if _, err := a.Pool.Exec(ctx, `
				INSERT INTO image_scans (org_id, image, source)
				SELECT DISTINCT d.org_id, d.spec->>'image', 'rescan' FROM deployments d WHERE d.status = 'live'`); err != nil {
				a.Log.Error("queue rescans", "err", err)
			}
		}
	}
}

func (a *Worker) processScans(ctx context.Context) {
	for ctx.Err() == nil {
		var id, image, slug string
		found := false
		err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `
				SELECT s.id, s.image, o.slug FROM image_scans s JOIN orgs o ON o.id = s.org_id
				WHERE s.status = 'queued' ORDER BY s.created_at LIMIT 1
				FOR UPDATE OF s SKIP LOCKED`).Scan(&id, &image, &slug)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			_, err = tx.Exec(ctx, `UPDATE image_scans SET status = 'running' WHERE id = $1`, id)
			return err
		})
		if err != nil {
			a.Log.Error("claim scan", "err", err)
			return
		}
		if !found {
			return
		}
		a.Log.Info("scanning image", "scan", id, "image", image)
		res, serr := a.RunTrivy(ctx, image, slug)
		a.StoreScan(ctx, id, res, serr)
		if serr == nil && res.Critical > 0 {
			a.alertRunningImage(ctx, image, res.Critical)
		}
	}
}

func (a *Worker) PokeScans() {
	select {
	case a.ScanWake <- struct{}{}:
	default:
	}
}

// alertRunningImage notifies every service currently running image about critical CVEs.
func (a *Worker) alertRunningImage(ctx context.Context, image string, critical int) {
	rows, err := a.Pool.Query(ctx, `
		SELECT d.org_id, d.service_id FROM deployments d
		WHERE d.status = 'live' AND d.spec->>'image' = $1`, image)
	if err != nil {
		return
	}
	type svc struct{ OrgID, ServiceID string }
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[svc])
	if err != nil {
		return
	}
	for _, s := range list {
		a.AlertEvent(ctx, s.OrgID, s.ServiceID, "critical_vulns",
			fmt.Sprintf("%d critical vulnerabilities found in the running image %s. Rebuild on an updated base image and redeploy.", critical, image))
	}
}
