package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/docker"
	"platform/internal/functions"
	"platform/internal/model"
)

// Serverless functions. The API queues invocations in function_invocations and
// notifies "fn"; the agent of the project's node claims them (at most the function's
// concurrency at a time), runs each one in the function's warm container (created on
// the first call: a cold start), writes the result back and notifies "fn_done". A warm
// container is removed after 10 idle minutes (scale to zero). Containers run under the
// same sandbox as services, as an unprivileged user, on the project's network.

const (
	fnIdle     = 10 * time.Minute
	fnMaxSlots = 32 // invocations running at once on this node
	fnLogMax   = 32 << 10
)

type fnWarm struct {
	containerID, hash string
	webAddr           string // web apps: the server's address on the project network
	lastUsed          time.Time
	running           int
}

type fnState struct {
	mu       sync.Mutex
	warm     map[string]*fnWarm     // function ID -> warm container
	draining map[string]*fnWarm     // replaced containers still finishing invocations
	start    map[string]*sync.Mutex // serializes cold starts per function
	slots    chan struct{}
}

func newFnState() *fnState {
	return &fnState{warm: map[string]*fnWarm{}, draining: map[string]*fnWarm{}, start: map[string]*sync.Mutex{}, slots: make(chan struct{}, fnMaxSlots)}
}

func (a *Agent) pokeFn() {
	select {
	case a.fnWake <- struct{}{}:
	default:
	}
}

type fnConfig struct {
	ID, OrgID, ProjectID, Slug, ProjectSlug, OrgSlug, Runtime, Handler, CodeSHA string
	MemoryMB, TimeoutSeconds                                                    int
	Env                                                                         map[string]string
	SecretVersions                                                              string // referenced secrets and their versions
	WebCommand                                                                  string // web apps: the server's start command
	WebPort                                                                     int
}

