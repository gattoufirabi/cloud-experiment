// Package worker is the core of the node agent, the only part of the platform that
// touches Docker and Caddy: deployments and the reconcile loop, the proxy's routes,
// placement on nodes, managed databases, and the code several components share. It
// reads desired state from PostgreSQL and converges the host to it, enforcing a hard
// policy that does not trust the database: even a compromised API cannot get a
// privileged container.
//
// Each feature's background work is in internal/components/<feature>/agent, as a type
// that embeds *Agent; internal/agent wires them together.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"platform/internal/lib/config"
	"platform/internal/lib/db"
	"platform/internal/lib/docker"
	"platform/internal/lib/kube"
	"platform/internal/lib/model"
	"platform/internal/lib/registry"
	"platform/internal/lib/secure"
)

type Agent struct {
	Cfg        config.Config
	Pool       *pgxpool.Pool
	Docker     *docker.Client
	Box        *secure.Box
	HostIndex  map[string]HostRef // application host -> organization/service (access logs)
	BucketTrig bucketTriggers     // bucket -> function triggers (S3 requests seen by the proxy)
	Caddy      *caddyClient
	Kube       *kube.Client // set when AUTOSCALE_BACKEND=kubernetes on the primary (kube.go)
	kubeNodes  []string     // the cluster nodes' addresses (denied to tenant pods)
	Logs       *LogShipper
	Wake       chan struct{}
	Log        *slog.Logger

	ScanWake    chan struct{}
	trivyPulled bool

	BackupWake   chan struct{}
	resticPulled bool

	AlertWake     chan struct{}
	BuildWake     chan struct{}
	CleanWake     chan struct{}
	DiskWake      chan struct{}
	JobWake       chan struct{}
	StorageWake   chan struct{}
	FnWake        chan struct{}
	LogExportWake chan struct{}
	MoveWake      chan struct{}
	QueueWake     chan struct{}
	EmailWake     chan struct{}
	AlarmWake     chan struct{}
	BudgetWake    chan struct{}
	DNSWake       chan struct{}
	DNSData       atomic.Value // *dnsData: the hosted zones served
	Fn            *FnState
	AlertHTTP     *http.Client
	ProbeFails    map[string]int
	Attached      map[string]bool // project networks the proxy and agent are known to be on
	RestoringDB   map[string]bool // databases held stopped by a restore

	SelfID string // this node (NODE_ID or the Docker host name)

	Mu        sync.Mutex
	targets   []target // running live containers, refreshed each reconcile
	caddyHash string
	ticks     int

	caddySync sync.Mutex // one config push at a time (the main loop and KubeUpstreamLoop both push)
}

type target struct {
	ContainerID, DeploymentID, OrgID, ProjectID, ServiceID, Instance string
}

// instance is one container of a live deployment (deployments.instances).
type Instance struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Upstream string `json:"upstream,omitempty"`
}

func instanceName(j Job, i int) string {
	if i == 0 {
		return containerName(j)
	}
	return fmt.Sprintf("%s-%d", containerName(j), i)
}

type Job struct {
	ID, OrgID, ServiceID, ProjectID string
	OrgSlug, ScanPolicy             string
	Revision                        int
	Spec                            model.Spec
	Suspended                       bool
	// Autoscaling settings of the service (AutoMin 0 = not autoscaled) and whether it is
	// a preview environment: they decide whether the revision runs on Kubernetes.
	AutoMin, AutoMax, AutoCPU int
	Preview                   bool
}

func (a *Agent) Poke() {
	select {
	case a.Wake <- struct{}{}:
	default:
	}
}

// ---------- deployments ----------

func (a *Agent) ProcessPending(ctx context.Context) {
	for ctx.Err() == nil {
		j, ok, err := a.claim(ctx)
		if err != nil {
			a.Log.Error("claim deployment", "err", err)
			return
		}
		if !ok {
			return
		}
		a.Log.Info("deploying", "deployment", j.ID, "service", j.ServiceID, "revision", j.Revision, "image", j.Spec.Image)
		if err := a.deploy(ctx, j); err != nil {
			a.fail(ctx, j, err)
		}
	}
}

func (a *Agent) claim(ctx context.Context) (Job, bool, error) {
	var j Job
	found := false
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		var spec []byte
		err := tx.QueryRow(ctx, `
			SELECT d.id, d.org_id, d.service_id, s.project_id, d.revision, d.spec, o.suspended, o.slug, o.scan_policy,
			       s.autoscale_min, s.autoscale_max, s.autoscale_cpu, s.preview_of IS NOT NULL
			FROM deployments d
			JOIN services s ON s.id = d.service_id
			JOIN projects p ON p.id = s.project_id
			JOIN orgs o ON o.id = d.org_id
			WHERE d.status = 'pending' AND `+MineSQL+`
			  AND NOT coalesce((SELECT deploys_paused FROM platform_settings WHERE id = 1), false)
			ORDER BY d.created_at
			LIMIT 1
			FOR UPDATE OF d SKIP LOCKED`, a.MineArgs()...).Scan(&j.ID, &j.OrgID, &j.ServiceID, &j.ProjectID, &j.Revision, &spec, &j.Suspended, &j.OrgSlug, &j.ScanPolicy,
			&j.AutoMin, &j.AutoMax, &j.AutoCPU, &j.Preview)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(spec, &j.Spec); err != nil {
			return fmt.Errorf("decode spec: %w", err)
		}
		found = true
		_, err = tx.Exec(ctx, `UPDATE deployments SET status = 'deploying', message = 'starting', updated_at = now() WHERE id = $1`, j.ID)
		return err
	})
	return j, found, err
}

