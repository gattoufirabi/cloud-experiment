package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/worker"
	"platform/internal/lib/model"
)

// Moving a project that keeps data to another node (see migration 032). The source
// agent stops the project's containers, snapshots each data volume (service folders,
// database data) into the organization's backup repository and hands the project over;
// the target agent restores the volumes, then the normal reconcile starts everything.
// mineSQL leaves a moving project to no node, so nothing restarts it meanwhile.

const moveMaxAttempts = 5

func (a *Worker) PokeMoves() {
	select {
	case a.MoveWake <- struct{}{}:
	default:
	}
}

func (a *Worker) MoveLoop(ctx context.Context) {
	if !a.Cfg.BackupsEnabled {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		a.runMoves(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.MoveWake:
		}
	}
}

type moveVolume struct {
	Volume   string `json:"volume"`
	Host     string `json:"host"` // restic host of the move snapshot (never a user backup's)
	Snapshot string `json:"snapshot"`
}

type projectMove struct {
	ID, ProjectID, OrgID, FromNode, ToNode, Status string
	Volumes                                        []moveVolume
	Attempts                                       int
}

func (a *Worker) runMoves(ctx context.Context) {
	rows, err := a.Pool.Query(ctx, `
		SELECT id, project_id, org_id, from_node, to_node, status, volumes, attempts FROM project_moves
		WHERE (status = 'snapshot' AND from_node = $1) OR (status = 'restore' AND to_node = $1) ORDER BY created_at`, a.SelfID)
	if err != nil {
		return
	}
	moves, err := pgx.CollectRows(rows, pgx.RowToStructByPos[projectMove])
	if err != nil {
		return
	}
	for _, m := range moves {
		var err error
		if m.Status == "snapshot" {
			err = a.moveSnapshot(ctx, m)
		} else {
			err = a.moveRestore(ctx, m)
		}
		if err != nil {
			a.Log.Warn("project move", "project", m.ProjectID, "step", m.Status, "err", err)
			a.moveFailed(ctx, m, err)
		}
	}
}

func (a *Worker) setMove(ctx context.Context, id, status, msg string) {
	_, _ = a.Pool.Exec(ctx, `UPDATE project_moves SET status = $2, message = $3, updated_at = now() WHERE id = $1`, id, status, msg)
}

// moveFailed retries a step a few times, then gives the project back to its source.
func (a *Worker) moveFailed(ctx context.Context, m projectMove, cause error) {
	if m.Attempts+1 < moveMaxAttempts {
		_, _ = a.Pool.Exec(ctx, `UPDATE project_moves SET attempts = attempts + 1, message = $2, updated_at = now() WHERE id = $1`,
			m.ID, worker.Truncate("retrying: "+cause.Error(), 400))
		return
	}
	_ = pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE project_moves SET status = 'failed', message = $2, updated_at = now() WHERE id = $1`,
			m.ID, worker.Truncate(cause.Error(), 400)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE projects SET node_id = $2 WHERE id = $1`, m.ProjectID, m.FromNode)
		return err
	})
	_ = worker.AuditAgent(ctx, a.Agent, m.OrgID, "project.move_failed", "project:"+m.ProjectID, map[string]string{"from": m.FromNode, "to": m.ToNode, "error": worker.Truncate(cause.Error(), 200)})
	_, _ = a.Pool.Exec(ctx, `SELECT pg_notify('deployments', '')`)
}

// projectVolumes lists the project's data volumes: service folders and database data.
func (a *Worker) projectVolumes(ctx context.Context, projectID string) ([]moveVolume, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT 's', id FROM services WHERE project_id = $1 AND volume_path <> ''
		UNION ALL SELECT 'd', id FROM databases WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []moveVolume
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, err
		}
		v := moveVolume{Volume: worker.VolumeName(id), Host: "move-" + model.Compact(id)}
		if kind == "d" {
			v.Volume = worker.DBDataVolume(id)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (a *Worker) moveSnapshot(ctx context.Context, m projectMove) error {
	// Stop everything of the project on this node (databases first flush their data).
	cs, err := a.Docker.ContainerList(ctx, "platform.project="+m.ProjectID)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if a.Owns(c.Labels) && c.State == "running" {
			if err := a.Docker.ContainerStop(ctx, c.ID, 30); err != nil {
				return fmt.Errorf("stop %s: %w", strings.Join(c.Names, ","), err)
			}
		}
	}
	a.setMove(ctx, m.ID, "snapshot", "project stopped; copying its data")
	vols, err := a.projectVolumes(ctx, m.ProjectID)
	if err != nil {
		return err
	}
	for i, v := range vols {
		if ok, err := a.Docker.VolumeExists(ctx, v.Volume); err != nil {
			return err
		} else if !ok {
			continue // never started: nothing to copy
		}
		script := `set -e
restic cat config >/dev/null 2>&1 || restic init >/dev/null
restic backup /data --host "$SVC" --tag move --json --quiet | grep '"summary"'`
		out, err := a.ResticRun(ctx, m.OrgID, v.Host, v.Volume, script, false)
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", v.Volume, err)
		}
		var summary struct {
			SnapshotID string `json:"snapshot_id"`
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, `"summary"`) {
				_ = json.Unmarshal([]byte(line), &summary)
			}
		}
		if summary.SnapshotID == "" {
			return fmt.Errorf("snapshot %s: no snapshot ID in restic output", v.Volume)
		}
		vols[i].Snapshot = summary.SnapshotID
	}
	raw, _ := json.Marshal(vols)
	err = pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE project_moves SET status = 'restore', volumes = $2, attempts = 0, message = 'data copied; restoring on the new node', updated_at = now() WHERE id = $1`,
			m.ID, raw); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE projects SET node_id = $2 WHERE id = $1`, m.ProjectID, m.ToNode)
		return err
	})
	if err != nil {
		return err
	}
	a.Log.Info("project data copied for move", "project", m.ProjectID, "to", m.ToNode, "volumes", len(vols))
	_, _ = a.Pool.Exec(ctx, `SELECT pg_notify('deployments', 'move')`)
	return nil
}

func (a *Worker) moveRestore(ctx context.Context, m projectMove) error {
	for _, v := range m.Volumes {
		if v.Snapshot == "" {
			continue
		}
		if !worker.SnapshotRe.MatchString(v.Snapshot) {
			return errors.New("invalid snapshot ID")
		}
		script := `set -e
find /data -mindepth 1 -delete
restic restore "$SNAP" --host "$SVC" --target /`
		if _, err := a.ResticRun(ctx, m.OrgID, v.Host, v.Volume, script, true, "SNAP="+v.Snapshot); err != nil {
			return fmt.Errorf("restore %s: %w", v.Volume, err)
		}
	}
	a.setMove(ctx, m.ID, "done", fmt.Sprintf("moved to %s with %d data volume(s)", m.ToNode, len(m.Volumes)))
	_ = worker.AuditAgent(ctx, a.Agent, m.OrgID, "project.moved", "project:"+m.ProjectID, map[string]string{"from": m.FromNode, "to": m.ToNode})
	a.Log.Info("project moved", "project", m.ProjectID, "from", m.FromNode, "to", m.ToNode)
	_, _ = a.Pool.Exec(ctx, `SELECT pg_notify('deployments', '')`) // the reconcile now starts it here
	return nil
}