func (f fnConfig) hash() string {
	keys := make([]string, 0, len(f.Env))
	for k := range f.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%d|%s|%s|%d|", f.Runtime, f.Handler, f.CodeSHA, f.MemoryMB, f.SecretVersions, f.WebCommand, f.WebPort)
	for _, k := range keys {
		b.WriteString(k + "=" + f.Env[k] + "\x00")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

func (a *Agent) fnLoop(ctx context.Context) {
	// Warm containers do not survive an agent restart: they hold no state.
	if cs, err := a.dk.ContainerList(ctx, "platform.function"); err == nil {
		for _, c := range cs {
			if a.owns(c.Labels) {
				_ = a.dk.ContainerRemove(ctx, c.ID)
			}
		}
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var lastReap, lastPrune time.Time
	for {
		a.claimInvocations(ctx)
		a.scheduleFunctions(ctx)
		if time.Since(lastReap) > 30*time.Second {
			lastReap = time.Now()
			a.reapFunctions(ctx)
		}
		if a.isPrimary() && time.Since(lastPrune) > 10*time.Minute {
			lastPrune = time.Now()
			_, _ = a.pool.Exec(ctx, `DELETE FROM function_invocations WHERE created_at < now() - interval '7 days'`)
			_, _ = a.pool.Exec(ctx, `UPDATE function_invocations SET response = NULL WHERE response IS NOT NULL AND finished_at < now() - interval '15 minutes'`)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.fnWake:
		}
	}
}

type claimed struct {
	ID, FunctionID, Trigger string
	Event                   []byte
	Async                   bool
}

// claimInvocations starts queued invocations of this node's functions, within each
// function's concurrency and the node's slots.
func (a *Agent) claimInvocations(ctx context.Context) {
	free := cap(a.fn.slots) - len(a.fn.slots)
	if free <= 0 {
		return
	}
	rows, err := a.pool.Query(ctx, `
		WITH cand AS (
		    SELECT i.id, row_number() OVER (PARTITION BY i.function_id ORDER BY i.created_at) AS rn,
		           f.concurrency - (SELECT count(*) FROM function_invocations r WHERE r.function_id = f.id AND r.status = 'running') AS room,
		           i.created_at
		    FROM function_invocations i JOIN functions f ON f.id = i.function_id JOIN projects p ON p.id = f.project_id
		    JOIN orgs o ON o.id = f.org_id
		    WHERE i.status = 'queued' AND NOT o.suspended AND `+mineSQL+`)
		UPDATE function_invocations i SET status = 'running', started_at = now(), node_id = $3
		FROM (SELECT id FROM cand WHERE rn <= room ORDER BY created_at LIMIT $4) c
		WHERE i.id = c.id AND i.status = 'queued'
		RETURNING i.id, i.function_id, i.trigger, coalesce(i.event, '\x'::bytea), i.async`,
		append(a.mineArgs(), a.selfID, free)...)
	if err != nil {
		a.log.Warn("claim function invocations", "err", err)
		return
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[claimed])
	if err != nil {
		a.log.Warn("claim function invocations", "err", err)
		return
	}
	for _, c := range list {
		a.fn.slots <- struct{}{}
		go func(c claimed) {
			defer func() { <-a.fn.slots; a.pokeFn() }()
			a.runInvocation(ctx, c)
		}(c)
	}
}

func (a *Agent) loadFnConfig(ctx context.Context, id string) (fnConfig, error) {
	var f fnConfig
	var env []byte
	err := a.pool.QueryRow(ctx, `
		SELECT f.id, f.org_id, f.project_id, f.slug, p.slug, o.slug, f.runtime, f.handler, f.code_sha256, f.memory_mb, f.timeout_seconds, f.env, f.web_command, f.web_port,
		       coalesce((SELECT string_agg(s.name || ':' || s.current_version, ',' ORDER BY s.name) FROM org_secrets s
		                 WHERE s.org_id = f.org_id AND EXISTS (SELECT 1 FROM jsonb_each_text(f.env) e WHERE e.value = '${secret:' || s.name || '}')), '')
		FROM functions f JOIN projects p ON p.id = f.project_id JOIN orgs o ON o.id = f.org_id WHERE f.id = $1`, id).
		Scan(&f.ID, &f.OrgID, &f.ProjectID, &f.Slug, &f.ProjectSlug, &f.OrgSlug, &f.Runtime, &f.Handler, &f.CodeSHA, &f.MemoryMB, &f.TimeoutSeconds, &env, &f.WebCommand, &f.WebPort, &f.SecretVersions)
	if err != nil {
		return f, err
	}
	_ = json.Unmarshal(env, &f.Env)
	return f, nil
}

// fnResult is the runtime's JSON line.
type fnResult struct {
	OK           bool            `json:"ok"`
	Result       json.RawMessage `json:"result"`
	ErrorType    string          `json:"errorType"`
	ErrorMessage string          `json:"errorMessage"`
	Stack        []string        `json:"stack"`
}

func (a *Agent) runInvocation(ctx context.Context, c claimed) {
	start := time.Now()
	status, errType, errMsg, logText := "failed", "", "", ""
	var response []byte
	cold := false
	f, err := a.loadFnConfig(ctx, c.FunctionID)
	defer func() {
		dur := int(time.Since(start).Milliseconds())
		if c.Async && c.Trigger != "queue" { // a queue trigger reads batchItemFailures
			response = nil
		}
		_, uerr := a.pool.Exec(context.WithoutCancel(ctx), `
			UPDATE function_invocations SET status = $2, response = $3, error_type = $4, error = $5, log = $6, duration_ms = $7, cold = $8, finished_at = now(), event = NULL
			WHERE id = $1`, c.ID, status, response, errType, truncate(errMsg, 2000), logText, dur, cold)
		if uerr != nil {
			a.log.Warn("record function invocation", "err", uerr)
		}
		_, _ = a.pool.Exec(context.WithoutCancel(ctx), `SELECT pg_notify('fn_done', $1)`, c.ID)
		if c.Trigger == "queue" {
			a.pokeQueues()
		}
		if f.ID != "" {
			hour := time.Now().UTC().Truncate(time.Hour)
			_, _ = a.pool.Exec(context.WithoutCancel(ctx), `
				INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, fn_requests, fn_mb_ms) VALUES ($1, $2, 'function', $3, $4, 1, $5)
				ON CONFLICT (resource_id, hour) DO UPDATE SET fn_requests = usage_hourly.fn_requests + 1, fn_mb_ms = usage_hourly.fn_mb_ms + EXCLUDED.fn_mb_ms`,
				f.OrgID, f.ID, f.ProjectSlug+"/"+f.Slug, hour, int64(dur)*int64(f.MemoryMB))
		}
	}()
	if err != nil {
		errType, errMsg = "Platform.FunctionNotFound", "the function no longer exists"
		return
	}
	rt, ok := functions.FindRuntime(f.Runtime)
	if !ok {
		errType, errMsg = "Platform.InvalidRuntime", "unknown runtime "+f.Runtime
		return
	}
	w, wasCold, err := a.warmContainer(ctx, f, rt)
	cold = wasCold
	if err != nil {
		errType, errMsg = "Platform.StartFailed", err.Error()
		return
	}
	defer a.releaseWarm(f.ID, w)

	event := c.Event
	if len(event) == 0 {
		event = []byte("{}")
	}
	deadline := time.Now().Add(time.Duration(f.TimeoutSeconds) * time.Second)
	if f.WebCommand != "" {
		wctx, cancel := context.WithDeadline(ctx, deadline)
		out, err := webInvoke(wctx, w.webAddr, event, c.ID)
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(wctx.Err(), context.DeadlineExceeded):
			status, errType, errMsg = "timed_out", "Sandbox.Timedout", fmt.Sprintf("Task timed out after %d seconds", f.TimeoutSeconds)
		case err != nil:
			errType, errMsg = "WebApp.RequestFailed", err.Error()
			a.discardWarm(ctx, f.ID, w) // the server may have crashed: start a fresh one next time
		default:
			var res fnResult
			_ = json.Unmarshal(out, &res)
			response = out
			if res.OK {
				status = "succeeded"
			} else {
				errType, errMsg = res.ErrorType, res.ErrorMessage
			}
		}
		return
	}
	input, _ := json.Marshal(map[string]any{
		"event": json.RawMessage(event),
		"context": map[string]any{
			"functionName": f.Slug, "functionVersion": "$LATEST", "memoryLimitInMB": f.MemoryMB, "awsRequestId": c.ID,
			"invokedFunctionArn": "plat:function:" + f.OrgSlug + ":" + f.ProjectSlug + ":" + f.Slug,
			"logGroupName":       "/platform/functions/" + f.ProjectSlug + "/" + f.Slug, "logStreamName": c.ID, "deadlineMs": deadline.UnixMilli(),
		},
	})
	ectx, cancel := context.WithDeadline(ctx, deadline.Add(500*time.Millisecond))
	stdout, stderr, code, err := a.dk.ContainerExecStdin(ectx, w.containerID, rt.Command, []string{"AWS_REQUEST_ID=" + c.ID}, "/var/task", input)
	cancel()
	logText = truncate(string(stderr), fnLogMax)
	switch {
	// The Docker connection's own read deadline can fire a moment before the context's:
	// any error at or after the deadline is the function's timeout.
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ectx.Err(), context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) || (err != nil && !time.Now().Before(deadline)):
		status, errType, errMsg = "timed_out", "Sandbox.Timedout", fmt.Sprintf("Task timed out after %d seconds", f.TimeoutSeconds)
		a.discardWarm(ctx, f.ID, w) // the handler may still be running
		return
	case err != nil:
		errType, errMsg = "Platform.InvokeFailed", err.Error()
		a.discardWarm(ctx, f.ID, w)
		return
	}
	line := lastLine(stdout)
	var res fnResult
	if len(line) == 0 || json.Unmarshal(line, &res) != nil {
		errType = "Runtime.ExitError"
		errMsg = fmt.Sprintf("the runtime exited with code %d without a result", code)
		if code == 137 {
			errMsg = fmt.Sprintf("the runtime was stopped (exit code 137): out of memory? The function has %d MB", f.MemoryMB)
		}
		return
	}
	if len(line) > functions.MaxPayloadBytes {
		errType, errMsg = "Function.ResponseSizeTooLarge", fmt.Sprintf("the response is larger than %d MB", functions.MaxPayloadBytes>>20)
		return
	}
	response = line
	if res.OK {
		status = "succeeded"
	} else {
		errType, errMsg = res.ErrorType, res.ErrorMessage
	}
}

func lastLine(b []byte) []byte {
	b = bytes.TrimRight(b, "\r\n")
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return b[i+1:]
	}
	return b
}

