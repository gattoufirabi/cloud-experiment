// Package nodeagent runs on a customer-owned server ("platform node join|run"). It is
// the untrusted side of internal/nodeproto: it talks to the API only, with its node
// token, runs the deployments the API lists for this server in Docker, reports their
// status, ships their logs and statistics, and keeps tunnel connections open so the
// platform's proxy can reach the applications without any open port on the server.
package nodeagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"platform/internal/docker"
	"platform/internal/nodeproto"
)

// Version is reported to the console.
var Version = "1"

type Options struct {
	Server     string // console address, e.g. https://console.example.com
	HostHeader string // optional Host header (development: reach the proxy by its container name)
	StateDir   string
	Socket     string // Docker socket
	// SelfContainer: set when the agent itself runs in a container; it then joins each
	// project network to health-check and reach the instances.
	SelfContainer string
}

type state struct {
	Server        string `json:"server"`
	HostHeader    string `json:"host_header,omitempty"`
	NodeID        string `json:"node_id"`
	Token         string `json:"token"`
	RegistryToken string `json:"registry_token"`
}

func statePath(dir string) string { return filepath.Join(dir, "node.json") }

// Join exchanges a one-time join token for this server's credentials and stores them.
func Join(ctx context.Context, o Options, joinToken string) error {
	if _, err := os.Stat(statePath(o.StateDir)); err == nil {
		return fmt.Errorf("this server already joined (%s exists); remove it from the console and delete that file to join again", statePath(o.StateDir))
	}
	a := &Agent{opt: o, hc: &http.Client{Timeout: 30 * time.Second}, dk: docker.New(o.Socket), log: slog.Default()}
	a.st.Server, a.st.HostHeader = strings.TrimRight(o.Server, "/"), o.HostHeader
	facts := a.facts(ctx)
	var resp struct {
		NodeID        string `json:"node_id"`
		Token         string `json:"token"`
		RegistryToken string `json:"registry_token"`
	}
	if err := a.call(ctx, http.MethodPost, "/node/v1/join", nodeproto.JoinRequest{Token: joinToken, Facts: facts}, &resp); err != nil {
		return err
	}
	a.st.NodeID, a.st.Token, a.st.RegistryToken = resp.NodeID, resp.Token, resp.RegistryToken
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(a.st, "", "  ")
	if err := os.WriteFile(statePath(o.StateDir), b, 0o600); err != nil {
		return err
	}
	fmt.Printf("Joined as %s.\n", resp.NodeID)
	return nil
}

type Agent struct {
	opt Options
	st  state
	hc  *http.Client
	dk  *docker.Client
	log *slog.Logger

	mu        sync.Mutex
	busy      map[string]bool               // deployments being started
	queued    map[string]bool               // deployments waiting for the deploy worker
	gone      map[string]time.Time          // containers whose deployment left the desired state
	targets   map[string][]string           // deployment -> instance addresses (ip:port)
	rr        map[string]int                // round robin per deployment
	following map[string]context.CancelFunc // container ID -> log follower
	work      chan nodeproto.Deployment
	logs      chan nodeproto.LogLine
	cpuPrev   map[string][2]uint64 // container -> (cpu total, sampled at ns)
	reg       string               // the platform registry's host (its images need the server's token)
}

// Run is the agent's main loop.
func Run(ctx context.Context, o Options) error {
	b, err := os.ReadFile(statePath(o.StateDir))
	if err != nil {
		return fmt.Errorf("not joined yet: run \"node join\" first (%w)", err)
	}
	a := &Agent{opt: o, hc: &http.Client{Timeout: 30 * time.Second}, dk: docker.New(o.Socket), log: slog.Default().With("component", "node"),
		busy: map[string]bool{}, queued: map[string]bool{}, gone: map[string]time.Time{}, targets: map[string][]string{}, rr: map[string]int{},
		following: map[string]context.CancelFunc{}, work: make(chan nodeproto.Deployment, 64), logs: make(chan nodeproto.LogLine, 10000),
		cpuPrev: map[string][2]uint64{}}
	if err := json.Unmarshal(b, &a.st); err != nil {
		return err
	}
	if o.Server != "" { // the console address may change (new domain)
		a.st.Server = strings.TrimRight(o.Server, "/")
	}
	if o.HostHeader != "" {
		a.st.HostHeader = o.HostHeader
	}
	a.log.Info("node agent starting", "node", a.st.NodeID, "server", a.st.Server)
	go a.deployWorker(ctx)
	go a.shipLogs(ctx)
	for i := 0; i < 4; i++ {
		go a.tunnelWorker(ctx, i)
	}
	go a.every(ctx, 30*time.Second, a.heartbeat)
	go a.every(ctx, 60*time.Second, a.stats)
	go a.every(ctx, 5*time.Second, a.followLogs)
	a.every(ctx, 5*time.Second, a.sync)
	return nil
}

