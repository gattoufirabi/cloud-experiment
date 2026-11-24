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
	"platform/internal/lib/docker"
	"platform/internal/lib/model"
)

// Scheduled jobs: a command run on a cron schedule (or on demand) in a fresh container
// made from the service's current deployment, with the same image, environment,
// secrets, project variables, network and resource limits. Each node runs the jobs of
// the projects placed on it. A job never overlaps itself: a run due while the previous
// one is still going is recorded as skipped.

const (
	maxJobsPerNode = 4
	jobLogBytes    = 64 << 10
)

func (a *Worker) PokeJobs() {
	select {
	case a.JobWake <- struct{}{}:
	default:
	}
}

func (a *Worker) JobLoop(ctx context.Context) {
	// Runs left "running" by a previous agent on this node cannot be followed any more.
	if rows, err := a.Pool.Query(ctx, `
		UPDATE job_runs SET status = 'failed', finished_at = now(), log = log || E'\n==> interrupted: the agent restarted'
		WHERE status = 'running' AND node_id = $1 RETURNING coalesce(container_id, '')`, a.SelfID); err == nil {
		ids, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, id := range ids {
			if id != "" {
				_ = a.Docker.ContainerRemove(context.Background(), id)
			}
		}
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	var lastPrune time.Time
	for {
		a.scheduleJobs(ctx)
		a.startDueJobs(ctx)
		if a.IsPrimary() && time.Since(lastPrune) > time.Hour { // keep the last 50 runs of each job
			lastPrune = time.Now()
			_, _ = a.Pool.Exec(ctx, `
				DELETE FROM job_runs WHERE id IN (SELECT id FROM (
				    SELECT id, row_number() OVER (PARTITION BY job_id ORDER BY started_at DESC) AS n FROM job_runs WHERE status <> 'running') x
				WHERE n > 50)`)
		}
		select {
		case <-ctx.Done():
			return
		case <-a.JobWake:
		case <-tick.C:
		}
	}
}

// scheduleJobs gives a next run time to this node's enabled jobs that have none (new,
// re-enabled, or schedule changed).
func (a *Worker) scheduleJobs(ctx context.Context) {
	rows, err := a.Pool.Query(ctx, `
		SELECT j.id, j.schedule FROM jobs j JOIN services s ON s.id = j.service_id JOIN projects p ON p.id = s.project_id
		WHERE j.enabled AND j.next_run_at IS NULL AND `+worker.MineSQL, a.MineArgs()...)
	if err != nil {
		return
	}
	type pending struct{ id, schedule string }
	var list []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.schedule) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		if next, ok := worker.NextRun(p.schedule); ok {
			_, _ = a.Pool.Exec(ctx, `UPDATE jobs SET next_run_at = $2 WHERE id = $1 AND next_run_at IS NULL`, p.id, next)
		}
	}
}

type dueJob struct {
	ID, OrgID, ServiceID, ProjectID, Slug, Command, Schedule string
	Timeout                                                  int
	Manual, Overlap                                          bool
}