func (a *Agent) fnStartLock(id string) *sync.Mutex {
	a.fn.mu.Lock()
	defer a.fn.mu.Unlock()
	m := a.fn.start[id]
	if m == nil {
		m = &sync.Mutex{}
		a.fn.start[id] = m
	}
	return m
}

// warmContainer returns the function's warm container, starting one if there is none
// or its settings changed (reports whether this was a cold start).
func (a *Agent) warmContainer(ctx context.Context, f fnConfig, rt functions.Runtime) (*fnWarm, bool, error) {
	hash := f.hash()
	lock := a.fnStartLock(f.ID)
	lock.Lock()
	defer lock.Unlock()
	a.fn.mu.Lock()
	w := a.fn.warm[f.ID]
	if w != nil && w.hash == hash {
		w.running++
		w.lastUsed = time.Now()
		a.fn.mu.Unlock()
		return w, false, nil
	}
	old := w
	delete(a.fn.warm, f.ID)
	a.fn.mu.Unlock()
	if old != nil {
		if old.running == 0 {
			_ = a.dk.ContainerRemove(ctx, old.containerID)
		} else { // removed by the reaper once its invocations finish
			a.fn.mu.Lock()
			a.fn.draining[old.containerID] = old
			a.fn.mu.Unlock()
		}
	}

	id, err := a.startFunctionContainer(ctx, f, rt, hash)
	if err != nil {
		return nil, true, err
	}
	w = &fnWarm{containerID: id, hash: hash, lastUsed: time.Now(), running: 1}
	if f.WebCommand != "" {
		addr, err := a.webAddr(ctx, id, f.WebPort)
		if err == nil {
			err = waitWebReady(ctx, addr, 60*time.Second, func() error {
				info, err := a.dk.ContainerInspect(ctx, id)
				if err != nil {
					return err
				}
				if !info.State.Running {
					return fmt.Errorf("the web app's process stopped (exit code %d) before listening", info.State.ExitCode)
				}
				return nil
			})
		}
		if err != nil {
			msg := err.Error() + a.tail(ctx, id)
			_ = a.dk.ContainerRemove(context.WithoutCancel(ctx), id)
			return nil, true, errors.New(msg)
		}
		w.webAddr = addr
	}
	a.fn.mu.Lock()
	a.fn.warm[f.ID] = w
	a.fn.mu.Unlock()
	return w, true, nil
}

