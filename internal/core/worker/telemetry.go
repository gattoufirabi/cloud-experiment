package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"platform/internal/lib/docker"
	"platform/internal/lib/kube"
)

// ---------- logs: container stdout/stderr -> VictoriaLogs ----------

type LogLine struct {
	t      target
	time   time.Time
	msg    string
	stream string
}

type Follower struct {
	cancel context.CancelFunc
	gen    int
}

type LogShipper struct {
	Docker *docker.Client
	Kube   *kube.Client // pods of autoscaled services on Kubernetes (nil = none)
	URL    string
	HTTP   *http.Client
	Log    *slog.Logger
	Start  int64
	Ch     chan LogLine
	mu     sync.Mutex
	Active map[string]Follower
	Since  map[string]int64
	gen    int
}

// ensure follows exactly the given containers.
func (s *LogShipper) ensure(ctx context.Context, targets []target) {
	if s.URL == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, t := range targets {
		want[t.ContainerID] = true
		if _, ok := s.Active[t.ContainerID]; ok {
			continue
		}
		s.gen++
		fctx, cancel := context.WithCancel(ctx)
		s.Active[t.ContainerID] = Follower{cancel: cancel, gen: s.gen}
		go s.follow(fctx, t, s.gen)
	}
	for id, f := range s.Active {
		if !want[id] {
			f.cancel()
			delete(s.Active, id)
			delete(s.Since, id)
		}
	}
}

func (s *LogShipper) follow(ctx context.Context, t target, gen int) {
	defer func() {
		s.mu.Lock()
		if f, ok := s.Active[t.ContainerID]; ok && f.gen == gen {
			delete(s.Active, t.ContainerID) // the next reconcile restarts it if still running
		}
		s.mu.Unlock()
	}()
	s.mu.Lock()
	since, ok := s.Since[t.ContainerID]
	s.mu.Unlock()
	if !ok {
		since = s.Start
	}
	if ns, pod, ok := ParseKubeInstance(t.ContainerID); ok {
		s.followPod(ctx, t, ns, pod, since)
		return
	}
	rc, err := s.Docker.ContainerLogs(ctx, t.ContainerID, since, true, "")
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
		s.Since[t.ContainerID] = tm.Unix() + 1
		s.mu.Unlock()
		name := "stdout"
		if stream == 2 {
			name = "stderr"
		}
		select {
		case s.Ch <- LogLine{t: t, time: tm, msg: msg, stream: name}:
		case <-ctx.Done():
		}
	})
}

// followPod ships a pod's log (Kubernetes merges stdout and stderr: all lines are "stdout").
func (s *LogShipper) followPod(ctx context.Context, t target, ns, pod string, since int64) {
	if s.Kube == nil {
		return
	}
	rc, err := s.Kube.Logs(ctx, ns, pod, time.Unix(since, 0), true, 0)
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
		s.Since[t.ContainerID] = tm.Unix() + 1
		s.mu.Unlock()
		select {
		case s.Ch <- LogLine{t: t, time: tm, msg: msg, stream: "stdout"}:
		case <-ctx.Done():
			return
		}
	}
}

func (s *LogShipper) Run(ctx context.Context) {
	if s.URL == "" {
		return
	}
	batch := make([]LogLine, 0, 500)
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
		case l := <-s.Ch:
			if batch = append(batch, l); len(batch) >= 500 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (s *LogShipper) push(ctx context.Context, lines []LogLine) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		_ = enc.Encode(map[string]string{
			"_time": l.time.UTC().Format(time.RFC3339Nano), "_msg": l.msg, "stream": l.stream,
			"org_id": l.t.OrgID, "project_id": l.t.ProjectID, "service_id": l.t.ServiceID, "deployment_id": l.t.DeploymentID,
		})
	}
	u := s.URL + "/insert/jsonline?_stream_fields=org_id,project_id,service_id&_msg_field=_msg&_time_field=_time"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/stream+json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		s.Log.Warn("ship logs", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.Log.Warn("ship logs", "status", resp.StatusCode)
	}
}
