package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/worker"
	"platform/internal/lib/model"
)

// Autoscaling: every 15 s the primary compares each autoscaled service's CPU (the
// average over the containers of its live deployment, last minute) with its target and
// changes the number of instances with a new deployment of the live settings.
//
//   - Up: as soon as the live deployment has a few metric samples (35 s after it went
//     live). A container cannot use more than its CPU limit, so near the limit the real
//     demand is unknown: the count then at least doubles instead of creeping up by one.
//   - Down: after 5 minutes without scaling, and only when well below target.
//
// Only the live deployment's containers are measured (deployment_id label): during a
// switch the old containers still run and would make the CPU look higher.

const (
	autoscaleEvery   = 15 * time.Second
	settleAfterLive  = 35 * time.Second // metrics are pushed every 15 s: 2-3 samples
	scaleDownAfter   = 5 * time.Minute
	saturatedPercent = 90.0 // of the CPU limit
)

// desiredInstances is the target-tracking formula: instances needed so that the
// average CPU comes back to the target, kept between min and max. At the CPU limit the
// measure hides how much more is needed, so the count at least doubles.
func desiredInstances(current int, cpuPercent float64, target, minN, maxN int) int {
	if current < 1 {
		current = 1
	}
	want := int(math.Ceil(float64(current) * cpuPercent / float64(target)))
	if cpuPercent >= saturatedPercent && want < 2*current {
		want = 2 * current
	}
	if want < minN {
		want = minN
	}
	if want > maxN {
		want = maxN
	}
	if want < 1 {
		want = 1
	}
	return want
}

func (a *Worker) AutoscaleLoop(ctx context.Context) {
	if !a.IsPrimary() || a.Cfg.VictoriaMetricsURL == "" {
		return
	}
	tick := time.NewTicker(autoscaleEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.autoscale(ctx)
		}
	}
}

type scaled struct {
	ServiceID, OrgID, ProjectSlug, Slug string
	CPUMillis, Instances                string
	Min, Max, Target                    int
	LastScaled                          *time.Time
	DeploymentID                        string
	LiveSince                           time.Time
	Spec                                []byte
}

func (a *Worker) autoscale(ctx context.Context) {
	rows, err := a.Pool.Query(ctx, `
		SELECT s.id, s.org_id, p.slug, s.slug, s.cpu_millis::text, s.instances::text, s.autoscale_min, s.autoscale_max, s.autoscale_cpu, s.last_scaled_at,
		       d.id, d.updated_at, d.spec
		FROM services s JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id
		JOIN deployments d ON d.service_id = s.id AND d.status = 'live'
		WHERE s.autoscale_min > 0 AND NOT o.suspended AND s.preview_of IS NULL
		  AND NOT EXISTS (SELECT 1 FROM deployments x WHERE x.service_id = s.id AND x.status IN ('pending', 'deploying'))`)
	if err != nil {
		return
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[scaled])
	if err != nil || len(list) == 0 {
		return
	}
	cores := a.VmVectorBy(ctx, `sum by (deployment_id) (rate(platform_container_cpu_seconds_total[1m]))`, "deployment_id")
	for _, s := range list {
		used, ok := cores[s.DeploymentID]
		if !ok || time.Since(s.LiveSince) < settleAfterLive {
			continue // no metrics yet for the live containers
		}
		var spec model.Spec
		if json.Unmarshal(s.Spec, &spec) != nil {
			continue
		}
		current := spec.Count()
		if spec.VolumePath != "" || len(spec.Ports) > 0 || spec.CPUMillis <= 0 {
			continue
		}
		cpuPercent := used / (float64(current) * float64(spec.CPUMillis) / 1000) * 100
		want := desiredInstances(current, cpuPercent, s.Target, s.Min, s.Max)
		since := time.Duration(math.MaxInt64)
		if s.LastScaled != nil {
			since = time.Since(*s.LastScaled)
		}
		switch {
		case want > current:
		case want < current && since >= scaleDownAfter && cpuPercent < float64(s.Target)*0.9:
		default:
			continue
		}
		spec.Instances = want
		raw, _ := json.Marshal(spec)
		err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				INSERT INTO deployments (org_id, service_id, revision, spec)
				VALUES ($1, $2, (SELECT coalesce(max(revision), 0) + 1 FROM deployments WHERE service_id = $2), $3)`, s.OrgID, s.ServiceID, raw); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE services SET instances = $2, last_scaled_at = now() WHERE id = $1`, s.ServiceID, want)
			return err
		})
		if err != nil {
			a.Log.Warn("autoscale", "service", s.ServiceID, "err", err)
			continue
		}
		_ = worker.AuditAgent(ctx, a.Agent, s.OrgID, "service.autoscaled", "service:"+s.ProjectSlug+"/"+s.Slug, map[string]string{
			"from": strconv.Itoa(current), "to": strconv.Itoa(want), "cpu_percent": fmt.Sprintf("%.0f", cpuPercent), "target": strconv.Itoa(s.Target)})
		a.Log.Info("autoscaled", "service", s.ServiceID, "from", current, "to", want, "cpu_percent", int(cpuPercent))
		a.Poke()
	}
}