func (a *Agent) releaseWarm(id string, w *fnWarm) {
	a.fn.mu.Lock()
	defer a.fn.mu.Unlock()
	w.running--
	w.lastUsed = time.Now()
}

// discardWarm drops a container that may hold a runaway handler.
func (a *Agent) discardWarm(ctx context.Context, id string, w *fnWarm) {
	a.fn.mu.Lock()
	if a.fn.warm[id] == w {
		delete(a.fn.warm, id)
	}
	a.fn.mu.Unlock()
	_ = a.dk.ContainerRemove(context.WithoutCancel(ctx), w.containerID)
}

func (a *Agent) startFunctionContainer(ctx context.Context, f fnConfig, rt functions.Runtime, hash string) (string, error) {
	if f.MemoryMB > a.cfg.MaxMemoryMB {
		return "", errors.New("rejected by policy: memory exceeds the host maximum")
	}
	if a.cfg.Runtime != "runsc" && !a.cfg.AllowInsecureRuntime {
		return "", errors.New("tenant containers must run under gVisor")
	}
	var code []byte
	if err := a.pool.QueryRow(ctx, `SELECT code FROM functions WHERE id = $1`, f.ID).Scan(&code); err != nil {
		return "", err
	}
	files, err := functions.ReadZip(code)
	if err != nil {
		return "", fmt.Errorf("code archive: %w", err)
	}
	tarball, err := functions.ContainerTar(rt, files)
	if err != nil {
		return "", err
	}
	netName, err := a.ensureNetwork(ctx, job{OrgID: f.OrgID, ProjectID: f.ProjectID})
	if err != nil {
		return "", err
	}
	env, err := a.functionEnv(ctx, f)
	if err != nil {
		return "", err
	}
	if _, err := a.dk.ImageID(ctx, rt.Image); err != nil {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err = a.dk.ImagePull(pctx, rt.Image, "")
		cancel()
		if err != nil {
			return "", fmt.Errorf("pull %s: %w", rt.Image, err)
		}
	}
	cpu := min(f.MemoryMB*2, 4000) // CPU grows with memory, as on AWS Lambda
	hc := docker.HostConfig{
		Runtime: a.cfg.Runtime, Memory: int64(f.MemoryMB) << 20, MemorySwap: int64(f.MemoryMB) << 20, NanoCPUs: int64(cpu) * 1_000_000,
		PidsLimit: 128, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, NetworkMode: netName,
		RestartPolicy: docker.RestartPolicy{Name: "no"}, ExtraHosts: a.projectHosts(ctx, f.ProjectID, netName),
		LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m", "max-file": "1"}},
	}
	a.tenantDNS(&hc)
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := "pf-" + model.Compact(f.ID)[:12] + "-" + hex.EncodeToString(suffix)
	entry, cmd := []string{"sleep"}, []string{"infinity"}
	if f.WebCommand != "" { // a web app: its server is the container's process
		entry, cmd = []string{"sh", "-c"}, []string{"cd /var/task && exec " + f.WebCommand}
		env = append(env, fmt.Sprintf("PORT=%d", f.WebPort), fmt.Sprintf("AWS_LWA_PORT=%d", f.WebPort))
	}
	id, err := a.dk.ContainerCreate(ctx, name, docker.ContainerConfig{
		Image: rt.Image, User: "65534:65534", Entrypoint: entry, Cmd: cmd, Env: env,
		Labels: map[string]string{"platform.function": f.ID, "platform.org": f.OrgID, "platform.project": f.ProjectID,
			"platform.node": a.selfID, "platform.spec": hash},
		HostConfig: hc,
	})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := a.dk.ContainerPutArchive(ctx, id, "/", tarball); err != nil {
		_ = a.dk.ContainerRemove(ctx, id)
		return "", fmt.Errorf("copy code: %w", err)
	}
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		_ = a.dk.ContainerRemove(ctx, id)
		return "", fmt.Errorf("start container: %w", err)
	}
	a.log.Info("function container started", "function", f.ID, "container", name)
	return id, nil
}