func (a *Agent) setMessage(ctx context.Context, id, msg string) {
	_, _ = a.Pool.Exec(ctx, `UPDATE deployments SET message = $2, updated_at = now() WHERE id = $1`, id, msg)
}

func (a *Agent) fail(ctx context.Context, j Job, err error) {
	msg := err.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	a.Log.Warn("deployment failed", "deployment", j.ID, "err", msg)
	_, _ = a.Pool.Exec(ctx, `UPDATE deployments SET status = 'failed', message = $2, updated_at = now() WHERE id = $1`, j.ID, msg)
	_ = db.Audit(ctx, a.Pool, j.OrgID, "", "deployment.failed", "service:"+j.ServiceID,
		map[string]any{"revision": j.Revision, "error": Truncate(msg, 300)}, "")
	a.AlertEvent(ctx, j.OrgID, j.ServiceID, "deploy_failed",
		fmt.Sprintf("Revision %d (%s) failed: %s", j.Revision, j.Spec.Image, Truncate(FirstLine(msg), 300)))
}

func containerName(j Job) string {
	return fmt.Sprintf("ps-%s-r%d", model.Compact(j.ServiceID)[:12], j.Revision)
}

func (a *Agent) deploy(ctx context.Context, j Job) error {
	if j.Suspended {
		return errors.New("organization is suspended")
	}
	if err := a.checkPolicy(j.Spec); err != nil {
		return fmt.Errorf("rejected by policy: %w", err)
	}
	// An organization may only run images from its own registry namespace. Pulls always
	// hit the registry with org-scoped credentials, so a locally cached image of another
	// tenant can never be reused through a different spelling of the reference.
	if err := registry.CheckNamespace(j.Spec.Image, a.Cfg.RegistryHost, j.OrgSlug); err != nil {
		return fmt.Errorf("rejected by policy: %w", err)
	}
	netName, err := a.EnsureNetwork(ctx, j)
	if err != nil {
		return err
	}

	if a.useKube(j) { // autoscaled web service, AUTOSCALE_BACKEND=kubernetes (kube.go)
		return a.deployKube(ctx, j, netName)
	}

	a.setMessage(ctx, j.ID, "pulling image "+j.Spec.Image)
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err = a.Docker.ImagePull(pctx, j.Spec.Image, a.pullAuth(j.Spec.Image, j.OrgSlug))
	cancel()
	if err != nil {
		return fmt.Errorf("could not pull image %s: %w", j.Spec.Image, err)
	}
	if a.Cfg.ScannerEnabled {
		if err := a.gate(ctx, j); err != nil {
			return err
		}
	}

	env, err := a.BuildEnv(ctx, j)
	if err != nil {
		return err
	}
	// A public TCP/UDP port can be bound by one container only: stop the running one
	// first (a short interruption); it is started again if the new one fails.
	var stopped []string
	if len(j.Spec.Ports) > 0 {
		if cs, err := a.Docker.ContainerList(ctx, "platform.service="+j.ServiceID); err == nil {
			for _, c := range cs {
				if c.State == "running" && c.Labels["platform.job"] == "" && a.Owns(c.Labels) {
					if err := a.Docker.ContainerStop(ctx, c.ID, 10); err == nil {
						stopped = append(stopped, c.ID)
					}
				}
			}
		}
	}
	restore := func() {
		for _, id := range stopped {
			_ = a.Docker.ContainerStart(context.Background(), id)
		}
	}
	// Start every instance, wait until all are healthy, then switch traffic at once.
	n := j.Spec.Count()
	var insts []Instance
	cleanup := func() {
		for _, in := range insts {
			_ = a.Docker.ContainerRemove(context.Background(), in.ID)
		}
		restore() // the previous version keeps serving its ports
	}
	if n > 1 {
		a.setMessage(ctx, j.ID, fmt.Sprintf("starting %d instances", n))
	} else {
		a.setMessage(ctx, j.ID, "starting container")
	}
	hosts := a.ProjectHosts(ctx, j.ProjectID, netName)
	for i := 0; i < n; i++ {
		name := instanceName(j, i)
		_ = a.Docker.ContainerRemove(ctx, name) // leftover from an interrupted attempt
		cfg := a.ContainerConfig(j, netName, env, hosts)
		cfg.Labels["platform.instance"] = strconv.Itoa(i)
		id, err := a.Docker.ContainerCreate(ctx, name, cfg)
		if err != nil {
			cleanup()
			return fmt.Errorf("create container: %w", err)
		}
		insts = append(insts, Instance{ID: id, Name: name})
		if err := a.Docker.ContainerStart(ctx, id); err != nil {
			cleanup()
			return fmt.Errorf("start container: %w", err)
		}
	}

	a.setMessage(ctx, j.ID, "waiting for health check")
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range insts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = a.waitHealthy(ctx, insts[i].ID, netName, j.Spec)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			cleanup()
			if n > 1 {
				return fmt.Errorf("instance %d of %d: %w", i+1, n, err)
			}
			return err
		}
	}
	if !a.IsPrimary() && !j.Spec.Worker {
		port := strconv.Itoa(j.Spec.Port) + "/tcp"
		for i := range insts {
			info, err := a.Docker.ContainerInspect(ctx, insts[i].ID)
			if err != nil || len(info.NetworkSettings.Ports[port]) == 0 {
				cleanup()
				return errors.New("the app port was not published on this node")
			}
			insts[i].Upstream = a.Cfg.NodeAddress + ":" + info.NetworkSettings.Ports[port][0].HostPort
		}
	}
	name := insts[0].Name

	old, err := a.promote(ctx, j, insts)
	if err != nil {
		cleanup()
		return err
	}
	a.SyncCaddy(ctx, true) // traffic moves to the new container before the old one stops
	if !a.IsPrimary() {
		// The proxy runs on the primary: wake it and give it time to switch routes.
		_, _ = a.Pool.Exec(ctx, `SELECT pg_notify('deployments', '')`)
		time.Sleep(3 * time.Second)
	}
	// Let requests already sent to the old instances finish before stopping them.
	var oldNames []string
	for _, oid := range old {
		if info, err := a.Docker.ContainerInspect(ctx, oid); oid != "" && err == nil {
			oldNames = append(oldNames, strings.TrimPrefix(info.Name, "/"))
		}
	}
	a.waitDrained(ctx, oldNames, 15*time.Second)
	for _, oid := range old {
		if oid != "" {
			_ = a.Docker.ContainerStop(ctx, oid, 10)
			_ = a.Docker.ContainerRemove(ctx, oid)
		}
	}
	_ = db.Audit(ctx, a.Pool, j.OrgID, "", "deployment.live", "service:"+j.ServiceID,
		map[string]any{"revision": j.Revision, "image": j.Spec.Image}, "")
	a.Log.Info("deployment live", "deployment", j.ID, "container", name)
	a.Reconcile(ctx)
	return nil
}

