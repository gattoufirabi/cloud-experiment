package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"platform/internal/buildsrc"
	"platform/internal/docker"
	"platform/internal/nodeproto"
)

// Builds on this server (migrations 069, 070): when the organization turned builds on
// for it, the agent claims waiting builds (as many at once as the organization allows,
// up to maxBuildsHere) and runs the same steps as the platform's builder: the helper
// image clones the repository (or unpacks the uploaded archive) into a fresh volume,
// rootless BuildKit builds it into an image tarball, and the agent loads and pushes it
// with the build's push token. The API records the result and queues the deployment.
// The claim request is held by the API until a build comes (long polling), so a build
// starts about a second after it is queued.

const (
	buildNetwork  = "pn-build"
	maxBuildsHere = 8 // the API never hands more (nodes.max_builds)
	claimWait     = 20 * time.Second
)

func (a *Agent) buildWorker(ctx context.Context) {
	var mu sync.Mutex
	running := map[string]bool{}
	finished := make(chan struct{}, maxBuildsHere)
	pause := func(d time.Duration) bool { // until a build ends or d passes
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-finished:
		case <-t.C:
		}
		return true
	}
	for {
		mu.Lock()
		ids := make([]string, 0, len(running))
		for id := range running {
			ids = append(ids, id)
		}
		mu.Unlock()
		if len(ids) >= maxBuildsHere {
			if !pause(time.Minute) {
				return
			}
			continue
		}
		var job nodeproto.BuildJob
		start := time.Now()
		cctx := context.WithValue(ctx, longPollKey{}, true)
		err := a.call(cctx, http.MethodPost, "/node/v1/builds/claim", nodeproto.BuildClaim{Running: ids, WaitSec: int(claimWait.Seconds())}, &job)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			if !strings.Contains(err.Error(), "HTTP 401") {
				a.log.Warn("claim build", "err", err)
			}
			if !pause(5 * time.Second) {
				return
			}
		case job.ID != "":
			a.log.Info("build claimed", "build", job.ID, "number", job.Number)
			mu.Lock()
			running[job.ID] = true
			mu.Unlock()
			go func() {
				a.runBuild(ctx, job)
				mu.Lock()
				delete(running, job.ID)
				mu.Unlock()
				select {
				case finished <- struct{}{}:
				default:
				}
			}()
		case time.Since(start) < 2*time.Second:
			// Answered at once: builds are off here, or as many run as allowed.
			if !pause(10 * time.Second) {
				return
			}
		}
	}
}

// buildRun keeps the log and sends it to the API while the build runs.
type buildRun struct {
	a      *Agent
	job    nodeproto.BuildJob
	cancel context.CancelFunc
	mu     sync.Mutex
	log    strings.Builder
	extra  string // live output of the current step
}

func (b *buildRun) step(format string, args ...any) {
	b.mu.Lock()
	fmt.Fprintf(&b.log, "==> "+format+"\n", args...)
	b.extra = ""
	b.mu.Unlock()
	b.send()
}

func (b *buildRun) write(s string) {
	b.mu.Lock()
	b.log.WriteString(s)
	b.extra = ""
	b.mu.Unlock()
}

func (b *buildRun) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.log.String() + b.extra
}

// send posts the log so far. A 409 means the build was ended elsewhere (removed
// server, timeout on the platform side): stop working on it.
func (b *buildRun) send() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := b.a.call(ctx, http.MethodPost, "/node/v1/builds/"+b.job.ID+"/log", nodeproto.BuildLogReport{Log: b.text()}, nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 409") {
		b.cancel()
	}
}

