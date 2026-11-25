package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/worker"
	"platform/internal/lib/cron"
	"platform/internal/lib/model"
)

// Scheduled scaling and node failover, both run every minute by the primary.

func (a *Worker) ScalingLoop(ctx context.Context) {
	if !a.IsPrimary() {
		return
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.applySchedules(ctx, time.Now().UTC())
			a.failover(ctx)
		}
	}
}

type scheduleRow struct {
	ID, Cron                            string
	Instances                           int
	LastRun                             *time.Time
	Created                             time.Time
	ServiceID, OrgID, ProjectSlug, Slug string
	AutoMin, AutoMax                    int
	Spec                                []byte
	Busy                                bool
}

// scheduleDue reports whether the cron expression fired after `from` and up to now.
func scheduleDue(sched *cron.Schedule, from, now time.Time) bool {
	return !sched.Next(from).After(now)
}

func (a *Worker) applySchedules(ctx context.Context, now time.Time) {
	rows, err := a.Pool.Query(ctx, `
		SELECT sc.id, sc.cron, sc.instances, sc.last_run, sc.created_at, s.id, s.org_id, p.slug, s.slug, s.autoscale_min, s.autoscale_max, d.spec,
		       EXISTS (SELECT 1 FROM deployments x WHERE x.service_id = s.id AND x.status IN ('pending', 'deploying'))
		FROM scaling_schedules sc JOIN services s ON s.id = sc.service_id JOIN projects p ON p.id = s.project_id
		JOIN orgs o ON o.id = s.org_id JOIN deployments d ON d.service_id = s.id AND d.status = 'live'
		WHERE NOT o.suspended AND s.preview_of IS NULL
		ORDER BY sc.created_at`)
	if err != nil {
		return
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[scheduleRow])
	if err != nil {
		return
	}
	done := map[string]bool{} // one change per service per minute (the first due schedule wins)
	for _, sc := range list {
		sched, err := cron.Parse(sc.Cron)
		if err != nil {
			continue
		}
		from := sc.Created
		if sc.LastRun != nil {
			from = *sc.LastRun
		}
		if !scheduleDue(sched, from, now) || done[sc.ServiceID] {
			continue
		}
		if sc.Busy { // a deployment is under way: try again next minute
			continue
		}
		done[sc.ServiceID] = true
		msg := a.scaleTo(ctx, sc)
		_, _ = a.Pool.Exec(ctx, `UPDATE scaling_schedules SET last_run = $2, last_error = $3 WHERE id = $1`, sc.ID, now, msg)
	}
}

// scaleTo applies one schedule; it returns an error message ("" when applied).
func (a *Worker) scaleTo(ctx context.Context, sc scheduleRow) string {
	var spec model.Spec
	if json.Unmarshal(sc.Spec, &spec) != nil {
		return "cannot read the live settings"
	}
	if spec.VolumePath != "" || len(spec.Ports) > 0 {
		return "a service with a persistent folder or public ports runs a single instance"
	}
	want, current := sc.Instances, spec.Count()
	autoscaled := sc.AutoMin > 0
	newDeploy := want != current
	if autoscaled && want < current {
		newDeploy = false // the autoscaler lowers the count itself once the minimum allows it
	}
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		if autoscaled {
			if _, err := tx.Exec(ctx, `UPDATE services SET autoscale_min = $2, autoscale_max = greatest(autoscale_max, $2), last_scaled_at = now() WHERE id = $1`, sc.ServiceID, want); err != nil {
				return err
			}
		}
		if !newDeploy {
			return nil
		}
		spec.Instances = want
		raw, _ := json.Marshal(spec)
		if _, err := tx.Exec(ctx, `
			INSERT INTO deployments (org_id, service_id, revision, spec)
			VALUES ($1, $2, (SELECT coalesce(max(revision), 0) + 1 FROM deployments WHERE service_id = $2), $3)`, sc.OrgID, sc.ServiceID, raw); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE services SET instances = $2, last_scaled_at = now() WHERE id = $1`, sc.ServiceID, want)
		return err
	})
	if err != nil {
		a.Log.Warn("scheduled scaling", "service", sc.ServiceID, "err", err)
		return "could not apply the schedule"
	}
	detail := map[string]string{"schedule": sc.Cron, "from": strconv.Itoa(current), "to": strconv.Itoa(want)}
	if autoscaled {
		detail["autoscale_min"] = strconv.Itoa(want)
	}
	_ = worker.AuditAgent(ctx, a.Agent, sc.OrgID, "service.scheduled_scaling", "service:"+sc.ProjectSlug+"/"+sc.Slug, detail)
	a.Log.Info("scheduled scaling", "service", sc.ServiceID, "from", current, "to", want, "autoscaled", autoscaled)
	if newDeploy {
		a.Poke()
	}
	return ""
}

// failover cordons a node that stopped reporting for NODE_FAILOVER_MINUTES and gives its
// stateless projects back to placement, so they are redeployed on the other nodes (their
// old containers, if the node comes back, are removed once the new ones are live).
// Projects with data (databases, persistent folders) cannot be moved without the node:
// the operator is told, and can restore them from backups.
func (a *Worker) failover(ctx context.Context) {
	if a.Cfg.NodeFailoverMinutes <= 0 {
		return
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT id FROM nodes
		WHERE id <> $1 AND role <> 'customer' AND last_seen < now() - make_interval(mins => $2)
		  AND (failed_over_at IS NULL OR failed_over_at < last_seen)
		  AND EXISTS (SELECT 1 FROM projects p WHERE p.node_id = nodes.id)`, a.SelfID, a.Cfg.NodeFailoverMinutes)
	if err != nil {
		return
	}
	dead, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return
	}
	for _, id := range dead {
		var moved, kept int
		err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `
				WITH upd AS (
				    UPDATE projects p SET node_id = NULL WHERE p.node_id = $1
				      AND NOT EXISTS (SELECT 1 FROM databases db WHERE db.project_id = p.id)
				      AND NOT EXISTS (SELECT 1 FROM services s WHERE s.project_id = p.id AND s.volume_path <> '')
				    RETURNING 1)
				SELECT count(*) FROM upd`, id).Scan(&moved); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM projects WHERE node_id = $1`, id).Scan(&kept); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE nodes SET cordoned = true, failed_over_at = now() WHERE id = $1`, id)
			return err
		})
		if err != nil {
			a.Log.Warn("node failover", "node", id, "err", err)
			continue
		}
		_ = worker.AuditAgent(ctx, a.Agent, "", "node.failed_over", "node:"+id, map[string]int{"moved": moved, "kept": kept})
		a.Log.Warn("node failed over", "node", id, "moved", moved, "kept", kept)
		msg := fmt.Sprintf("Node %s stopped reporting for %d minutes. It was cordoned and %d stateless project(s) are being redeployed on other nodes.", id, a.Cfg.NodeFailoverMinutes, moved)
		if kept > 0 {
			msg += fmt.Sprintf(" %d project(s) with data stay on it: bring the node back, or drain it and restore them from backups.", kept)
		}
		a.NotifyOperator("Node "+id+" failed over", msg)
		if moved > 0 {
			a.PlaceProjects(ctx)
			_, _ = a.Pool.Exec(ctx, `SELECT pg_notify('deployments', '')`)
		}
	}
}