func (a *Agent) every(ctx context.Context, d time.Duration, f func(context.Context)) {
	f(ctx)
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f(ctx)
		}
	}
}

// ---------- API ----------

func (a *Agent) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.st.Server+path, body)
	if err != nil {
		return err
	}
	if a.st.HostHeader != "" {
		req.Host = a.st.HostHeader
	}
	if a.st.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.st.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (a *Agent) report(ctx context.Context, dep, status, msg string, instances []string) {
	rep := nodeproto.StatusReport{Status: status, Message: msg, Instances: instances}
	for attempt := 0; attempt < 5; attempt++ {
		err := a.call(ctx, http.MethodPost, "/node/v1/deployments/"+dep+"/status", rep, nil)
		if err == nil || strings.Contains(err.Error(), "HTTP 4") {
			return
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
}

// ---------- facts and heartbeat ----------

func (a *Agent) facts(ctx context.Context) nodeproto.Facts {
	f := nodeproto.Facts{OS: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: Version, CPUs: runtime.NumCPU()}
	f.Hostname, _ = os.Hostname()
	if info, err := a.dk.Info(ctx); err == nil {
		f.DockerVersion, f.Containers, f.CPUs = info.ServerVersion, info.Containers, info.NCPU
		f.MemMB = info.MemTotal >> 20
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
				kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
				f.MemAvailMB = kb >> 10
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fs := strings.Fields(string(b)); len(fs) > 0 {
			f.Load1, _ = strconv.ParseFloat(fs[0], 64)
		}
	}
	f.DiskTotalMB, f.DiskFreeMB = diskMB("/")
	return f
}

func (a *Agent) heartbeat(ctx context.Context) {
	if err := a.call(ctx, http.MethodPost, "/node/v1/heartbeat", a.facts(ctx), nil); err != nil {
		a.log.Warn("heartbeat", "err", err)
	}
}

// ---------- desired state ----------

func instanceName(d nodeproto.Deployment, i int) string {
	return fmt.Sprintf("pn-%s-r%d-%d", compact(d.ServiceID, 12), d.Revision, i)
}

func networkName(projectID string) string { return "pn-" + compact(projectID, 12) }

func compact(id string, n int) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > n {
		return id[:n]
	}
	return id
}

func (a *Agent) sync(ctx context.Context) {
	var d nodeproto.Desired
	if err := a.call(ctx, http.MethodGet, "/node/v1/desired", nil, &d); err != nil {
		a.log.Warn("desired state", "err", err)
		return
	}
	a.mu.Lock()
	a.reg = d.Registry.Host
	a.mu.Unlock()
	want := map[string]nodeproto.Deployment{}
	for _, dep := range d.Deployments {
		want[dep.ID] = dep
		switch dep.Status {
		case "live":
			a.mu.Lock()
			busy := a.busy[dep.ID]
			a.mu.Unlock()
			if !busy {
				a.ensureLive(ctx, dep)
			}
		default: // pending or deploying (an interrupted start is started again)
			a.mu.Lock()
			if !a.busy[dep.ID] && !a.queued[dep.ID] {
				a.queued[dep.ID] = true
				select {
				case a.work <- dep:
				default:
					delete(a.queued, dep.ID)
				}
			}
			a.mu.Unlock()
		}
	}
	// Containers of deployments that left the desired state are removed after a grace
	// period, so requests the proxy still sends them (before it switches) are served.
	cs, err := a.dk.ContainerList(ctx, nodeproto.LabelManaged+"=true")
	if err != nil {
		a.log.Warn("list containers", "err", err)
		return
	}
	now := time.Now()
	a.mu.Lock()
	var remove []string
	seen := map[string]bool{}
	for _, c := range cs {
		dep := c.Labels[nodeproto.LabelDeployment]
		seen[dep] = true
		if _, ok := want[dep]; ok || a.busy[dep] {
			delete(a.gone, dep)
			continue
		}
		since, ok := a.gone[dep]
		if !ok {
			a.gone[dep] = now
			continue
		}
		if now.Sub(since) > 20*time.Second {
			remove = append(remove, c.ID)
		}
	}
	for dep := range a.gone {
		if !seen[dep] {
			delete(a.gone, dep)
			delete(a.targets, dep)
		}
	}
	a.mu.Unlock()
	for _, id := range remove {
		_ = a.dk.ContainerStop(ctx, id, 10)
		_ = a.dk.ContainerRemove(ctx, id)
	}
	a.refreshTargets(ctx, cs)
}

// ensureLive restarts or recreates the instances of a live deployment that went missing
// (a reboot, a container removed by hand).
func (a *Agent) ensureLive(ctx context.Context, d nodeproto.Deployment) {
	cs, err := a.dk.ContainerList(ctx, nodeproto.LabelDeployment+"="+d.ID)
	if err != nil {
		return
	}
	have := map[string]docker.ContainerSummary{}
	for _, c := range cs {
		if len(c.Names) > 0 {
			have[strings.TrimPrefix(c.Names[0], "/")] = c
		}
	}
	for i := 0; i < max(1, d.Instances); i++ {
		name := instanceName(d, i)
		c, ok := have[name]
		switch {
		case ok && c.State == "running":
		case ok:
			_ = a.dk.ContainerStart(ctx, c.ID)
		default:
			netName, err := a.ensureNetwork(ctx, d)
			if err != nil {
				continue
			}
			if err := a.pull(ctx, d.Image); err != nil {
				a.log.Warn("pull", "image", d.Image, "err", err)
				continue
			}
			if id, err := a.dk.ContainerCreate(ctx, name, a.containerConfig(d, netName, i)); err == nil {
				_ = a.dk.ContainerStart(ctx, id)
				a.log.Info("instance recreated", "deployment", d.ID, "name", name)
			}
		}
	}
}

func (a *Agent) refreshTargets(ctx context.Context, cs []docker.ContainerSummary) {
	next := map[string][]string{}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		port := c.Labels["staysa.node.port"]
		if port == "" {
			continue
		}
		info, err := a.dk.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}
		net := c.Labels["staysa.node.network"]
		if ip := info.NetworkSettings.Networks[net].IPAddress; ip != "" {
			dep := c.Labels[nodeproto.LabelDeployment]
			next[dep] = append(next[dep], net2addr(ip, port))
		}
	}
	a.mu.Lock()
	a.targets = next
	a.mu.Unlock()
}