func (a *Agent) runBuild(parent context.Context, job nodeproto.BuildJob) {
	timeout := time.Duration(job.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout+10*time.Minute)
	defer cancel()
	b := &buildRun{a: a, job: job, cancel: cancel}
	// Keep the build alive on the platform side even through a long silent step.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				b.send()
			}
		}
	}()
	commit, image, err := a.build(ctx, b, timeout)
	close(stop)
	res := nodeproto.BuildResult{Status: "done", Commit: commit, Image: image}
	if err != nil {
		if parent.Err() != nil {
			return // the agent is stopping: the platform fails the build when it notices
		}
		b.step("failed: %s", err.Error())
		res = nodeproto.BuildResult{Status: "failed", Error: err.Error()}
		a.log.Info("build failed", "build", job.ID, "err", err)
	} else {
		b.step("pushed %s", image)
		a.log.Info("build done", "build", job.ID, "image", image)
	}
	res.Log = b.text()
	for attempt := 0; attempt < 5; attempt++ {
		fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := a.call(fctx, http.MethodPost, "/node/v1/builds/"+job.ID+"/finish", res, nil)
		fcancel()
		if err == nil || strings.Contains(err.Error(), "HTTP 4") {
			return
		}
		time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
	}
}

func (a *Agent) build(ctx context.Context, b *buildRun, timeout time.Duration) (string, string, error) {
	job := b.job
	switch {
	case job.Zip:
		b.step("build #%d of the uploaded archive %s (sha256 %s), on this server", job.Number, job.SourceName, short(job.SourceSHA, 12))
	default:
		b.step("build #%d of %s (branch %s), on this server", job.Number, job.Repo, job.Branch)
	}
	if _, err := a.dk.ImageID(ctx, job.BuilderImage); err != nil {
		b.step("preparing the build helper image (git and Railpack; first build on this server only)")
		if err := a.dk.ImageBuild(ctx, job.BuilderImage, buildsrc.BuilderDockerfile); err != nil {
			return "", "", fmt.Errorf("build helper image: %w", err)
		}
	}
	if _, err := a.dk.ImageID(ctx, job.BuildkitImage); err != nil {
		if err := a.dk.ImagePull(ctx, job.BuildkitImage, ""); err != nil {
			return "", "", fmt.Errorf("pull %s: %w", job.BuildkitImage, err)
		}
	}
	if _, err := a.dk.NetworkInspect(ctx, buildNetwork); docker.IsNotFound(err) {
		if err := a.dk.NetworkCreateAuto(ctx, buildNetwork, map[string]string{"staysa.node.build-network": "true"}); err != nil && !strings.Contains(err.Error(), "already exists") {
			return "", "", fmt.Errorf("build network: %w", err)
		}
	}
	id := compact(job.ID, 12)
	vol, cache := "pn-build-"+id, "pn-bcache-"+compact(job.ServiceID, 12)
	labels := map[string]string{"staysa.node.build": job.ID}
	if err := a.dk.VolumeCreate(ctx, vol, labels); err != nil {
		return "", "", fmt.Errorf("create build volume: %w", err)
	}
	defer a.dk.VolumeRemove(context.Background(), vol)
	if job.Clean {
		// A clean build starts from nothing and leaves nothing: its own cache volume,
		// removed with the build.
		cache = "pn-bcache-clean-" + id
		b.step("clean build: no cache from earlier builds, none kept")
		if err := a.dk.VolumeCreate(ctx, cache, labels); err != nil {
			return "", "", fmt.Errorf("create cache volume: %w", err)
		}
		defer a.dk.VolumeRemove(context.Background(), cache)
	} else if err := a.dk.VolumeCreate(ctx, cache, map[string]string{"staysa.node.build-cache": job.ServiceID}); err != nil {
		return "", "", fmt.Errorf("create cache volume: %w", err)
	}

	// 1. source
	var tarball []byte
	script := buildsrc.Git + buildsrc.Common
	if job.Zip {
		zip, err := a.fetch(ctx, "/node/v1/builds/"+job.ID+"/source")
		if err != nil {
			return "", "", fmt.Errorf("download the archive: %w", err)
		}
		var n int
		if tarball, n, err = buildsrc.ZipToTar(zip); err != nil {
			return "", "", fmt.Errorf("archive: %w", err)
		}
		b.step("unpacking %d files", n)
		script = buildsrc.Zip + buildsrc.Common
	} else {
		b.step("cloning")
	}
	cloneID, code, out, err := a.runOnce(ctx, "pn-build-clone-"+id, docker.ContainerConfig{
		Image:      job.BuilderImage,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{script},
		Env: []string{"REPO=" + job.Repo, "BRANCH=" + job.Branch, "DIR=" + job.Dir, "TOKEN=" + job.Token, "SHA=" + job.SourceSHA,
			"HOME=/tmp", "GIT_TERMINAL_PROMPT=0"},
		Labels: labels,
		HostConfig: docker.HostConfig{
			NetworkMode: buildNetwork, Memory: 1 << 30, MemorySwap: 1 << 30, PidsLimit: 512,
			CapDrop: []string{"ALL"}, CapAdd: []string{"CHOWN"}, SecurityOpt: []string{"no-new-privileges:true"},
			ReadonlyRootfs: true, Tmpfs: map[string]string{"/tmp": "rw,exec,size=1g"},
			Mounts:    []docker.Mount{{Type: "volume", Source: vol, Target: "/src"}, {Type: "volume", Source: cache, Target: "/cache"}},
			LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "4m"}},
		},
	}, 5*time.Minute, nil, "/src", tarball)
	if cloneID != "" {
		defer a.dk.ContainerRemove(context.Background(), cloneID)
	}
	commit, mode := "", ""
	for _, line := range strings.Split(out, "\n") {
		if c, ok := strings.CutPrefix(strings.TrimSpace(line), "COMMIT="); ok {
			commit = c
		} else if m, ok := strings.CutPrefix(strings.TrimSpace(line), "MODE="); ok {
			mode = m
		} else if line != "" {
			b.write(line + "\n")
		}
	}
	if err != nil || code != 0 || len(commit) < 12 {
		msg := buildsrc.FailureMessage(code, job.Zip, job.Dir)
		if err != nil {
			msg += ": " + err.Error()
		}
		return "", "", errors.New(msg)
	}
	tag := "git-" + commit[:12]
	if job.Zip {
		tag = "zip-" + commit[:12]
	} else {
		b.step("commit %s", commit)
	}

	// 2. build
	image := job.ImageRepo + ":" + tag
	ctxDir := "/src/repo"
	if job.Dir != "" {
		ctxDir += "/" + job.Dir
	}
	args := []string{"build", "--progress", "plain", "--local", "context=" + ctxDir,
		"--output", "type=docker,name=" + image + ",dest=/src/out/image.tar,compression=uncompressed"}
	if mode == "railpack" {
		b.step("building %s with Railpack (no Dockerfile)", image)
		args = append(args, "--frontend", "gateway.v0", "--opt", "source="+job.RailpackFrontend,
			"--local", "dockerfile=/src/plan", "--opt", "build-arg:cache-key="+compact(job.ServiceID, 32))
	} else {
		b.step("building %s from the Dockerfile", image)
		args = append(args, "--frontend", "dockerfile.v0", "--local", "dockerfile="+ctxDir)
	}
	kitID, code, out, err := a.runOnce(ctx, "pn-build-kit-"+id, docker.ContainerConfig{
		Image:      job.BuildkitImage,
		Entrypoint: []string{"buildctl-daemonless.sh"},
		Cmd:        args,
		Env:        []string{"BUILDKITD_FLAGS=--oci-worker-no-process-sandbox"},
		Labels:     labels,
		HostConfig: docker.HostConfig{
			NetworkMode: buildNetwork, PidsLimit: 4096,
			// Rootless BuildKit maps its user namespace with newuidmap/newgidmap (setuid);
			// every other capability is dropped. Docker's default seccomp and AppArmor
			// profiles block the mounts it needs; buildSeccomp allows those and still
			// refuses the system calls a build never needs (see seccomp.go). AppArmor
			// profiles must be loaded on the host, which the agent cannot do.
			CapDrop: []string{"ALL"}, CapAdd: []string{"SETUID", "SETGID"},
			SecurityOpt: []string{"seccomp=" + buildSeccomp, "apparmor=unconfined"},
			Mounts: []docker.Mount{{Type: "volume", Source: vol, Target: "/src"},
				{Type: "volume", Source: cache, Target: "/home/user/.local/share/buildkit"}},
			LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "16m"}},
		},
	}, timeout, func(s string) {
		b.mu.Lock()
		b.extra = s
		b.mu.Unlock()
	}, "", nil)
	if kitID != "" {
		defer a.dk.ContainerRemove(context.Background(), kitID)
	}
	b.write(out)
	if err != nil {
		return "", "", err
	}
	if code != 0 {
		return "", "", errors.New("the build failed (see the log above)")
	}

	// 3. load and push
	b.step("pushing")
	rc, size, err := a.dk.CopyFileOut(ctx, kitID, "/src/out/image.tar")
	if err != nil {
		return "", "", fmt.Errorf("read built image: %w", err)
	}
	if job.MaxImageMB > 0 && size > int64(job.MaxImageMB)<<20 {
		rc.Close()
		return "", "", fmt.Errorf("the image is %d MB; the limit is %d MB", size>>20, job.MaxImageMB)
	}
	err = a.dk.ImageLoad(ctx, rc)
	rc.Close()
	if err != nil {
		return "", "", fmt.Errorf("load built image: %w", err)
	}
	defer a.dk.ImageRemove(context.Background(), image) // it is in the registry now
	host, _, _ := strings.Cut(job.ImageRepo, "/")
	if err := a.dk.ImagePush(ctx, image, docker.RegistryAuth("build", job.PushToken, host)); err != nil {
		return "", "", fmt.Errorf("push image: %w", err)
	}
	return commit, image, nil
}