// checkPolicy is the last line of defence: it re-validates everything the API already checked.
func (a *Agent) checkPolicy(s model.Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.CPUMillis > a.Cfg.MaxCPUMillis {
		return fmt.Errorf("CPU %d exceeds the host maximum of %d millicores", s.CPUMillis, a.Cfg.MaxCPUMillis)
	}
	if s.MemoryMB > a.Cfg.MaxMemoryMB {
		return fmt.Errorf("memory %d MB exceeds the host maximum of %d MB", s.MemoryMB, a.Cfg.MaxMemoryMB)
	}
	if a.Cfg.Runtime != "runsc" && !a.Cfg.AllowInsecureRuntime {
		return errors.New("tenant containers must run under gVisor")
	}
	return nil
}

func (a *Agent) EnsureNetwork(ctx context.Context, j Job) (string, error) {
	name := model.NetworkName(j.ProjectID)
	n, err := a.Docker.NetworkInspect(ctx, name)
	switch {
	case docker.IsNotFound(err):
		if err := a.createProjectNetwork(ctx, name, j); err != nil {
			return "", err
		}
	case err != nil:
		return "", fmt.Errorf("inspect network: %w", err)
	case n.Labels["platform.org"] != j.OrgID || n.Labels["platform.project"] != j.ProjectID:
		return "", fmt.Errorf("network %s belongs to another tenant", name)
	}
	// Caddy must reach the app (on the primary; workers publish the port instead);
	// in development the agent container needs it for health checks.
	if a.IsPrimary() {
		if err := a.attachProxy(ctx, name); err != nil {
			return "", fmt.Errorf("connect proxy to network: %w", err)
		}
	}
	if a.Cfg.AgentContainer != "" {
		if err := a.Docker.NetworkConnect(ctx, name, a.Cfg.AgentContainer); err != nil {
			return "", fmt.Errorf("connect agent to network: %w", err)
		}
	}
	return name, nil
}

func (a *Agent) BuildEnv(ctx context.Context, j Job) ([]string, error) {
	env := make([]string, 0, len(j.Spec.Env)+8)
	// Shared project variables first; the service's own variables and secrets below
	// override them (dedupEnv keeps the last value of a repeated name).
	prows, err := a.Pool.Query(ctx, `SELECT name, value_enc FROM project_variables WHERE project_id = $1 ORDER BY name`, j.ProjectID)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var name string
		var enc []byte
		if err := prows.Scan(&name, &enc); err != nil {
			prows.Close()
			return nil, err
		}
		plain, err := a.Box.Open(enc, "projvar:"+j.ProjectID+":"+name)
		if err != nil {
			prows.Close()
			return nil, fmt.Errorf("decrypt project variable %s: %w", name, err)
		}
		env = append(env, name+"="+string(plain))
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(j.Spec.Env))
	for k := range j.Spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+j.Spec.Env[k])
	}
	rows, err := a.Pool.Query(ctx, `SELECT name, value_enc FROM secrets WHERE service_id = $1 ORDER BY name`, j.ServiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var enc []byte
		if err := rows.Scan(&name, &enc); err != nil {
			return nil, err
		}
		plain, err := a.Box.Open(enc, "secret:"+j.ServiceID+":"+name)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", name, err)
		}
		env = append(env, name+"="+string(plain))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !j.Spec.Worker {
		env = append(env, "PORT="+strconv.Itoa(j.Spec.Port))
	}
	return a.ResolveSecretRefs(ctx, j.OrgID, DedupEnv(env))
}