// startDueJobs claims due jobs (schedule reached or "Run now") and starts them.
func (a *Worker) startDueJobs(ctx context.Context) {
	for ctx.Err() == nil {
		var running int
		if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE status = 'running' AND node_id = $1`, a.SelfID).Scan(&running); err != nil || running >= maxJobsPerNode {
			return
		}
		var d dueJob
		found := false
		err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `
				SELECT j.id, j.org_id, j.service_id, s.project_id, j.slug, j.command, j.schedule, j.timeout_seconds, j.run_requested,
				       EXISTS (SELECT 1 FROM job_runs r WHERE r.job_id = j.id AND r.status = 'running')
				FROM jobs j JOIN services s ON s.id = j.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = j.org_id
				WHERE (j.run_requested OR (j.enabled AND j.next_run_at <= now())) AND NOT o.suspended AND `+worker.MineSQL+`
				ORDER BY j.run_requested DESC, j.next_run_at
				LIMIT 1 FOR UPDATE OF j SKIP LOCKED`, a.MineArgs()...).
				Scan(&d.ID, &d.OrgID, &d.ServiceID, &d.ProjectID, &d.Slug, &d.Command, &d.Schedule, &d.Timeout, &d.Manual, &d.Overlap)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			var next any // the next scheduled run, unchanged by a manual run
			if !d.Manual {
				if n, ok := worker.NextRun(d.Schedule); ok {
					next = n
				}
				_, err = tx.Exec(ctx, `UPDATE jobs SET next_run_at = $2 WHERE id = $1`, d.ID, next)
			} else {
				_, err = tx.Exec(ctx, `UPDATE jobs SET run_requested = false WHERE id = $1`, d.ID)
			}
			return err
		})
		if err != nil {
			a.Log.Warn("claim job", "err", err)
			return
		}
		if !found {
			return
		}
		trigger := "schedule"
		if d.Manual {
			trigger = "manual"
		}
		if d.Overlap {
			_, _ = a.Pool.Exec(ctx, `
				INSERT INTO job_runs (org_id, job_id, trigger, status, log, node_id, finished_at)
				VALUES ($1, $2, $3, 'skipped', 'the previous run is still running', $4, now())`, d.OrgID, d.ID, trigger, a.SelfID)
			continue
		}
		var runID string
		if err := a.Pool.QueryRow(ctx, `
			INSERT INTO job_runs (org_id, job_id, trigger, node_id) VALUES ($1, $2, $3, $4) RETURNING id`,
			d.OrgID, d.ID, trigger, a.SelfID).Scan(&runID); err != nil {
			a.Log.Warn("record job run", "err", err)
			return
		}
		go a.runJob(context.WithoutCancel(ctx), d, runID)
	}
}

func (a *Worker) runJob(ctx context.Context, d dueJob, runID string) {
	finish := func(status string, code *int, logText string) {
		if len(logText) > jobLogBytes {
			logText = "…" + logText[len(logText)-jobLogBytes:]
		}
		_, _ = a.Pool.Exec(ctx, `UPDATE job_runs SET status = $2, exit_code = $3, log = $4, finished_at = now() WHERE id = $1`,
			runID, status, code, strings.ToValidUTF8(logText, "?"))
		if status != "succeeded" {
			what := "failed"
			switch {
			case status == "timed_out":
				what = fmt.Sprintf("timed out after %d s", d.Timeout)
			case code != nil:
				what = fmt.Sprintf("failed with exit code %d", *code)
			}
			a.AlertEvent(ctx, d.OrgID, d.ServiceID, "job_failed", fmt.Sprintf("job %s %s", d.Slug, what))
		}
	}

	// The service's current deployment gives the image and settings.
	var raw []byte
	err := a.Pool.QueryRow(ctx, `SELECT spec FROM deployments WHERE service_id = $1 AND status = 'live' ORDER BY revision DESC LIMIT 1`, d.ServiceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		finish("failed", nil, "==> failed: the service has no running deployment; deploy it first")
		return
	}
	var spec model.Spec
	if err == nil {
		err = json.Unmarshal(raw, &spec)
	}
	if err != nil {
		finish("failed", nil, "==> failed: "+err.Error())
		return
	}
	spec.Worker = true   // nothing to publish or probe
	spec.VolumePath = "" // the service's folder stays with the service
	spec.Instances = 1   // one container per run
	j := worker.Job{ID: runID, OrgID: d.OrgID, ServiceID: d.ServiceID, ProjectID: d.ProjectID, Spec: spec}
	netName, err := a.EnsureNetwork(ctx, j)
	if err != nil {
		finish("failed", nil, "==> failed: "+err.Error())
		return
	}
	env, err := a.BuildEnv(ctx, j)
	if err != nil {
		finish("failed", nil, "==> failed: "+err.Error())
		return
	}
	env = append(env, "PLATFORM_JOB="+d.Slug, "PLATFORM_JOB_RUN="+runID)
	cfg := a.ContainerConfig(j, netName, env, a.ProjectHosts(ctx, d.ProjectID, netName))
	// The command replaces the image's entrypoint (an empty one would be ignored).
	cfg.Entrypoint = []string{"/bin/sh", "-c"}
	cfg.Cmd = []string{d.Command}
	cfg.HostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	delete(cfg.Labels, "platform.deployment")
	cfg.Labels["platform.job"] = d.ID
	cfg.Labels["platform.job_run"] = runID
	_, _ = a.Pool.Exec(ctx, `UPDATE job_runs SET cpu_millis = $2, memory_mb = $3 WHERE id = $1`, runID, spec.CPUMillis, spec.MemoryMB)

	name := "pj-" + model.Compact(d.ID)[:12] + "-" + model.Compact(runID)[:8]
	header := fmt.Sprintf("==> %s: %s\n", d.Slug, d.Command)
	id, code, out, err := a.RunOnce(ctx, name, cfg, time.Duration(d.Timeout)*time.Second, func(o string) {
		_, _ = a.Pool.Exec(ctx, `UPDATE job_runs SET log = $2 WHERE id = $1 AND status = 'running'`, runID, strings.ToValidUTF8(header+o, "?"))
	})
	if id != "" {
		_, _ = a.Pool.Exec(ctx, `UPDATE job_runs SET container_id = $2 WHERE id = $1`, runID, id)
		defer a.Docker.ContainerRemove(context.Background(), id)
	}
	switch {
	case err != nil && strings.HasPrefix(err.Error(), "time limit"):
		finish("timed_out", nil, header+out+fmt.Sprintf("\n==> stopped: still running after %d s", d.Timeout))
	case err != nil:
		finish("failed", nil, header+out+"\n==> failed: "+err.Error())
	case code != 0:
		finish("failed", &code, header+out+fmt.Sprintf("\n==> exit code %d", code))
	default:
		finish("succeeded", &code, header+out+"\n==> done")
	}
}