func net2addr(ip, port string) string { return net.JoinHostPort(ip, port) }

// ---------- deployments ----------

func (a *Agent) deployWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-a.work:
			a.mu.Lock()
			delete(a.queued, d.ID)
			a.busy[d.ID] = true
			a.mu.Unlock()
			a.log.Info("deploying", "deployment", d.ID, "service", d.Service, "revision", d.Revision, "image", d.Image)
			names, err := a.deploy(ctx, d)
			if err != nil {
				a.log.Warn("deployment failed", "deployment", d.ID, "err", err)
				a.report(ctx, d.ID, "failed", err.Error(), nil)
			} else {
				a.report(ctx, d.ID, "live", "", names)
				a.log.Info("deployment live", "deployment", d.ID)
			}
			a.mu.Lock()
			delete(a.busy, d.ID)
			a.mu.Unlock()
		}
	}
}

func (a *Agent) pull(ctx context.Context, image string) error {
	auth := ""
	host, _, _ := strings.Cut(image, "/")
	if a.registryHost() != "" && host == a.registryHost() {
		auth = docker.RegistryAuth("server", a.st.RegistryToken, host)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return a.dk.ImagePull(pctx, image, auth)
}

func (a *Agent) registryHost() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reg
}

func (a *Agent) ensureNetwork(ctx context.Context, d nodeproto.Deployment) (string, error) {
	name := networkName(d.ProjectID)
	n, err := a.dk.NetworkInspect(ctx, name)
	switch {
	case docker.IsNotFound(err):
		if err := a.dk.NetworkCreateAuto(ctx, name, map[string]string{nodeproto.LabelManaged: "true", nodeproto.LabelProject: d.ProjectID}); err != nil {
			return "", fmt.Errorf("create network: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("inspect network: %w", err)
	case n.Labels[nodeproto.LabelProject] != d.ProjectID:
		return "", fmt.Errorf("network %s exists and is not managed by this agent", name)
	}
	if a.opt.SelfContainer != "" {
		_ = a.dk.NetworkConnect(ctx, name, a.opt.SelfContainer)
	}
	return name, nil
}

func (a *Agent) containerConfig(d nodeproto.Deployment, netName string, i int) docker.ContainerConfig {
	hc := docker.HostConfig{
		Memory:        int64(d.MemoryMB) << 20,
		MemorySwap:    int64(d.MemoryMB) << 20,
		NanoCPUs:      int64(d.CPUMillis) * 1_000_000,
		PidsLimit:     512,
		CapDrop:       []string{"ALL"},
		CapAdd:        []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID", "NET_BIND_SERVICE", "KILL"},
		SecurityOpt:   []string{"no-new-privileges:true"},
		NetworkMode:   netName,
		RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		LogConfig:     docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	if d.VolumePath != "" {
		hc.Mounts = []docker.Mount{{Type: "volume", Source: "pn-vol-" + compact(d.ServiceID, 32), Target: d.VolumePath}}
	}
	labels := map[string]string{
		nodeproto.LabelManaged: "true", nodeproto.LabelDeployment: d.ID, nodeproto.LabelService: d.ServiceID,
		nodeproto.LabelProject: d.ProjectID, nodeproto.LabelInstance: strconv.Itoa(i), "staysa.node.network": netName,
	}
	if !d.Worker {
		labels["staysa.node.port"] = strconv.Itoa(d.Port)
	}
	// Services of a project reach each other by name on their network, as on the platform.
	aliases := []string{d.Service, d.Service + "." + d.Project + ".internal"}
	return docker.ContainerConfig{
		Image: d.Image, Env: d.Env, Labels: labels, HostConfig: hc,
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointConfig{netName: {Aliases: aliases}}},
	}
}

// deploy starts every instance, waits until all are healthy, and returns their names.
// The previous version keeps running until the proxy has switched (see sync).
func (a *Agent) deploy(ctx context.Context, d nodeproto.Deployment) ([]string, error) {
	a.report(ctx, d.ID, "deploying", "pulling image "+d.Image, nil)
	netName, err := a.ensureNetwork(ctx, d)
	if err != nil {
		return nil, err
	}
	if err := a.pull(ctx, d.Image); err != nil {
		return nil, fmt.Errorf("could not pull image %s: %w", d.Image, err)
	}
	n := max(1, d.Instances)
	a.report(ctx, d.ID, "", fmt.Sprintf("starting %d instance(s) on your server", n), nil)
	var ids, names []string
	cleanup := func() {
		for _, id := range ids {
			_ = a.dk.ContainerRemove(context.Background(), id)
		}
	}
	for i := 0; i < n; i++ {
		name := instanceName(d, i)
		_ = a.dk.ContainerRemove(ctx, name) // leftover from an interrupted attempt
		id, err := a.dk.ContainerCreate(ctx, name, a.containerConfig(d, netName, i))
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("create container: %w", err)
		}
		ids, names = append(ids, id), append(names, name)
		if err := a.dk.ContainerStart(ctx, id); err != nil {
			cleanup()
			return nil, fmt.Errorf("start container: %w", err)
		}
	}
	a.report(ctx, d.ID, "", "waiting for health check", nil)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = a.waitHealthy(ctx, ids[i], netName, d)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			cleanup()
			if n > 1 {
				return nil, fmt.Errorf("instance %d of %d: %w", i+1, n, err)
			}
			return nil, err
		}
	}
	// Make the new instances reachable through the tunnel before reporting live.
	if cs, err := a.dk.ContainerList(ctx, nodeproto.LabelManaged+"=true"); err == nil {
		a.refreshTargets(ctx, cs)
	}
	return names, nil
}

