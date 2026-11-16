// Package agent is the only component that touches Docker and Caddy. It reads desired
// state from PostgreSQL and converges the host to it, enforcing a hard policy that does
// not trust the database: even a compromised API cannot get a privileged container.
package agent

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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"platform/internal/config"
	"platform/internal/db"
	"platform/internal/docker"
	"platform/internal/model"
	"platform/internal/registry"
	"platform/internal/secure"
)

type Agent struct {
	cfg   config.Config
	pool  *pgxpool.Pool
	dk    *docker.Client
	box   *secure.Box
	caddy *caddyClient
	logs  *logShipper
	wake  chan struct{}
	log   *slog.Logger

	scanWake    chan struct{}
	trivyPulled bool

	backupWake   chan struct{}
	resticPulled bool

	alertWake   chan struct{}
	buildWake   chan struct{}
	cleanWake   chan struct{}
	diskWake    chan struct{}
	alertHTTP   *http.Client
	probeFails  map[string]int
	attached    map[string]bool // project networks the proxy and agent are known to be on
	restoringDB map[string]bool // databases held stopped by a restore

	selfID string // this node (NODE_ID or the Docker host name)

	mu        sync.Mutex
	targets   []target // running live containers, refreshed each reconcile
	caddyHash string
	ticks     int
}

type target struct {
	ContainerID, DeploymentID, OrgID, ProjectID, ServiceID string
}

type job struct {
	ID, OrgID, ServiceID, ProjectID string
	OrgSlug, ScanPolicy             string
	Revision                        int
	Spec                            model.Spec
	Suspended                       bool
}

func Run(ctx context.Context, cfg config.Config) error {
	log := slog.Default().With("component", "agent")
	if cfg.Runtime != "runsc" {
		if !cfg.AllowInsecureRuntime {
			return errors.New("refusing to start: RUNTIME must be runsc (gVisor); set ALLOW_INSECURE_RUNTIME=true only for local development")
		}
		log.Warn("INSECURE: tenant containers run WITHOUT the gVisor sandbox. Development only.", "runtime", cfg.Runtime)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	box, err := secure.LoadBox(cfg.MasterKeyFile)
	if err != nil {
		return err
	}
	dk := docker.New(cfg.DockerSocket)
	a := &Agent{
		cfg: cfg, pool: pool, dk: dk, box: box,
		caddy: newCaddyClient(cfg.CaddyAdminSocket),
		logs:  newLogShipper(dk, cfg.VictoriaLogsURL, log),
		wake:  make(chan struct{}, 1),
		log:   log,

		scanWake:    make(chan struct{}, 1),
		backupWake:  make(chan struct{}, 1),
		alertWake:   make(chan struct{}, 1),
		buildWake:   make(chan struct{}, 1),
		cleanWake:   make(chan struct{}, 1),
		diskWake:    make(chan struct{}, 1),
		alertHTTP:   newAlertHTTP(cfg.AlertAllowPrivate),
		probeFails:  map[string]int{},
		attached:    map[string]bool{},
		restoringDB: map[string]bool{},
	}

	a.resolveSelf(ctx)
	a.heartbeat(ctx) // register the node before anything is placed on it

	// A deploy interrupted by a restart is retried from the start (only this node's).
	if _, err := pool.Exec(ctx, `
		UPDATE deployments d SET status = 'pending', message = 'retrying after agent restart', updated_at = now()
		FROM services s, projects p WHERE s.id = d.service_id AND p.id = s.project_id AND d.status = 'deploying' AND `+mineSQL,
		a.mineArgs()...); err != nil {
		return err
	}

	go a.listen(ctx)
	go a.logs.run(ctx)
	go a.metricsLoop(ctx)
	go a.scanLoop(ctx)
	go a.backupLoop(ctx)
	go a.alertLoop(ctx)
	go a.meterLoop(ctx)
	go a.buildLoop(ctx)
	go a.registryCleanLoop(ctx)
	go a.diskLoop(ctx)
	go a.nodeLoop(ctx)

	a.reconcile(ctx)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		a.placeProjects(ctx)
		a.processPending(ctx)
		a.syncCaddy(ctx, false)
		a.reconcileDatabases(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-a.wake:
		case <-tick.C:
			a.reconcile(ctx)
		}
	}
}

