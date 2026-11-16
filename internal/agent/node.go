package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Node heartbeat, every 30 seconds: this server's capacity and load, disk space, and
// whether each platform service answers. The operator console shows it; stage 2 (more
// servers) uses the same table for placement.

// AgentVersion is reported in the heartbeat (set at build time with -ldflags).
var AgentVersion = "dev"

func (a *Agent) nodeLoop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		a.heartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) nodeID(ctx context.Context) (string, string) {
	if a.cfg.NodeID != "" {
		return a.cfg.NodeID, a.cfg.NodeID
	}
	info, err := a.dk.Info(ctx)
	if err != nil || info.Name == "" {
		h, _ := os.Hostname()
		return h, h
	}
	return info.Name, info.Name
}

func (a *Agent) heartbeat(ctx context.Context) {
	id, name := a.nodeID(ctx)
	info, err := a.dk.Info(ctx)
	if err != nil {
		a.log.Warn("node heartbeat", "err", err)
		return
	}
	total, free := diskSpaceMB("/")
	load1 := hostLoad1()
	avail := hostMemAvailableMB()
	services, _ := json.Marshal(a.checkPlatformServices(ctx))
	role := a.cfg.NodeRole
	if role == "" {
		role = "primary"
	}
	_, err = a.pool.Exec(ctx, `
		INSERT INTO nodes (id, name, cluster, role, address, cpus, mem_mb, mem_avail_mb, load1, disk_total_mb, disk_free_mb,
		                   containers, docker_version, agent_version, services, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now())
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, role = EXCLUDED.role, address = EXCLUDED.address,
		    cpus = EXCLUDED.cpus, mem_mb = EXCLUDED.mem_mb, mem_avail_mb = EXCLUDED.mem_avail_mb, load1 = EXCLUDED.load1,
		    disk_total_mb = EXCLUDED.disk_total_mb, disk_free_mb = EXCLUDED.disk_free_mb, containers = EXCLUDED.containers,
		    docker_version = EXCLUDED.docker_version, agent_version = EXCLUDED.agent_version, services = EXCLUDED.services,
		    last_seen = now()`,
		id, name, a.cfg.NodeCluster, role, a.cfg.NodeAddress, info.NCPU, info.MemTotal>>20, avail, load1, total, free,
		info.Containers, info.ServerVersion, AgentVersion, services)
	if err != nil {
		a.log.Warn("node heartbeat", "err", err)
	}
}

// checkPlatformServices asks each platform service whether it is up. Only the
// primary node runs the control plane, so workers report Docker only.
func (a *Agent) checkPlatformServices(ctx context.Context) map[string]string {
	out := map[string]string{"docker": "ok"}
	if a.cfg.NodeRole == "worker" {
		return out
	}
	hc := &http.Client{Timeout: 4 * time.Second}
	probe := func(name, url string, okCodes ...int) {
		if url == "" {
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := hc.Do(req)
		if err != nil {
			out[name] = "unreachable"
			return
		}
		resp.Body.Close()
		for _, c := range append(okCodes, http.StatusOK) {
			if resp.StatusCode == c {
				out[name] = "ok"
				return
			}
		}
		out[name] = "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	probe("api", a.cfg.APIInternalURL+"/healthz")
	if a.cfg.RegistryHost != "" {
		probe("registry", "http://"+a.cfg.RegistryUpstream+"/v2/", http.StatusUnauthorized)
	}
	if a.cfg.ScannerEnabled {
		// TRIVY_SERVER_URL is the address scan containers use; on a host-network setup
		// that is loopback, which the agent reaches by service name instead.
		u := strings.NewReplacer("127.0.0.1", "trivy", "localhost", "trivy").Replace(strings.TrimRight(a.cfg.TrivyServerURL, "/"))
		probe("scanner", u+"/healthz")
	}
	if a.cfg.VictoriaLogsURL != "" {
		probe("logs", a.cfg.VictoriaLogsURL+"/health")
	}
	if a.cfg.VictoriaMetricsURL != "" {
		probe("metrics", a.cfg.VictoriaMetricsURL+"/health")
	}
	if err := a.pool.Ping(ctx); err != nil {
		out["database"] = "unreachable"
	} else {
		out["database"] = "ok"
	}
	if _, err := a.caddy.get(ctx, "/config/"); err != nil {
		out["proxy"] = "unreachable"
	} else {
		out["proxy"] = "ok"
	}
	return out
}

// hostLoad1 and hostMemAvailableMB read /proc, which shows the host's values inside a
// container (load and memory are not namespaced).
func hostLoad1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func hostMemAvailableMB() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				return kb >> 10
			}
		}
	}
	return 0
}