// functionEnv: shared project variables, the function's variables, then the runtime's.
func (a *Agent) functionEnv(ctx context.Context, f fnConfig) ([]string, error) {
	var env []string
	rows, err := a.pool.Query(ctx, `SELECT name, value_enc FROM project_variables WHERE project_id = $1 ORDER BY name`, f.ProjectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		var enc []byte
		if err := rows.Scan(&name, &enc); err != nil {
			rows.Close()
			return nil, err
		}
		plain, err := a.box.Open(enc, "projvar:"+f.ProjectID+":"+name)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("decrypt project variable %s: %w", name, err)
		}
		env = append(env, name+"="+string(plain))
	}
	rows.Close()
	keys := make([]string, 0, len(f.Env))
	for k := range f.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+f.Env[k])
	}
	env = append(env, "_HANDLER="+f.Handler, "LAMBDA_TASK_ROOT=/var/task", "HOME=/tmp", "TZ=UTC",
		"AWS_LAMBDA_FUNCTION_NAME="+f.Slug, "AWS_LAMBDA_FUNCTION_VERSION=$LATEST",
		fmt.Sprintf("AWS_LAMBDA_FUNCTION_MEMORY_SIZE=%d", f.MemoryMB), "PLATFORM_FUNCTION="+f.ProjectSlug+"/"+f.Slug)
	return a.resolveSecretRefs(ctx, f.OrgID, dedupEnv(env))
}