// listen wakes the main loop on NOTIFY deployments (sent by the API).
func (a *Agent) listen(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := a.pool.Acquire(ctx)
		if err == nil {
			if _, err = conn.Exec(ctx, "LISTEN deployments"); err == nil {
				for err == nil {
					var n *pgconn.Notification
					n, err = conn.Conn().WaitForNotification(ctx)
					if err == nil {
						switch n.Payload {
						case "scan":
							a.pokeScans()
						case "backup":
							a.pokeBackups()
						case "alert":
							a.pokeAlerts()
						case "build":
							a.pokeBuilds()
						case "registry-clean":
							a.pokeRegistryClean()
						case "disk":
							a.pokeDisk()
						default:
							a.poke()
						}
					}
				}
			}
			conn.Release()
		}
		if ctx.Err() == nil {
			a.log.Warn("notification listener restarting", "err", err)
			time.Sleep(2 * time.Second)
		}
	}
}

func (a *Agent) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// ---------- deployments ----------

func (a *Agent) processPending(ctx context.Context) {
	for ctx.Err() == nil {
		j, ok, err := a.claim(ctx)
		if err != nil {
			a.log.Error("claim deployment", "err", err)
			return
		}
		if !ok {
			return
		}
		a.log.Info("deploying", "deployment", j.ID, "service", j.ServiceID, "revision", j.Revision, "image", j.Spec.Image)
		if err := a.deploy(ctx, j); err != nil {
			a.fail(ctx, j, err)
		}
	}
}

func (a *Agent) claim(ctx context.Context) (job, bool, error) {
	var j job
	found := false
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		var spec []byte
		err := tx.QueryRow(ctx, `
			SELECT d.id, d.org_id, d.service_id, s.project_id, d.revision, d.spec, o.suspended, o.slug, o.scan_policy
			FROM deployments d
			JOIN services s ON s.id = d.service_id
			JOIN projects p ON p.id = s.project_id
			JOIN orgs o ON o.id = d.org_id
			WHERE d.status = 'pending' AND `+mineSQL+`
			ORDER BY d.created_at
			LIMIT 1
			FOR UPDATE OF d SKIP LOCKED`, a.mineArgs()...).Scan(&j.ID, &j.OrgID, &j.ServiceID, &j.ProjectID, &j.Revision, &spec, &j.Suspended, &j.OrgSlug, &j.ScanPolicy)
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
	_, _ = a.pool.Exec(ctx, `UPDATE deployments SET message = $2, updated_at = now() WHERE id = $1`, id, msg)
}

func (a *Agent) fail(ctx context.Context, j job, err error) {
	msg := err.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	a.log.Warn("deployment failed", "deployment", j.ID, "err", msg)
	_, _ = a.pool.Exec(ctx, `UPDATE deployments SET status = 'failed', message = $2, updated_at = now() WHERE id = $1`, j.ID, msg)
	_ = db.Audit(ctx, a.pool, j.OrgID, "", "deployment.failed", "service:"+j.ServiceID,
		map[string]any{"revision": j.Revision, "error": truncate(msg, 300)}, "")
	a.alertEvent(ctx, j.OrgID, j.ServiceID, "deploy_failed",
		fmt.Sprintf("Revision %d (%s) failed: %s", j.Revision, j.Spec.Image, truncate(firstLine(msg), 300)))
}

func containerName(j job) string {
	return fmt.Sprintf("ps-%s-r%d", model.Compact(j.ServiceID)[:12], j.Revision)
}