// runOnce runs a container to completion and returns its output; the caller removes it.
func (a *Agent) runOnce(ctx context.Context, name string, cfg docker.ContainerConfig, timeout time.Duration, progress func(string), dir string, tarball []byte) (string, int, string, error) {
	_ = a.dk.ContainerRemove(ctx, name) // leftover from an interrupted build
	id, err := a.dk.ContainerCreate(ctx, name, cfg)
	if err != nil {
		return "", 0, "", fmt.Errorf("create %s: %w", name, err)
	}
	if tarball != nil {
		if err := a.dk.ContainerPutArchive(ctx, id, dir, tarball); err != nil {
			return id, 0, "", fmt.Errorf("copy files into %s: %w", name, err)
		}
	}
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		return id, 0, "", fmt.Errorf("start %s: %w", name, err)
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	done := make(chan struct{})
	if progress != nil {
		go func() {
			t := time.NewTicker(4 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if o, e, err := a.dk.ContainerOutput(context.Background(), id); err == nil {
						progress(string(o) + string(e))
					}
				}
			}
		}()
	}
	code, werr := a.dk.ContainerWait(wctx, id)
	close(done)
	cancel()
	if werr != nil {
		_ = a.dk.ContainerStop(context.Background(), id, 1)
	}
	stdout, stderr, _ := a.dk.ContainerOutput(context.Background(), id)
	out := string(stdout) + string(stderr)
	if werr != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return id, code, out, errors.New("the build was stopped")
		}
		if wctx.Err() != nil {
			return id, code, out, fmt.Errorf("time limit of %s reached", timeout.Round(time.Minute))
		}
		return id, code, out, werr
	}
	return id, code, out, nil
}

// fetch downloads a file from the API (the archive of a zip build).
func (a *Agent) fetch(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.st.Server+path, nil)
	if err != nil {
		return nil, err
	}
	if a.st.HostHeader != "" {
		req.Host = a.st.HostHeader
	}
	req.Header.Set("Authorization", "Bearer "+a.st.Token)
	hc := &http.Client{Timeout: 10 * time.Minute, Transport: a.hc.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<30))
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