// dedupEnv keeps one entry per name, the last one, at the position of its first.
func DedupEnv(env []string) []string {
	last := map[string]string{}
	var order []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, seen := last[k]; !seen {
			order = append(order, k)
		}
		last[k] = kv
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, last[k])
	}
	return out
}

func (a *Agent) ContainerConfig(j Job, netName string, env, hosts []string) docker.ContainerConfig {
	hc := docker.HostConfig{
		Runtime:    a.Cfg.Runtime,
		Memory:     int64(j.Spec.MemoryMB) << 20,
		MemorySwap: int64(j.Spec.MemoryMB) << 20, // no swap
		NanoCPUs:   int64(j.Spec.CPUMillis) * 1_000_000,
		PidsLimit:  256,
		CapDrop:    []string{"ALL"},
		// Enough for common images (nginx, node, python) to start and drop privileges.
		// Excluded on purpose: NET_RAW, SYS_*, MKNOD, AUDIT_WRITE, SETFCAP, SETPCAP.
		CapAdd:        []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID", "NET_BIND_SERVICE", "KILL"},
		SecurityOpt:   []string{"no-new-privileges:true"},
		Privileged:    false,
		NetworkMode:   netName,
		RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		LogConfig:     docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
		ExtraHosts:    hosts,
	}
	if j.Spec.VolumePath != "" {
		hc.Mounts = []docker.Mount{{Type: "volume", Source: "pv-" + model.Compact(j.ServiceID), Target: j.Spec.VolumePath}}
	}
	a.TenantDNS(&hc)
	cfg := docker.ContainerConfig{
		Image: j.Spec.Image,
		Env:   env,
		Labels: map[string]string{
			"platform.managed":    "true",
			"platform.org":        j.OrgID,
			"platform.project":    j.ProjectID,
			"platform.service":    j.ServiceID,
			"platform.deployment": j.ID,
			"platform.node":       a.SelfID,
		},
		HostConfig: hc,
	}
	if !a.IsPrimary() && !j.Spec.Worker { // reachable only on the private (WireGuard) address
		port := strconv.Itoa(j.Spec.Port) + "/tcp"
		cfg.ExposedPorts = map[string]struct{}{port: {}}
		cfg.HostConfig.PortBindings = map[string][]docker.PortBinding{port: {{HostIP: a.Cfg.NodeBind}}}
	}
	// Public TCP/UDP ports (non-HTTP services), on every address of the node.
	for _, p := range j.Spec.Ports {
		if p.PublicPort == 0 {
			continue
		}
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
			cfg.HostConfig.PortBindings = map[string][]docker.PortBinding{}
		}
		key := p.Key()
		cfg.ExposedPorts[key] = struct{}{}
		cfg.HostConfig.PortBindings[key] = append(cfg.HostConfig.PortBindings[key], docker.PortBinding{HostIP: "0.0.0.0", HostPort: strconv.Itoa(p.PublicPort)})
	}
	return cfg
}

func (a *Agent) waitHealthy(ctx context.Context, id, netName string, s model.Spec) error {
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if s.Worker { // no port to probe: healthy if it is still running after a few seconds
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(8 * time.Second):
		}
		info, err := a.Docker.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect container: %w", err)
		}
		if !info.State.Running || info.RestartCount > 0 {
			reason := fmt.Sprintf("exit code %d", info.State.ExitCode)
			if info.State.OOMKilled {
				reason = "killed: out of memory, raise the memory limit"
			}
			return fmt.Errorf("worker stopped (%s)%s", reason, a.Tail(ctx, id))
		}
		return nil
	}
	deadline := time.Now().Add(90 * time.Second)
	lastErr := errors.New("no answer yet")
	for time.Now().Before(deadline) {
		info, err := a.Docker.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect container: %w", err)
		}
		if !info.State.Running {
			reason := fmt.Sprintf("exit code %d", info.State.ExitCode)
			if info.State.OOMKilled {
				reason = "killed: out of memory, raise the memory limit"
			}
			return fmt.Errorf("container stopped (%s)%s", reason, a.Tail(ctx, id))
		}
		if ip := info.NetworkSettings.Networks[netName].IPAddress; ip != "" {
			u := "http://" + net.JoinHostPort(ip, strconv.Itoa(s.Port)) + s.HealthPath
			resp, err := client.Get(u)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				if resp.StatusCode < 500 {
					return nil
				}
				lastErr = fmt.Errorf("health check %s returned HTTP %d", s.HealthPath, resp.StatusCode)
			} else {
				lastErr = fmt.Errorf("nothing answering on port %d", s.Port)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("not healthy after 90 s: %v%s", lastErr, a.Tail(ctx, id))
}