func (a *Agent) deploy(ctx context.Context, j job) error {
	if j.Suspended {
		return errors.New("organization is suspended")
	}
	if err := a.checkPolicy(j.Spec); err != nil {
		return fmt.Errorf("rejected by policy: %w", err)
	}
	// An organization may only run images from its own registry namespace. Pulls always
	// hit the registry with org-scoped credentials, so a locally cached image of another
	// tenant can never be reused through a different spelling of the reference.
	if err := registry.CheckNamespace(j.Spec.Image, a.cfg.RegistryHost, j.OrgSlug); err != nil {
		return fmt.Errorf("rejected by policy: %w", err)
	}
	netName, err := a.ensureNetwork(ctx, j)
	if err != nil {
		return err
	}

	a.setMessage(ctx, j.ID, "pulling image "+j.Spec.Image)
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err = a.dk.ImagePull(pctx, j.Spec.Image, a.pullAuth(j.Spec.Image, j.OrgSlug))
	cancel()
	if err != nil {
		return fmt.Errorf("could not pull image %s: %w", j.Spec.Image, err)
	}
	if a.cfg.ScannerEnabled {
		if err := a.gate(ctx, j); err != nil {
			return err
		}
	}

	env, err := a.buildEnv(ctx, j)
	if err != nil {
		return err
	}
	name := containerName(j)
	_ = a.dk.ContainerRemove(ctx, name) // leftover from an interrupted attempt

	a.setMessage(ctx, j.ID, "starting container")
	id, err := a.dk.ContainerCreate(ctx, name, a.containerConfig(j, netName, env, a.projectHosts(ctx, j.ProjectID, netName)))
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	cleanup := func() { _ = a.dk.ContainerRemove(context.Background(), id) }
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		cleanup()
		return fmt.Errorf("start container: %w", err)
	}

	a.setMessage(ctx, j.ID, "waiting for health check")
	if err := a.waitHealthy(ctx, id, netName, j.Spec); err != nil {
		cleanup()
		return err
	}
	upstream := ""
	if !a.isPrimary() {
		info, err := a.dk.ContainerInspect(ctx, id)
		port := strconv.Itoa(j.Spec.Port) + "/tcp"
		if err != nil || len(info.NetworkSettings.Ports[port]) == 0 {
			cleanup()
			return errors.New("the app port was not published on this node")
		}
		upstream = a.cfg.NodeAddress + ":" + info.NetworkSettings.Ports[port][0].HostPort
	}

	old, err := a.promote(ctx, j, id, name, upstream)
	if err != nil {
		cleanup()
		return err
	}
	a.syncCaddy(ctx, true) // traffic moves to the new container before the old one stops
	if !a.isPrimary() {
		// The proxy runs on the primary: wake it and give it time to switch routes.
		_, _ = a.pool.Exec(ctx, `SELECT pg_notify('deployments', '')`)
		time.Sleep(3 * time.Second)
	}
	for _, oid := range old {
		if oid != "" {
			_ = a.dk.ContainerStop(ctx, oid, 10)
			_ = a.dk.ContainerRemove(ctx, oid)
		}
	}
	_ = db.Audit(ctx, a.pool, j.OrgID, "", "deployment.live", "service:"+j.ServiceID,
		map[string]any{"revision": j.Revision, "image": j.Spec.Image}, "")
	a.log.Info("deployment live", "deployment", j.ID, "container", name)
	a.reconcile(ctx)
	return nil
}

// checkPolicy is the last line of defence: it re-validates everything the API already checked.
func (a *Agent) checkPolicy(s model.Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.CPUMillis > a.cfg.MaxCPUMillis {
		return fmt.Errorf("CPU %d exceeds the host maximum of %d millicores", s.CPUMillis, a.cfg.MaxCPUMillis)
	}
	if s.MemoryMB > a.cfg.MaxMemoryMB {
		return fmt.Errorf("memory %d MB exceeds the host maximum of %d MB", s.MemoryMB, a.cfg.MaxMemoryMB)
	}
	if a.cfg.Runtime != "runsc" && !a.cfg.AllowInsecureRuntime {
		return errors.New("tenant containers must run under gVisor")
	}
	return nil
}

