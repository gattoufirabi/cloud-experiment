package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"platform/internal/core/worker"
	"platform/internal/lib/docker"
)

func NewLogShipper(dk *docker.Client, url string, log *slog.Logger) *worker.LogShipper {
	return &worker.LogShipper{
		Docker: dk, URL: url, Log: log,
		HTTP:   &http.Client{Timeout: 10 * time.Second},
		Start:  time.Now().Unix(),
		Ch:     make(chan worker.LogLine, 4096),
		Active: map[string]worker.Follower{},
		Since:  map[string]int64{},
	}
}

// ---------- metrics: container stats -> VictoriaMetrics ----------

func (a *Worker) MetricsLoop(ctx context.Context) {
	if a.Cfg.VictoriaMetricsURL == "" {
		return
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var buf bytes.Buffer
		now := time.Now().UnixMilli()
		for _, t := range a.CurrentTargets() {
			st, err := a.Docker.ContainerStats(ctx, t.ContainerID)
			if err != nil {
				continue
			}
			mem := st.MemoryStats.Usage
			if inactive := st.MemoryStats.Stats["inactive_file"]; inactive < mem {
				mem -= inactive
			}
			var rx, tx uint64
			for _, n := range st.Networks {
				rx += n.RxBytes
				tx += n.TxBytes
			}
			inst := t.Instance
			if inst == "" {
				inst = "0"
			}
			// deployment_id keeps the old and new containers of a redeploy in separate series
			// (same instance number): mixed together their counters look like resets.
			l := fmt.Sprintf(`{org_id="%s",project_id="%s",service_id="%s",instance="%s",deployment_id="%s"}`, t.OrgID, t.ProjectID, t.ServiceID, inst, t.DeploymentID)
			fmt.Fprintf(&buf, "platform_container_cpu_seconds_total%s %f %d\n", l, float64(st.CPUStats.CPUUsage.TotalUsage)/1e9, now)
			fmt.Fprintf(&buf, "platform_container_memory_bytes%s %d %d\n", l, mem, now)
			fmt.Fprintf(&buf, "platform_container_memory_limit_bytes%s %d %d\n", l, st.MemoryStats.Limit, now)
			fmt.Fprintf(&buf, "platform_container_network_rx_bytes_total%s %d %d\n", l, rx, now)
			fmt.Fprintf(&buf, "platform_container_network_tx_bytes_total%s %d %d\n", l, tx, now)
		}
		if buf.Len() == 0 {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Cfg.VictoriaMetricsURL+"/api/v1/import/prometheus", &buf)
		if err != nil {
			continue
		}
		resp, err := hc.Do(req)
		if err != nil {
			a.Log.Warn("push metrics", "err", err)
			continue
		}
		resp.Body.Close()
	}
}