// tail returns the last log lines of a container, for failure messages shown to the client.
// logSays reports whether the container's recent output contains text.
func (a *Agent) logSays(ctx context.Context, id, text string) bool {
	rc, err := a.Docker.ContainerLogs(ctx, id, 0, false, "2000")
	if err != nil {
		return false
	}
	defer rc.Close()
	found := false
	_ = docker.Demux(rc, func(_ byte, line string) {
		if !found && strings.Contains(line, text) {
			found = true
		}
	})
	return found
}

func (a *Agent) Tail(ctx context.Context, id string) string {
	rc, err := a.Docker.ContainerLogs(ctx, id, 0, false, "15")
	if err != nil {
		return ""
	}
	defer rc.Close()
	var lines []string
	_ = docker.Demux(rc, func(_ byte, line string) {
		if _, msg, ok := strings.Cut(line, " "); ok {
			line = msg
		}
		lines = append(lines, Truncate(line, 200))
	})
	if len(lines) == 0 {
		return ""
	}
	return "\n--- last log lines ---\n" + strings.Join(lines, "\n")
}

// promote makes the deployment live with its instances and supersedes the previous
// one; it returns the containers of the deployments it replaced.
func (a *Agent) promote(ctx context.Context, j Job, insts []Instance) ([]string, error) {
	var old []string
	raw, _ := json.Marshal(insts)
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE deployments SET status = 'superseded', updated_at = now()
			WHERE service_id = $1 AND status = 'live' AND id <> $2
			RETURNING coalesce(container_id, ''), instances`, j.ServiceID, j.ID)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for rows.Next() {
			var first string
			var list []Instance
			if err := rows.Scan(&first, &list); err != nil {
				rows.Close()
				return err
			}
			for _, id := range append([]string{first}, instanceIDs(list)...) {
				if id != "" && !seen[id] {
					seen[id] = true
					old = append(old, id)
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE deployments SET status = 'live', message = '', container_id = $2, container_name = $3, upstream = $4, node_id = $5,
			       instances = $6, updated_at = now()
			WHERE id = $1`, j.ID, insts[0].ID, insts[0].Name, insts[0].Upstream, a.SelfID, raw)
		return err
	})
	return old, err
}

func instanceIDs(list []Instance) []string {
	out := make([]string, 0, len(list))
	for _, in := range list {
		out = append(out, in.ID)
	}
	return out
}

// ---------- reconcile ----------