func (a *Agent) ensureNetwork(ctx context.Context, j job) (string, error) {
	name := model.NetworkName(j.ProjectID)
	n, err := a.dk.NetworkInspect(ctx, name)
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
	if a.isPrimary() {
		if err := a.dk.NetworkConnect(ctx, name, a.cfg.CaddyContainer); err != nil {
			return "", fmt.Errorf("connect proxy to network: %w", err)
		}
	}
	if a.cfg.AgentContainer != "" {
		if err := a.dk.NetworkConnect(ctx, name, a.cfg.AgentContainer); err != nil {
			return "", fmt.Errorf("connect agent to network: %w", err)
		}
	}
	return name, nil
}

func (a *Agent) buildEnv(ctx context.Context, j job) ([]string, error) {
	env := make([]string, 0, len(j.Spec.Env)+8)
	// Shared project variables first; the service's own variables and secrets below
	// override them (dedupEnv keeps the last value of a repeated name).
	prows, err := a.pool.Query(ctx, `SELECT name, value_enc FROM project_variables WHERE project_id = $1 ORDER BY name`, j.ProjectID)
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
		plain, err := a.box.Open(enc, "projvar:"+j.ProjectID+":"+name)
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
	rows, err := a.pool.Query(ctx, `SELECT name, value_enc FROM secrets WHERE service_id = $1 ORDER BY name`, j.ServiceID)
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
		plain, err := a.box.Open(enc, "secret:"+j.ServiceID+":"+name)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", name, err)
		}
		env = append(env, name+"="+string(plain))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dedupEnv(append(env, "PORT="+strconv.Itoa(j.Spec.Port))), nil
}

// dedupEnv keeps one entry per name, the last one, at the position of its first.
func dedupEnv(env []string) []string {
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

func (a *Agent) containerConfig(j job, netName string, env, hosts []string) docker.ContainerConfig {
	hc := docker.HostConfig{
		Runtime:    a.cfg.Runtime,
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
	a.tenantDNS(&hc)
	cfg := docker.ContainerConfig{
		Image: j.Spec.Image,
		Env:   env,
		Labels: map[string]string{
			"platform.managed":    "true",
			"platform.org":        j.OrgID,
			"platform.project":    j.ProjectID,
			"platform.service":    j.ServiceID,
			"platform.deployment": j.ID,
			"platform.node":       a.selfID,
		},
		HostConfig: hc,
	}
	if !a.isPrimary() { // reachable only on the private (WireGuard) address
		port := strconv.Itoa(j.Spec.Port) + "/tcp"
		cfg.ExposedPorts = map[string]struct{}{port: {}}
		cfg.HostConfig.PortBindings = map[string][]docker.PortBinding{port: {{HostIP: a.cfg.NodeBind}}}
	}
	return cfg
}

func (a *Agent) waitHealthy(ctx context.Context, id, netName string, s model.Spec) error {
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.Now().Add(90 * time.Second)
	lastErr := errors.New("no answer yet")
	for time.Now().Before(deadline) {
		info, err := a.dk.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect container: %w", err)
		}
		if !info.State.Running {
			reason := fmt.Sprintf("exit code %d", info.State.ExitCode)
			if info.State.OOMKilled {
				reason = "killed: out of memory, raise the memory limit"
			}
			return fmt.Errorf("container stopped (%s)%s", reason, a.tail(ctx, id))
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
	return fmt.Errorf("not healthy after 90 s: %v%s", lastErr, a.tail(ctx, id))
}

// tail returns the last log lines of a container, for failure messages shown to the client.
func (a *Agent) tail(ctx context.Context, id string) string {
	rc, err := a.dk.ContainerLogs(ctx, id, 0, false, "15")
	if err != nil {
		return ""
	}
	defer rc.Close()
	var lines []string
	_ = docker.Demux(rc, func(_ byte, line string) {
		if _, msg, ok := strings.Cut(line, " "); ok {
			line = msg
		}
		lines = append(lines, truncate(line, 200))
	})
	if len(lines) == 0 {
		return ""
	}
	return "\n--- last log lines ---\n" + strings.Join(lines, "\n")
}

func (a *Agent) promote(ctx context.Context, j job, id, name, upstream string) ([]string, error) {
	var old []string
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE deployments SET status = 'superseded', updated_at = now()
			WHERE service_id = $1 AND status = 'live' AND id <> $2
			RETURNING coalesce(container_id, '')`, j.ServiceID, j.ID)
		if err != nil {
			return err
		}
		if old, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE deployments SET status = 'live', message = '', container_id = $2, container_name = $3, upstream = $4, updated_at = now()
			WHERE id = $1`, j.ID, id, name, upstream)
		return err
	})
	return old, err
}

