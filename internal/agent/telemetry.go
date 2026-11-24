package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"platform/internal/docker"
	"platform/internal/kube"
)

// ---------- logs: container stdout/stderr -> VictoriaLogs ----------

type logLine struct {
	t      target
	time   time.Time
	msg    string
	stream string
}

type follower struct {
	cancel context.CancelFunc
	gen    int
}

type logShipper struct {
	dk     *docker.Client
	kube   *kube.Client // pods of autoscaled services on Kubernetes (nil = none)
	url    string
	hc     *http.Client
	log    *slog.Logger
	start  int64
	ch     chan logLine
	mu     sync.Mutex
	active map[string]follower
	since  map[string]int64
	gen    int
}

func newLogShipper(dk *docker.Client, url string, log *slog.Logger) *logShipper {
	return &logShipper{
		dk: dk, url: url, log: log,
		hc:     &http.Client{Timeout: 10 * time.Second},
		start:  time.Now().Unix(),
		ch:     make(chan logLine, 4096),
		active: map[string]follower{},
		since:  map[string]int64{},
	}
}

// ensure follows exactly the given containers.
func (s *logShipper) ensure(ctx context.Context, targets []target) {
	if s.url == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, t := range targets {
		want[t.ContainerID] = true
		if _, ok := s.active[t.ContainerID]; ok {
			continue
		}
		s.gen++
		fctx, cancel := context.WithCancel(ctx)
		s.active[t.ContainerID] = follower{cancel: cancel, gen: s.gen}
		go s.follow(fctx, t, s.gen)
	}
	for id, f := range s.active {
		if !want[id] {
			f.cancel()
			delete(s.active, id)
			delete(s.since, id)
		}
	}
}

func (s *logShipper) follow(ctx context.Context, t target, gen int) {
	defer func() {
		s.mu.Lock()
		if f, ok := s.active[t.ContainerID]; ok && f.gen == gen {
			delete(s.active, t.ContainerID) // the next reconcile restarts it if still running
		}
		s.mu.Unlock()
	}()
	s.mu.Lock()
	since, ok := s.since[t.ContainerID]
	s.mu.Unlock()
	if !ok {
		since = s.start
	}
	if ns, pod, ok := parseKubeInstance(t.ContainerID); ok {
		s.followPod(ctx, t, ns, pod, since)
		return
	}
	rc, err := s.dk.ContainerLogs(ctx, t.ContainerID, since, true, "")
	if err != nil {
		return
	}
	defer rc.Close()
	_ = docker.Demux(rc, func(stream byte, line string) {
		ts, msg, found := strings.Cut(line, " ")
		tm, err := time.Parse(time.RFC3339Nano, ts)
		if !found || err != nil {
			tm, msg = time.Now(), line
		}
		if strings.TrimSpace(msg) == "" {
			return
		}
		s.mu.Lock()
		s.since[t.ContainerID] = tm.Unix() + 1
		s.mu.Unlock()
		name := "stdout"
		if stream == 2 {
			name = "stderr"
		}
		select {
		case s.ch <- logLine{t: t, time: tm, msg: msg, stream: name}:
		case <-ctx.Done():
		}
	})
}

// followPod ships a pod's log (Kubernetes merges stdout and stderr: all lines are "stdout").
func (s *logShipper) followPod(ctx context.Context, t target, ns, pod string, since int64) {
	if s.kube == nil {
		return
	}
	rc, err := s.kube.Logs(ctx, ns, pod, time.Unix(since, 0), true, 0)
	if err != nil {
		return
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		ts, msg, found := strings.Cut(sc.Text(), " ")
		tm, err := time.Parse(time.RFC3339Nano, ts)
		if !found || err != nil {
			tm, msg = time.Now(), sc.Text()
		}
		if strings.TrimSpace(msg) == "" {
			continue
		}
		s.mu.Lock()
		s.since[t.ContainerID] = tm.Unix() + 1
		s.mu.Unlock()
		select {
		case s.ch <- logLine{t: t, time: tm, msg: msg, stream: "stdout"}:
		case <-ctx.Done():
			return
		}
	}
}

func (s *logShipper) run(ctx context.Context) {
	if s.url == "" {
		return
	}
	batch := make([]logLine, 0, 500)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	flush := func() {
		if len(batch) > 0 {
			s.push(ctx, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-s.ch:
			if batch = append(batch, l); len(batch) >= 500 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (s *logShipper) push(ctx context.Context, lines []logLine) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		_ = enc.Encode(map[string]string{
			"_time": l.time.UTC().Format(time.RFC3339Nano), "_msg": l.msg, "stream": l.stream,
			"org_id": l.t.OrgID, "project_id": l.t.ProjectID, "service_id": l.t.ServiceID, "deployment_id": l.t.DeploymentID,
		})
	}
	u := s.url + "/insert/jsonline?_stream_fields=org_id,project_id,service_id&_msg_field=_msg&_time_field=_time"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/stream+json")
	resp, err := s.hc.Do(req)
	if err != nil {
		s.log.Warn("ship logs", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.log.Warn("ship logs", "status", resp.StatusCode)
	}
}

// ---------- metrics: container stats -> VictoriaMetrics ----------

func (a *Agent) metricsLoop(ctx context.Context) {
	if a.cfg.VictoriaMetricsURL == "" {
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
		for _, t := range a.currentTargets() {
			st, err := a.dk.ContainerStats(ctx, t.ContainerID)
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
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.VictoriaMetricsURL+"/api/v1/import/prometheus", &buf)
		if err != nil {
			continue
		}
		resp, err := hc.Do(req)
		if err != nil {
			a.log.Warn("push metrics", "err", err)
			continue
		}
		resp.Body.Close()
	}
}