func (a *Agent) Reconcile(ctx context.Context) {
	type live struct {
		DepID, ContainerID, OrgID, ProjectID, ServiceID, Upstream, NodeID string
		Suspended                                                         bool
		Count                                                             int
		FirstInstance                                                     string // "k8s:<ns>/<name>" on Kubernetes
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT d.id, coalesce(d.container_id, ''), d.org_id, s.project_id, d.service_id, d.upstream, coalesce(d.node_id, ''), o.suspended,
		       greatest(1, jsonb_array_length(d.instances)), coalesce(d.instances->0->>'id', '')
		FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id
		JOIN projects p ON p.id = s.project_id
		WHERE d.status = 'live' AND `+MineSQL, a.MineArgs()...)
	if err != nil {
		a.Log.Error("reconcile: load live deployments", "err", err)
		return
	}
	lives, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (live, error) {
		var l live
		err := r.Scan(&l.DepID, &l.ContainerID, &l.OrgID, &l.ProjectID, &l.ServiceID, &l.Upstream, &l.NodeID, &l.Suspended, &l.Count, &l.FirstInstance)
		return l, err
	})
	if err != nil {
		a.Log.Error("reconcile: scan", "err", err)
		return
	}
	rows, err = a.Pool.Query(ctx, `
		SELECT d.id::text FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id
		WHERE d.status IN ('live', 'deploying', 'pending') AND `+MineSQL, a.MineArgs()...)
	if err != nil {
		a.Log.Error("reconcile: load active ids", "err", err)
		return
	}
	activeIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, id := range activeIDs {
		keep[id] = true
	}

	containers, err := a.Docker.ContainerList(ctx, "platform.managed=true")
	if err != nil {
		a.Log.Error("reconcile: list containers", "err", err)
		return
	}
	// A project moved to another node: its old container keeps serving until the new
	// node's deployment is live (that supersedes this one), then it is removed here.
	var mine []string
	for _, c := range containers {
		if c.Labels["platform.database"] == "" && c.Labels["platform.job"] == "" && a.Owns(c.Labels) && !keep[c.Labels["platform.deployment"]] {
			mine = append(mine, c.ID)
		}
	}
	if len(mine) > 0 {
		if rows, err := a.Pool.Query(ctx, `
			SELECT id::text FROM deployments d WHERE status = 'live' AND (container_id = ANY($1)
			   OR EXISTS (SELECT 1 FROM jsonb_array_elements(d.instances) e WHERE e->>'id' = ANY($1)))`, mine); err == nil {
			if ids, err := pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
				for _, id := range ids {
					keep[id] = true
				}
			}
		}
	}
	byDep := map[string][]docker.ContainerSummary{}
	for _, c := range containers {
		if c.Labels["platform.database"] != "" || c.Labels["platform.job"] != "" || !a.Owns(c.Labels) {
			continue
		}
		dep := c.Labels["platform.deployment"]
		if !keep[dep] {
			a.Log.Info("removing orphan container", "container", strings.Join(c.Names, ","))
			_ = a.Docker.ContainerStop(ctx, c.ID, 10)
			_ = a.Docker.ContainerRemove(ctx, c.ID)
			continue
		}
		byDep[dep] = append(byDep[dep], c)
	}

	a.Mu.Lock()
	a.ticks++
	force := a.ticks%4 == 0 // re-verify network attachments and push Caddy config every minute
	a.Mu.Unlock()

	// A recreated proxy or agent container loses its tenant network attachments
	// (on every upgrade, for instance). Re-attach both to every live project network,
	// otherwise routes break and health probes report healthy apps as down.
	for _, l := range lives {
		if l.Suspended || !a.IsPrimary() && a.Cfg.AgentContainer == "" {
			continue
		}
		netName := model.NetworkName(l.ProjectID)
		a.Mu.Lock()
		known := a.Attached[netName]
		a.Mu.Unlock()
		if known && !force {
			continue
		}
		var err error
		if a.IsPrimary() {
			err = a.attachProxy(ctx, netName)
		}
		if err == nil && a.Cfg.AgentContainer != "" {
			err = a.Docker.NetworkConnect(ctx, netName, a.Cfg.AgentContainer)
		}
		a.Mu.Lock()
		a.Attached[netName] = err == nil
		a.Mu.Unlock()
		if err != nil {
			a.Log.Warn("attach to project network", "network", netName, "err", err)
		}
	}

	var running []target
	var onKube []kubeLive
	for _, l := range lives {
		if ns, name, ok := ParseKubeInstance(l.FirstInstance); ok { // no containers: see reconcileKube
			onKube = append(onKube, kubeLive{DepID: l.DepID, OrgID: l.OrgID, ProjectID: l.ProjectID, ServiceID: l.ServiceID,
				Namespace: ns, Name: name, Suspended: l.Suspended})
			continue
		}
		cs := byDep[l.DepID]
		ok := len(cs) >= l.Count
		if l.Suspended {
			for _, c := range cs {
				if c.State == "running" {
					a.Log.Info("stopping container of suspended organization", "org", l.OrgID)
					_ = a.Docker.ContainerStop(ctx, c.ID, 10)
				}
			}
			continue
		}
		if !ok && (len(cs) > 0 || a.RanElsewhere(l.NodeID, l.Upstream)) {
			// The project was moved here (drain, placement), or some instances were lost:
			// start a new revision; what still runs keeps serving until it is live.
			reason := "project moved to node " + a.SelfID
			if len(cs) > 0 {
				reason = fmt.Sprintf("%d of %d instances were lost", l.Count-len(cs), l.Count)
			}
			a.redeploy(ctx, l.DepID, l.OrgID, l.ServiceID, reason)
			continue
		}
		if !ok {
			a.Log.Warn("live container missing, redeploying", "deployment", l.DepID)
			_, _ = a.Pool.Exec(ctx, `UPDATE deployments SET status = 'pending', message = 'container was lost, redeploying', updated_at = now() WHERE id = $1`, l.DepID)
			a.Poke()
			continue
		}
		for _, c := range cs {
			if c.State != "running" {
				if err := a.Docker.ContainerStart(ctx, c.ID); err != nil {
					a.Log.Warn("restart container", "deployment", l.DepID, "err", err)
					continue
				}
			}
			running = append(running, target{c.ID, l.DepID, l.OrgID, l.ProjectID, l.ServiceID, c.Labels["platform.instance"]})
		}
	}

	a.Mu.Lock()
	a.targets = running // Docker containers only: stats, restarts and probes use the Docker API
	a.Mu.Unlock()

	pods := a.reconcileKube(ctx, onKube, keep, force)
	a.Logs.ensure(ctx, append(append([]target(nil), running...), pods...))
	a.ReconcileDatabases(ctx)
	a.SyncCaddy(ctx, force)
	if force {
		a.collectNetworks(ctx)
	}
}

// redeploy starts a new revision of a live deployment's spec (unless one is already on
// its way, or one failed in the last 5 minutes); what still runs keeps serving until it
// is live.
func (a *Agent) redeploy(ctx context.Context, depID, orgID, serviceID, reason string) {
	tag, err := a.Pool.Exec(ctx, `
		INSERT INTO deployments (org_id, service_id, revision, spec)
		SELECT d.org_id, d.service_id, (SELECT max(revision) + 1 FROM deployments WHERE service_id = d.service_id), d.spec
		FROM deployments d WHERE d.id = $1
		  AND NOT EXISTS (SELECT 1 FROM deployments x WHERE x.service_id = d.service_id AND (x.status IN ('pending', 'deploying')
		      OR (x.status = 'failed' AND x.revision > d.revision AND x.updated_at > now() - interval '5 minutes')))`, depID)
	if err == nil && tag.RowsAffected() > 0 {
		_ = AuditAgent(ctx, a, orgID, "deployment.redeployed", "service:"+serviceID, map[string]string{"reason": reason})
		a.Log.Info("redeploying", "service", serviceID, "node", a.SelfID, "reason", reason)
		a.Poke()
	}
}

func (a *Agent) CurrentTargets() []target {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return append([]target(nil), a.targets...)
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func AuditAgent(ctx context.Context, a *Agent, orgID, action, target string, detail any) error {
	return db.Audit(ctx, a.Pool, orgID, "", action, target, detail, "")
}

func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------- project networks ----------

// Each project network gets a /24 from 10.208.0.0/12 (4096 networks, 254 addresses each)
// instead of Docker's default /16, which would exhaust the default pools after ~30
// projects. The starting /24 is derived from the project ID; on overlap the next is tried.
func subnetCandidate(projectID string, attempt int) string {
	h := fnv.New32a()
	h.Write([]byte(projectID))
	idx := (int(h.Sum32()) + attempt) % 4096
	if idx < 0 {
		idx += 4096
	}
	return fmt.Sprintf("10.%d.%d.0/24", 208+idx/256, idx%256)
}

func (a *Agent) createProjectNetwork(ctx context.Context, name string, j Job) error {
	labels := map[string]string{"platform.managed": "true", "platform.org": j.OrgID, "platform.project": j.ProjectID, "platform.node": a.SelfID}
	var err error
	for attempt := 0; attempt < 64; attempt++ {
		err = a.Docker.NetworkCreate(ctx, name, subnetCandidate(j.ProjectID, attempt), labels)
		if err == nil {
			return nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "overlap") {
			break
		}
	}
	return fmt.Errorf("create network: %w", err)
}

// collectNetworks removes project networks that no deployment needs any more (deleted
// projects, services or organizations), so addresses are returned to the pool.
func (a *Agent) collectNetworks(ctx context.Context) {
	rows, err := a.Pool.Query(ctx, `
		SELECT DISTINCT s.project_id::text FROM deployments d JOIN services s ON s.id = d.service_id
		JOIN projects p ON p.id = s.project_id
		WHERE d.status IN ('live', 'deploying', 'pending') AND `+MineSQL+`
		UNION SELECT db.project_id::text FROM databases db JOIN projects p ON p.id = db.project_id WHERE `+MineSQL,
		a.MineArgs()...)
	if err != nil {
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return
	}
	needed := map[string]bool{}
	for _, id := range ids {
		needed[id] = true
	}
	nets, err := a.Docker.NetworkList(ctx, "platform.managed=true")
	if err != nil {
		return
	}
	for _, n := range nets {
		if needed[n.Labels["platform.project"]] || !a.Owns(n.Labels) {
			continue
		}
		// Leave it alone while a tenant container still uses it (for instance a project
		// that moved to another node sharing this Docker host): detaching the proxy
		// first would cut that app off until the next attachment check.
		if full, err := a.Docker.NetworkInspect(ctx, n.Name); err != nil || a.hasTenantEndpoints(full) {
			continue
		}
		_ = a.Docker.NetworkDisconnect(ctx, n.Name, a.Cfg.CaddyContainer)
		if a.Cfg.AgentContainer != "" {
			_ = a.Docker.NetworkDisconnect(ctx, n.Name, a.Cfg.AgentContainer)
		}
		if err := a.Docker.NetworkRemove(ctx, n.Name); err != nil {
			continue // tenant containers still attached; retried on a later pass
		}
		a.Mu.Lock()
		delete(a.Attached, n.Name)
		a.Mu.Unlock()
		a.Log.Info("removed unused project network", "network", n.Name)
	}
}

// ---------- name resolution for sandboxed containers ----------

// tenantDNS mounts TENANT_RESOLV_CONF (read-only) as the container's resolv.conf.
// gVisor's network stack cannot reach Docker's embedded DNS (127.0.0.11) on
// user-defined networks, so without this, sandboxed containers resolve nothing.
func (a *Agent) TenantDNS(hc *docker.HostConfig) {
	if a.Cfg.TenantResolvConf == "" {
		return
	}
	hc.Mounts = append(hc.Mounts, docker.Mount{Type: "bind", Source: a.Cfg.TenantResolvConf, Target: "/etc/resolv.conf", ReadOnly: true})
}

func (a *Agent) WithDNS(hc docker.HostConfig) docker.HostConfig {
	a.TenantDNS(&hc)
	return hc
}

// proxyAddress is the proxy's fixed address on a project network: .253 of its /24.
// Fixed, so the hosts entry services use to reach platform endpoints through the proxy
// (the S3 API) stays right when Docker restarts and hands out addresses again.
func (a *Agent) proxyAddress(ctx context.Context, netName string) (string, error) {
	n, err := a.Docker.NetworkInspect(ctx, netName)
	if err != nil {
		return "", err
	}
	if len(n.IPAM.Config) == 0 {
		return "", errors.New("network " + netName + " has no subnet")
	}
	prefix, err := netip.ParsePrefix(n.IPAM.Config[0].Subnet)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return "", fmt.Errorf("unexpected subnet %q on %s", n.IPAM.Config[0].Subnet, netName)
	}
	b := prefix.Addr().As4()
	b[3] = 253
	return netip.AddrFrom4(b).String(), nil
}

// attachProxy connects the proxy to a project network at its fixed address, moving it
// there if it is attached elsewhere (a short interruption, once per network).
func (a *Agent) attachProxy(ctx context.Context, netName string) error {
	want, err := a.proxyAddress(ctx, netName)
	if err != nil {
		return a.Docker.NetworkConnect(ctx, netName, a.Cfg.CaddyContainer)
	}
	info, err := a.Docker.ContainerInspect(ctx, a.Cfg.CaddyContainer)
	if err != nil {
		return err
	}
	if ep, ok := info.NetworkSettings.Networks[netName]; ok {
		if ep.IPAddress == want {
			return nil
		}
		if err := a.Docker.NetworkDisconnect(ctx, netName, a.Cfg.CaddyContainer); err != nil {
			return err
		}
	}
	if err := a.Docker.NetworkConnectIP(ctx, netName, a.Cfg.CaddyContainer, want); err != nil {
		a.Log.Warn("proxy fixed address unavailable, attaching with any address", "network", netName, "err", err)
		return a.Docker.NetworkConnect(ctx, netName, a.Cfg.CaddyContainer)
	}
	return nil
}

// databaseAddress picks a fixed address for a managed database on its project network,
// in .200-.249 of the /24 (Docker hands out addresses from the bottom), derived from the
// database ID so a recreated container usually gets the same one.
func (a *Agent) databaseAddress(ctx context.Context, netName, dbID string) (string, error) {
	n, err := a.Docker.NetworkInspect(ctx, netName)
	if err != nil {
		return "", err
	}
	if len(n.IPAM.Config) == 0 {
		return "", errors.New("network " + netName + " has no subnet")
	}
	prefix, err := netip.ParsePrefix(n.IPAM.Config[0].Subnet)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return "", fmt.Errorf("unexpected subnet %q on %s", n.IPAM.Config[0].Subnet, netName)
	}
	used := map[string]bool{}
	for _, c := range n.Containers {
		used[strings.Split(c.IPv4Address, "/")[0]] = true
	}
	h := fnv.New32a()
	h.Write([]byte(dbID))
	base := prefix.Masked().Addr().As4()
	for i := 0; i < 50; i++ {
		base[3] = byte(200 + (int(h.Sum32()%50)+i)%50)
		ip := netip.AddrFrom4(base).String()
		if !used[ip] {
			return ip, nil
		}
	}
	return "", errors.New("no free database address on " + netName)
}

// projectHosts returns "pd-<id>:<ip>" lines for the project's running databases, so
// apps reach them by the name in their DATABASE_URL / REDIS_URL without Docker DNS.
func (a *Agent) ProjectHosts(ctx context.Context, projectID, netName string) []string {
	rows, err := a.Pool.Query(ctx, `SELECT id, container_id FROM databases WHERE project_id = $1 AND status = 'running' AND container_id <> ''`, projectID)
	if err != nil {
		return nil
	}
	type dbc struct{ id, cid string }
	var list []dbc
	for rows.Next() {
		var d dbc
		if rows.Scan(&d.id, &d.cid) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	var hosts []string
	// The console (its API: Prometheus queries from Grafana, the platform API from your
	// code) and the S3 endpoint are reached through the proxy on the project network:
	// the host's public address is closed to tenant containers.
	if a.IsPrimary() {
		if info, err := a.Docker.ContainerInspect(ctx, a.Cfg.CaddyContainer); err == nil {
			if ep, ok := info.NetworkSettings.Networks[netName]; ok && ep.IPAddress != "" {
				if a.Cfg.ConsoleHost != "" {
					hosts = append(hosts, hostOnly(a.Cfg.ConsoleHost)+":"+ep.IPAddress)
				}
				if a.Cfg.StorageEnabled() {
					hosts = append(hosts, hostOnly(s3ServiceHost(a.Cfg))+":"+ep.IPAddress)
				}
				for _, n := range a.internalNames(ctx, projectID) { // <service>.<project>.internal
					hosts = append(hosts, n+":"+ep.IPAddress)
				}
			}
		}
	}
	for _, d := range list {
		info, err := a.Docker.ContainerInspect(ctx, d.cid)
		if err != nil {
			continue
		}
		if ep, ok := info.NetworkSettings.Networks[netName]; ok && ep.IPAddress != "" {
			hosts = append(hosts, dbContainerName(d.id)+":"+ep.IPAddress)
		}
	}
	return hosts
}

// owns tells whether a container or network belongs to this node: labelled with its
// node ID, or unlabelled (created before multi-node support) and this is the primary.
// ranElsewhere reports whether a live deployment was started by another node: the
// recorded node when there is one; for older rows, from the upstream (the primary never
// records one, a worker node records its own address).
func (a *Agent) RanElsewhere(nodeID, upstream string) bool {
	if nodeID != "" {
		return nodeID != a.SelfID
	}
	if upstream == "" {
		return !a.IsPrimary()
	}
	return a.Cfg.NodeAddress == "" || !strings.HasPrefix(upstream, a.Cfg.NodeAddress+":")
}

// hasTenantEndpoints reports whether a container other than the proxy or an agent is
// attached to the network.
func (a *Agent) hasTenantEndpoints(n *docker.Network) bool {
	for _, c := range n.Containers {
		switch {
		case c.Name == a.Cfg.CaddyContainer, c.Name == a.Cfg.AgentContainer, strings.HasPrefix(c.Name, "platform-agent"):
		default:
			return true
		}
	}
	return false
}

func (a *Agent) Owns(labels map[string]string) bool {
	n := labels["platform.node"]
	if n == "" {
		return a.IsPrimary()
	}
	return n == a.SelfID
}
