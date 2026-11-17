package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/model"
)

// Autoscaling: every minute the primary compares each autoscaled service's CPU (the
// average over its instances, last 2 minutes) with its target and changes the number of
// instances with a new deployment of the live settings. Up: at once (one minute apart
// at least). Down: after 5 minutes without scaling, and only when well below target.

const (
	scaleUpEvery   = time.Minute
	scaleDownAfter = 5 * time.Minute
)

// desiredInstances is the target-tracking formula: instances needed so that the
// average CPU comes back to the target, kept between min and max.
func desiredInstances(current int, cpuPercent float64, target, minN, maxN int) int {
	if current < 1 {
		current = 1
	}
	want := int(math.Ceil(float64(current) * cpuPercent / float64(target)))
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

func (a *Agent) autoscaleLoop(ctx context.Context) {
	if !a.isPrimary() || a.cfg.VictoriaMetricsURL == "" {
		return
	}
	tick := time.NewTicker(time.Minute)
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
	Spec                                []byte
}

func (a *Agent) autoscale(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT s.id, s.org_id, p.slug, s.slug, s.cpu_millis::text, s.instances::text, s.autoscale_min, s.autoscale_max, s.autoscale_cpu, s.last_scaled_at,
		       d.id, d.spec
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
	cores := a.vmVector(ctx, `sum by (service_id) (rate(platform_container_cpu_seconds_total[2m]))`)
	for _, s := range list {
		used, ok := cores[s.ServiceID]
		if !ok {
			continue // no metrics yet
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
		case want > current && since >= scaleUpEvery:
		case want < current && since >= scaleDownAfter && cpuPercent < float64(s.Target)*0.9:
		default:
			continue
		}
		spec.Instances = want
		raw, _ := json.Marshal(spec)
		err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				INSERT INTO deployments (org_id, service_id, revision, spec)
				VALUES ($1, $2, (SELECT coalesce(max(revision), 0) + 1 FROM deployments WHERE service_id = $2), $3)`, s.OrgID, s.ServiceID, raw); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE services SET instances = $2, last_scaled_at = now() WHERE id = $1`, s.ServiceID, want)
			return err
		})
		if err != nil {
			a.log.Warn("autoscale", "service", s.ServiceID, "err", err)
			continue
		}
		_ = auditAgent(ctx, a, s.OrgID, "service.autoscaled", "service:"+s.ProjectSlug+"/"+s.Slug, map[string]string{
			"from": strconv.Itoa(current), "to": strconv.Itoa(want), "cpu_percent": fmt.Sprintf("%.0f", cpuPercent), "target": strconv.Itoa(s.Target)})
		a.log.Info("autoscaled", "service", s.ServiceID, "from", current, "to", want, "cpu_percent", int(cpuPercent))
		a.poke()
	}
}