// reapFunctions removes idle warm containers (scale to zero) and containers of
// deleted or replaced functions.
func (a *Agent) reapFunctions(ctx context.Context) {
	cs, err := a.dk.ContainerList(ctx, "platform.function")
	if err != nil {
		return
	}
	alive := map[string]bool{}
	if rows, err := a.pool.Query(ctx, `SELECT f.id FROM functions f JOIN orgs o ON o.id = f.org_id WHERE NOT o.suspended`); err == nil {
		ids, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, id := range ids {
			alive[id] = true
		}
	}
	a.fn.mu.Lock()
	keep := map[string]bool{}
	for fid, w := range a.fn.warm {
		if !alive[fid] || (w.running == 0 && time.Since(w.lastUsed) > fnIdle) {
			delete(a.fn.warm, fid)
			continue
		}
		keep[w.containerID] = true
	}
	for cid, w := range a.fn.draining {
		if w.running > 0 {
			keep[cid] = true
		} else {
			delete(a.fn.draining, cid)
		}
	}
	a.fn.mu.Unlock()
	for _, c := range cs {
		if !a.owns(c.Labels) || keep[c.ID] {
			continue
		}
		if time.Since(time.Unix(c.Created, 0)) < time.Minute {
			continue // a cold start in progress
		}
		_ = a.dk.ContainerRemove(ctx, c.ID)
		a.log.Info("function container removed", "function", c.Labels["platform.function"])
	}
}

// scheduleFunctions queues the scheduled invocations that are due (an EventBridge-like
// "Scheduled Event").
func (a *Agent) scheduleFunctions(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT f.id, f.org_id, f.schedule, f.next_run_at FROM functions f JOIN projects p ON p.id = f.project_id JOIN orgs o ON o.id = f.org_id
		WHERE f.schedule_enabled AND f.schedule <> '' AND NOT o.suspended AND (f.next_run_at IS NULL OR f.next_run_at <= now()) AND `+mineSQL, a.mineArgs()...)
	if err != nil {
		return
	}
	type due struct {
		id, org, schedule string
		next              *time.Time
	}
	var list []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.org, &d.schedule, &d.next) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	for _, d := range list {
		next, ok := nextRun(d.schedule)
		if !ok {
			continue
		}
		if d.next != nil { // due now (a NULL next run only gets scheduled)
			event, _ := json.Marshal(map[string]any{"version": "0", "id": newID(), "detail-type": "Scheduled Event", "source": "platform.scheduler",
				"time": d.next.UTC().Format(time.RFC3339), "resources": []string{}, "detail": map[string]any{}})
			if _, err := a.pool.Exec(ctx, `INSERT INTO function_invocations (org_id, function_id, trigger, async, event) VALUES ($1, $2, 'schedule', true, $3)`,
				d.org, d.id, event); err != nil {
				continue
			}
		}
		_, _ = a.pool.Exec(ctx, `UPDATE functions SET next_run_at = $2 WHERE id = $1`, d.id, next)
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