// ---------- reconcile ----------

func (a *Agent) reconcile(ctx context.Context) {
	type live struct {
		DepID, ContainerID, OrgID, ProjectID, ServiceID, Upstream string
		Suspended                                                 bool
	}
	rows, err := a.pool.Query(ctx, `
		SELECT d.id, coalesce(d.container_id, ''), d.org_id, s.project_id, d.service_id, d.upstream, o.suspended
		FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id
		JOIN projects p ON p.id = s.project_id
		WHERE d.status = 'live' AND `+mineSQL, a.mineArgs()...)
	if err != nil {
		a.log.Error("reconcile: load live deployments", "err", err)
		return
	}
	lives, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (live, error) {
		var l live
		err := r.Scan(&l.DepID, &l.ContainerID, &l.OrgID, &l.ProjectID, &l.ServiceID, &l.Upstream, &l.Suspended)
		return l, err
	})
	if err != nil {
		a.log.Error("reconcile: scan", "err", err)
		return
	}
	rows, err = a.pool.Query(ctx, `
		SELECT d.id::text FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id
		WHERE d.status IN ('live', 'deploying', 'pending') AND `+mineSQL, a.mineArgs()...)
	if err != nil {
		a.log.Error("reconcile: load active ids", "err", err)
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

	containers, err := a.dk.ContainerList(ctx, "platform.managed=true")
	if err != nil {
		a.log.Error("reconcile: list containers", "err", err)
		return
	}
	// A project moved to another node: its old container keeps serving until the new
	// node's deployment is live (that supersedes this one), then it is removed here.
	var mine []string
	for _, c := range containers {
		if c.Labels["platform.database"] == "" && a.owns(c.Labels) && !keep[c.Labels["platform.deployment"]] {
			mine = append(mine, c.ID)
		}
	}
	if len(mine) > 0 {
		if rows, err := a.pool.Query(ctx, `SELECT id::text FROM deployments WHERE status = 'live' AND container_id = ANY($1)`, mine); err == nil {
			if ids, err := pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
				for _, id := range ids {
					keep[id] = true
				}
			}
		}
	}
	byDep := map[string]docker.ContainerSummary{}
	for _, c := range containers {
		if c.Labels["platform.database"] != "" || !a.owns(c.Labels) {
			continue
		}
		dep := c.Labels["platform.deployment"]
		if !keep[dep] {
			a.log.Info("removing orphan container", "container", strings.Join(c.Names, ","))
			_ = a.dk.ContainerStop(ctx, c.ID, 10)
			_ = a.dk.ContainerRemove(ctx, c.ID)
			continue
		}
		byDep[dep] = c
	}

	a.mu.Lock()
	a.ticks++
	force := a.ticks%4 == 0 // re-verify network attachments and push Caddy config every minute
	a.mu.Unlock()

	// A recreated proxy or agent container loses its tenant network attachments
	// (on every upgrade, for instance). Re-attach both to every live project network,
	// otherwise routes break and health probes report healthy apps as down.
	for _, l := range lives {
		if l.Suspended || !a.isPrimary() && a.cfg.AgentContainer == "" {
			continue
		}
		netName := model.NetworkName(l.ProjectID)
		a.mu.Lock()
		known := a.attached[netName]
		a.mu.Unlock()
		if known && !force {
			continue
		}
		var err error
		if a.isPrimary() {
			err = a.dk.NetworkConnect(ctx, netName, a.cfg.CaddyContainer)
		}
		if err == nil && a.cfg.AgentContainer != "" {
			err = a.dk.NetworkConnect(ctx, netName, a.cfg.AgentContainer)
		}
		a.mu.Lock()
		a.attached[netName] = err == nil
		a.mu.Unlock()
		if err != nil {
			a.log.Warn("attach to project network", "network", netName, "err", err)
		}
	}

	var running []target
	for _, l := range lives {
		c, ok := byDep[l.DepID]
		if l.Suspended {
			if ok && c.State == "running" {
				a.log.Info("stopping container of suspended organization", "org", l.OrgID)
				_ = a.dk.ContainerStop(ctx, c.ID, 10)
			}
			continue
		}
		if !ok && a.ranElsewhere(l.Upstream) {
			// The project was moved here (drain, placement): start a new revision on this
			// node; the old container keeps serving until it is live.
			tag, err := a.pool.Exec(ctx, `
				INSERT INTO deployments (org_id, service_id, revision, spec)
				SELECT d.org_id, d.service_id, (SELECT max(revision) + 1 FROM deployments WHERE service_id = d.service_id), d.spec
				FROM deployments d WHERE d.id = $1
				  AND NOT EXISTS (SELECT 1 FROM deployments x WHERE x.service_id = d.service_id AND (x.status IN ('pending', 'deploying')
				      OR (x.status = 'failed' AND x.revision > d.revision AND x.updated_at > now() - interval '5 minutes')))`, l.DepID)
			if err == nil && tag.RowsAffected() > 0 {
				_ = auditAgent(ctx, a, l.OrgID, "deployment.redeployed", "service:"+l.ServiceID, map[string]string{"reason": "project moved to node " + a.selfID})
				a.log.Info("project moved here, redeploying", "service", l.ServiceID, "node", a.selfID)
				a.poke()
			}
			continue
		}
		if !ok {
			a.log.Warn("live container missing, redeploying", "deployment", l.DepID)
			_, _ = a.pool.Exec(ctx, `UPDATE deployments SET status = 'pending', message = 'container was lost, redeploying', updated_at = now() WHERE id = $1`, l.DepID)
			a.poke()
			continue
		}
		if c.State != "running" {
			if err := a.dk.ContainerStart(ctx, c.ID); err != nil {
				a.log.Warn("restart container", "deployment", l.DepID, "err", err)
				continue
			}
		}
		running = append(running, target{c.ID, l.DepID, l.OrgID, l.ProjectID, l.ServiceID})
	}

	a.mu.Lock()
	a.targets = running
	a.mu.Unlock()

	a.logs.ensure(ctx, running)
	a.reconcileDatabases(ctx)
	a.syncCaddy(ctx, force)
	if force {
		a.collectNetworks(ctx)
	}
}