func (a *Agent) waitHealthy(ctx context.Context, id, netName string, d nodeproto.Deployment) error {
	if d.Worker {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(8 * time.Second):
		}
		info, err := a.dk.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !info.State.Running || info.RestartCount > 0 {
			return fmt.Errorf("worker stopped (exit code %d)%s", info.State.ExitCode, a.tail(ctx, id))
		}
		return nil
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(90 * time.Second)
	lastErr := errors.New("no answer yet")
	for time.Now().Before(deadline) {
		info, err := a.dk.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !info.State.Running {
			reason := fmt.Sprintf("exit code %d", info.State.ExitCode)
			if info.State.OOMKilled {
				reason = "killed: out of memory, raise the memory limit"
			}
			return fmt.Errorf("container stopped (%s)%s", reason, a.tail(ctx, id))
		}
		if ip := info.NetworkSettings.Networks[netName].IPAddress; ip != "" {
			resp, err := client.Get("http://" + net.JoinHostPort(ip, strconv.Itoa(d.Port)) + d.HealthPath)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				if resp.StatusCode < 500 {
					return nil
				}
				lastErr = fmt.Errorf("health check %s returned HTTP %d", d.HealthPath, resp.StatusCode)
			} else {
				lastErr = fmt.Errorf("nothing answering on port %d", d.Port)
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
		if len(line) > 200 {
			line = line[:200]
		}
		lines = append(lines, line)
	})
	if len(lines) == 0 {
		return ""
	}
	return "\n--- last log lines ---\n" + strings.Join(lines, "\n")
}