func (a *Agent) currentTargets() []target {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]target(nil), a.targets...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func auditAgent(ctx context.Context, a *Agent, orgID, action, target string, detail any) error {
	return db.Audit(ctx, a.pool, orgID, "", action, target, detail, "")
}

func firstLine(s string) string {
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

func (a *Agent) createProjectNetwork(ctx context.Context, name string, j job) error {
	labels := map[string]string{"platform.managed": "true", "platform.org": j.OrgID, "platform.project": j.ProjectID, "platform.node": a.selfID}
	var err error
	for attempt := 0; attempt < 64; attempt++ {
		err = a.dk.NetworkCreate(ctx, name, subnetCandidate(j.ProjectID, attempt), labels)
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
	rows, err := a.pool.Query(ctx, `
		SELECT DISTINCT s.project_id::text FROM deployments d JOIN services s ON s.id = d.service_id
		JOIN projects p ON p.id = s.project_id
		WHERE d.status IN ('live', 'deploying', 'pending') AND `+mineSQL+`
		UNION SELECT db.project_id::text FROM databases db JOIN projects p ON p.id = db.project_id WHERE `+mineSQL,
		a.mineArgs()...)
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
	nets, err := a.dk.NetworkList(ctx, "platform.managed=true")
	if err != nil {
		return
	}
	for _, n := range nets {
		if needed[n.Labels["platform.project"]] || !a.owns(n.Labels) {
			continue
		}
		// Leave it alone while a tenant container still uses it (for instance a project
		// that moved to another node sharing this Docker host): detaching the proxy
		// first would cut that app off until the next attachment check.
		if full, err := a.dk.NetworkInspect(ctx, n.Name); err != nil || a.hasTenantEndpoints(full) {
			continue
		}
		_ = a.dk.NetworkDisconnect(ctx, n.Name, a.cfg.CaddyContainer)
		if a.cfg.AgentContainer != "" {
			_ = a.dk.NetworkDisconnect(ctx, n.Name, a.cfg.AgentContainer)
		}
		if err := a.dk.NetworkRemove(ctx, n.Name); err != nil {
			continue // tenant containers still attached; retried on a later pass
		}
		a.mu.Lock()
		delete(a.attached, n.Name)
		a.mu.Unlock()
		a.log.Info("removed unused project network", "network", n.Name)
	}
}

// ---------- name resolution for sandboxed containers ----------

// tenantDNS mounts TENANT_RESOLV_CONF (read-only) as the container's resolv.conf.
// gVisor's network stack cannot reach Docker's embedded DNS (127.0.0.11) on
// user-defined networks, so without this, sandboxed containers resolve nothing.
func (a *Agent) tenantDNS(hc *docker.HostConfig) {
	if a.cfg.TenantResolvConf == "" {
		return
	}
	hc.Mounts = append(hc.Mounts, docker.Mount{Type: "bind", Source: a.cfg.TenantResolvConf, Target: "/etc/resolv.conf", ReadOnly: true})
}

func (a *Agent) withDNS(hc docker.HostConfig) docker.HostConfig {
	a.tenantDNS(&hc)
	return hc
}

// databaseAddress picks a fixed address for a managed database on its project network,
// in .200-.249 of the /24 (Docker hands out addresses from the bottom), derived from the
// database ID so a recreated container usually gets the same one.
func (a *Agent) databaseAddress(ctx context.Context, netName, dbID string) (string, error) {
	n, err := a.dk.NetworkInspect(ctx, netName)
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
func (a *Agent) projectHosts(ctx context.Context, projectID, netName string) []string {
	rows, err := a.pool.Query(ctx, `SELECT id, container_id FROM databases WHERE project_id = $1 AND status = 'running' AND container_id <> ''`, projectID)
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
	for _, d := range list {
		info, err := a.dk.ContainerInspect(ctx, d.cid)
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
// ranElsewhere reports whether a live deployment with this upstream was started by
// another node: the primary never records an upstream, a worker records its own address.
func (a *Agent) ranElsewhere(upstream string) bool {
	if upstream == "" {
		return !a.isPrimary()
	}
	return a.cfg.NodeAddress == "" || !strings.HasPrefix(upstream, a.cfg.NodeAddress+":")
}

// hasTenantEndpoints reports whether a container other than the proxy or an agent is
// attached to the network.
func (a *Agent) hasTenantEndpoints(n *docker.Network) bool {
	for _, c := range n.Containers {
		switch {
		case c.Name == a.cfg.CaddyContainer, c.Name == a.cfg.AgentContainer, strings.HasPrefix(c.Name, "platform-agent"):
		default:
			return true
		}
	}
	return false
}

func (a *Agent) owns(labels map[string]string) bool {
	n := labels["platform.node"]
	if n == "" {
		return a.isPrimary()
	}
	return n == a.selfID
}