// ---------- tunnel ----------

// tunnelWorker keeps one idle tunnel connection open; when the API uses it, a new one
// is opened at once and the stream is served in the background.
func (a *Agent) tunnelWorker(ctx context.Context, n int) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := a.dialTunnel(ctx)
		if err != nil {
			a.log.Warn("tunnel", "worker", n, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		// Wait for the API to use it; recycle it every few minutes so middleboxes keep it.
		_ = conn.SetReadDeadline(time.Now().Add(4 * time.Minute))
		line, err := readLine(conn)
		if err != nil {
			_ = conn.Close()
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		go a.serveStream(conn, line)
	}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (a *Agent) dialTunnel(ctx context.Context) (net.Conn, error) {
	u, err := url.Parse(a.st.Server)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	if u.Scheme == "https" {
		conn, err = tls.DialWithDialer(d, "tcp", host, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, err
	}
	hostHeader := u.Host
	if a.st.HostHeader != "" {
		hostHeader = a.st.HostHeader
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	req := "GET /node/v1/tunnel HTTP/1.1\r\nHost: " + hostHeader + "\r\nAuthorization: Bearer " + a.st.Token +
		"\r\nConnection: Upgrade\r\nUpgrade: " + nodeproto.TunnelProtocol + "\r\nUser-Agent: platform-node/" + Version + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		conn.Close()
		return nil, fmt.Errorf("tunnel refused: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 512 {
		if _, err := c.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return strings.TrimRight(string(b), "\r"), nil
		}
		b = append(b, one[0])
	}
	return "", errors.New("line too long")
}

// serveStream connects the tunnel stream to one instance of the deployment.
func (a *Agent) serveStream(tun net.Conn, line string) {
	defer tun.Close()
	dep, ok := strings.CutPrefix(line, "OPEN ")
	if !ok {
		return
	}
	a.mu.Lock()
	targets := append([]string(nil), a.targets[dep]...)
	start := a.rr[dep]
	a.rr[dep] = start + 1
	a.mu.Unlock()
	if len(targets) == 0 {
		_, _ = io.WriteString(tun, "ERR no running instance of this deployment\n")
		return
	}
	var app net.Conn
	var err error
	for i := range targets {
		app, err = net.DialTimeout("tcp", targets[(start+i)%len(targets)], 5*time.Second)
		if err == nil {
			break
		}
	}
	if app == nil {
		_, _ = io.WriteString(tun, "ERR the application is not answering\n")
		return
	}
	defer app.Close()
	if _, err := io.WriteString(tun, "OK\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(app, tun)
		if tc, ok := app.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(tun, app)
		done <- struct{}{}
	}()
	<-done // either side finished: the HTTP exchange is over (one request per stream)
	<-time.After(10 * time.Millisecond)
}

// ---------- logs and statistics ----------

func (a *Agent) followLogs(ctx context.Context) {
	cs, err := a.dk.ContainerList(ctx, nodeproto.LabelManaged+"=true")
	if err != nil {
		return
	}
	running := map[string]bool{}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		running[c.ID] = true
		a.mu.Lock()
		_, ok := a.following[c.ID]
		if !ok {
			fctx, cancel := context.WithCancel(ctx)
			a.following[c.ID] = cancel
			go a.follow(fctx, c.ID, c.Labels[nodeproto.LabelDeployment])
		}
		a.mu.Unlock()
	}
	a.mu.Lock()
	for id, cancel := range a.following {
		if !running[id] {
			cancel()
			delete(a.following, id)
		}
	}
	a.mu.Unlock()
}

func (a *Agent) follow(ctx context.Context, id, dep string) {
	defer func() {
		a.mu.Lock()
		delete(a.following, id)
		a.mu.Unlock()
	}()
	rc, err := a.dk.ContainerLogs(ctx, id, time.Now().Unix(), true, "")
	if err != nil {
		return
	}
	defer rc.Close()
	_ = docker.Demux(rc, func(stream byte, line string) {
		l := nodeproto.LogLine{Deployment: dep, Stream: "stdout", Msg: line, Time: time.Now()}
		if stream == 2 {
			l.Stream = "stderr"
		}
		if ts, msg, ok := strings.Cut(line, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				l.Time, l.Msg = t, msg
			}
		}
		select {
		case a.logs <- l:
		default: // the API is behind: drop rather than grow without bound
		}
	})
}

func (a *Agent) shipLogs(ctx context.Context) {
	var batch []nodeproto.LogLine
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := a.call(ctx, http.MethodPost, "/node/v1/logs", nodeproto.LogBatch{Lines: batch}, nil); err != nil {
			a.log.Warn("ship logs", "err", err, "lines", len(batch))
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-a.logs:
			batch = append(batch, l)
			if len(batch) >= 500 {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

func (a *Agent) stats(ctx context.Context) {
	cs, err := a.dk.ContainerList(ctx, nodeproto.LabelManaged+"=true")
	if err != nil {
		return
	}
	type agg struct {
		cpu float64
		mem int
	}
	per := map[string]*agg{}
	now := uint64(time.Now().UnixNano())
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		st, err := a.dk.ContainerStats(ctx, c.ID)
		if err != nil {
			continue
		}
		dep := c.Labels[nodeproto.LabelDeployment]
		x := per[dep]
		if x == nil {
			x = &agg{}
			per[dep] = x
		}
		mem := st.MemoryStats.Usage
		if cache, ok := st.MemoryStats.Stats["inactive_file"]; ok && cache < mem {
			mem -= cache
		}
		x.mem += int(mem >> 20)
		total := st.CPUStats.CPUUsage.TotalUsage
		if prev, ok := a.cpuPrev[c.ID]; ok && now > prev[1] && total >= prev[0] {
			x.cpu += float64(total-prev[0]) / float64(now-prev[1]) * 100
		}
		a.cpuPrev[c.ID] = [2]uint64{total, now}
	}
	rep := nodeproto.StatsReport{}
	for dep, x := range per {
		rep.Deployments = append(rep.Deployments, nodeproto.DeploymentStats{Deployment: dep, CPUPct: x.cpu, MemMB: x.mem})
	}
	if len(rep.Deployments) > 0 {
		if err := a.call(ctx, http.MethodPost, "/node/v1/stats", rep, nil); err != nil {
			a.log.Warn("stats", "err", err)
		}
	}
}
