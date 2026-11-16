package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"platform/internal/docker"
	"platform/internal/model"
	"platform/internal/netguard"
	"platform/internal/registry"
)

// Builds from git, one at a time:
//
//  1. a git container clones the branch into a fresh volume (no capabilities except
//     CHOWN, read-only root, network "pt-build");
//  2. a rootless BuildKit container builds the Dockerfile into an image tarball
//     (non-root, only SETUID/SETGID for its user namespace, no Docker socket, same
//     network, CPU/memory/time limits);
//  3. the agent loads the tarball through the Docker API and pushes it to the
//     organization's registry namespace (credential "builder/<org>", push only there);
//     the registry's push event queues the usual vulnerability scan;
//  4. optionally the service is redeployed with the new image.
//
// "pt-build" matches the host firewall's pt-* rules (no private ranges, no metadata
// endpoint, no outbound mail) and has inter-container traffic off, so builds cannot
// reach the platform or each other. A per-service cache volume speeds up rebuilds.

const buildNetwork = "pt-build"

type buildJob struct {
	ID, OrgID, OrgSlug, ServiceID, ServiceSlug string
	Number                                     int
	Repo, Branch, Dir                          string
	TokenEnc                                   []byte
	Deploy                                     bool
	CreatedBy                                  *string
}

func (a *Agent) pokeBuilds() {
	select {
	case a.buildWake <- struct{}{}:
	default:
	}
}

func (a *Agent) buildLoop(ctx context.Context) {
	if !a.cfg.BuildsEnabled || a.cfg.RegistryHost == "" || !a.isPrimary() {
		return
	}
	if _, err := a.pool.Exec(ctx, `
		UPDATE builds SET status = 'failed', error = 'interrupted by a platform restart; start the build again', finished_at = now()
		WHERE status = 'running'`); err != nil {
		a.log.Warn("reset interrupted builds", "err", err)
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		for ctx.Err() == nil {
			b, ok := a.claimBuild(ctx)
			if !ok {
				break
			}
			a.runBuild(ctx, b)
		}
		select {
		case <-ctx.Done():
			return
		case <-a.buildWake:
		case <-tick.C:
		}
	}
}

func (a *Agent) claimBuild(ctx context.Context) (buildJob, bool) {
	var b buildJob
	err := a.pool.QueryRow(ctx, `
		WITH next AS (
			SELECT id FROM builds WHERE status = 'queued' ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE builds bu SET status = 'running', started_at = now()
		FROM next, services s, orgs o
		WHERE bu.id = next.id AND s.id = bu.service_id AND o.id = bu.org_id
		RETURNING bu.id, bu.org_id, o.slug, s.id, s.slug, bu.number, s.source_repo, s.source_branch, s.source_dir,
		          s.source_token_enc, bu.deploy, bu.created_by`).
		Scan(&b.ID, &b.OrgID, &b.OrgSlug, &b.ServiceID, &b.ServiceSlug, &b.Number, &b.Repo, &b.Branch, &b.Dir,
			&b.TokenEnc, &b.Deploy, &b.CreatedBy)
	if err != nil {
		return b, false
	}
	return b, true
}

type buildLog struct {
	strings.Builder
	save func(string) // writes the log so far to the build row (live view)
}

func (l *buildLog) step(format string, args ...any) {
	fmt.Fprintf(l, "==> "+format+"\n", args...)
	if l.save != nil {
		l.save(l.String())
	}
}

// text keeps the last 200 KB, which is what the console shows.
func (l *buildLog) text() string { return trimLog(l.String()) }

func trimLog(s string) string {
	if len(s) > 200_000 {
		s = "… (earlier output trimmed)\n" + s[len(s)-200_000:]
	}
	return s
}

// saveBuildLog stores the log so far while the build runs (the console polls it).
func (a *Agent) saveBuildLog(buildID, s string) {
	_, _ = a.pool.Exec(context.Background(), `UPDATE builds SET log = $2 WHERE id = $1 AND status = 'running'`, buildID, trimLog(s))
}

func (a *Agent) runBuild(parent context.Context, b buildJob) {
	ctx, cancel := context.WithTimeout(parent, a.cfg.BuildTimeout+10*time.Minute)
	defer cancel()
	var log buildLog
	log.save = func(s string) { a.saveBuildLog(b.ID, s) }
	fail := func(msg string) {
		log.step("failed: %s", msg)
		_, _ = a.pool.Exec(context.Background(), `
			UPDATE builds SET status = 'failed', error = $2, log = $3, finished_at = now() WHERE id = $1`, b.ID, msg, log.text())
		_ = auditAgent(context.Background(), a, b.OrgID, "build.failed", "service:"+b.ServiceID, map[string]any{"build": b.Number, "error": msg})
		a.log.Info("build failed", "build", b.ID, "err", msg)
	}

	log.step("build #%d of %s (branch %s)", b.Number, b.Repo, b.Branch)
	if err := a.checkRepo(ctx, b.Repo); err != nil {
		fail(err.Error())
		return
	}
	token := ""
	if len(b.TokenEnc) > 0 {
		plain, err := a.box.Open(b.TokenEnc, "source:"+b.ServiceID)
		if err != nil {
			fail("cannot read the repository token")
			return
		}
		token = string(plain)
	}
	if err := a.ensureBuildNetwork(ctx); err != nil {
		fail("build network: " + err.Error())
		return
	}
	for _, img := range []string{a.cfg.GitImage, a.cfg.BuildkitImage} {
		if _, err := a.dk.ImageID(ctx, img); err != nil {
			if err := a.dk.ImagePull(ctx, img, ""); err != nil {
				fail("pull " + img + ": " + err.Error())
				return
			}
		}
	}
	short := model.Compact(b.ID)[:12]
	vol := "pbuild-" + short
	cache := "pbcache-" + model.Compact(b.ServiceID)[:12]
	labels := map[string]string{"platform.build": b.ID, "platform.org": b.OrgID}
	if err := a.dk.VolumeCreate(ctx, vol, labels); err != nil {
		fail("create build volume: " + err.Error())
		return
	}
	defer a.dk.VolumeRemove(context.Background(), vol)
	if err := a.dk.VolumeCreate(ctx, cache, map[string]string{"platform.build-cache": b.ServiceID, "platform.org": b.OrgID}); err != nil {
		fail("create cache volume: " + err.Error())
		return
	}

	// 1. clone
	log.step("cloning")
	cloneScript := `set -eu
cd /src
URL="$REPO"
if [ -n "${TOKEN:-}" ]; then URL="${REPO%%://*}://x-access-token:${TOKEN}@${REPO#*://}"; fi
clone() { git clone --quiet --single-branch --branch "$BRANCH" "$@" -- "$URL" repo; }
if ! clone --depth 1 2>/tmp/err; then
  rm -rf repo
  if ! clone 2>>/tmp/err; then
    if [ -n "${TOKEN:-}" ]; then sed "s|${TOKEN}|***|g" /tmp/err >&2; else cat /tmp/err >&2; fi
    exit 1
  fi
fi
echo "COMMIT=$(git -C repo rev-parse HEAD)"
rm -rf repo/.git
D="repo/${DIR:-.}"
if [ ! -d "$D" ]; then echo "Folder ${DIR} not found in the repository." >&2; exit 2; fi
mkdir -p out plan && chmod 777 out
if [ -f "$D/Dockerfile" ]; then
  echo "MODE=dockerfile"
else
  # No Dockerfile: Railpack detects the language and writes a build plan.
  cp -a /opt/railpack-tmp/. /tmp/ 2>/dev/null || true
  if ! railpack prepare "$D" --plan-out plan/railpack-plan.json --info-out plan/info.json >/tmp/prepare.out 2>&1; then
    cat /tmp/prepare.out >&2
    exit 3
  fi
  tail -n 30 /tmp/prepare.out
  echo "MODE=railpack"
fi
chmod -R a+rX repo plan
chown 1000:1000 /cache`
	cloneID, code, out, err := a.runOnce(ctx, "build-clone-"+short, docker.ContainerConfig{
		Image:      a.cfg.GitImage,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{cloneScript},
		Env: []string{"REPO=" + b.Repo, "BRANCH=" + b.Branch, "DIR=" + b.Dir, "TOKEN=" + token,
			"HOME=/tmp", "GIT_TERMINAL_PROMPT=0"},
		Labels: labels,
		HostConfig: a.withDNS(docker.HostConfig{
			Runtime: a.cfg.BuildRuntime, NetworkMode: buildNetwork,
			Memory: 512 << 20, MemorySwap: 512 << 20, NanoCPUs: 1_000_000_000, PidsLimit: 256,
			CapDrop: []string{"ALL"}, CapAdd: []string{"CHOWN"}, SecurityOpt: []string{"no-new-privileges:true"},
			ReadonlyRootfs: true, Tmpfs: map[string]string{"/tmp": "rw,exec,size=1g"}, // Railpack runs its helper from /tmp
			Mounts:    []docker.Mount{{Type: "volume", Source: vol, Target: "/src"}, {Type: "volume", Source: cache, Target: "/cache"}},
			LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "4m"}},
		}),
	}, 5*time.Minute, nil)
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
			log.WriteString(line + "\n")
		}
	}
	if err != nil || code != 0 || len(commit) < 12 {
		msg := "could not clone the repository (check the address, branch and token)"
		switch code {
		case 2:
			msg = "folder " + b.Dir + " not found in the repository"
		case 3:
			msg = "no Dockerfile, and Railpack could not detect how to build this project (see the log above); add a Dockerfile"
		}
		if err != nil {
			msg += ": " + err.Error()
		}
		fail(msg)
		return
	}
	log.step("commit %s", commit)

	// 2. build
	image := fmt.Sprintf("%s/%s/%s:git-%s", a.cfg.RegistryHost, b.OrgSlug, b.ServiceSlug, commit[:12])
	ctxDir := "/src/repo"
	if b.Dir != "" {
		ctxDir += "/" + b.Dir
	}
	args := []string{"build", "--progress", "plain", "--local", "context=" + ctxDir,
		"--output", "type=docker,name=" + image + ",dest=/src/out/image.tar,compression=uncompressed"}
	if mode == "railpack" {
		log.step("building %s with Railpack (no Dockerfile)", image)
		args = append(args, "--frontend", "gateway.v0", "--opt", "source="+a.cfg.RailpackFrontend,
			"--local", "dockerfile=/src/plan", "--opt", "build-arg:cache-key="+model.Compact(b.ServiceID))
	} else {
		log.step("building %s from the Dockerfile", image)
		args = append(args, "--frontend", "dockerfile.v0", "--local", "dockerfile="+ctxDir)
	}
	buildID, code, out, err := a.runOnce(ctx, "build-kit-"+short, docker.ContainerConfig{
		Image:      a.cfg.BuildkitImage,
		Entrypoint: []string{"buildctl-daemonless.sh"},
		Cmd:        args,
		Env:        []string{"BUILDKITD_FLAGS=--oci-worker-no-process-sandbox"},
		Labels:     labels,
		HostConfig: a.withDNS(docker.HostConfig{
			Runtime: a.cfg.BuildRuntime, NetworkMode: buildNetwork,
			Memory: int64(a.cfg.BuildMemoryMB) << 20, MemorySwap: int64(a.cfg.BuildMemoryMB) << 20,
			NanoCPUs: int64(a.cfg.BuildCPUMillis) * 1_000_000, PidsLimit: 2048,
			// Rootless BuildKit maps its user namespace with newuidmap/newgidmap (setuid),
			// so no-new-privileges cannot be set; every other capability is dropped.
			CapDrop: []string{"ALL"}, CapAdd: []string{"SETUID", "SETGID"},
			Mounts: []docker.Mount{{Type: "volume", Source: vol, Target: "/src"},
				{Type: "volume", Source: cache, Target: "/home/user/.local/share/buildkit"}},
			LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "16m"}},
		}),
	}, a.cfg.BuildTimeout, func(out string) {
		a.saveBuildLog(b.ID, log.String()+out)
	})
	if buildID != "" {
		defer a.dk.ContainerRemove(context.Background(), buildID)
	}
	log.WriteString(out)
	if err != nil {
		fail(err.Error())
		return
	}
	if code != 0 {
		fail("the build failed (see the log above)")
		return
	}

	// 3. load and push
	log.step("pushing")
	rc, size, err := a.dk.CopyFileOut(ctx, buildID, "/src/out/image.tar")
	if err != nil {
		fail("read built image: " + err.Error())
		return
	}
	if size > int64(a.cfg.BuildMaxImageMB)<<20 {
		rc.Close()
		fail(fmt.Sprintf("the image is %d MB; the limit is %d MB", size>>20, a.cfg.BuildMaxImageMB))
		return
	}
	err = a.dk.ImageLoad(ctx, rc)
	rc.Close()
	if err != nil {
		fail("load built image: " + err.Error())
		return
	}
	auth := docker.RegistryAuth(registry.BuilderUserPrefix+b.OrgSlug,
		registry.BuilderPassword(a.cfg.RegistryInternalSecret, b.OrgSlug), a.cfg.RegistryHost)
	if err := a.dk.ImagePush(ctx, image, auth); err != nil {
		fail("push image: " + err.Error())
		return
	}
	log.step("pushed %s (%d MB)", image, size>>20)

	// 4. record and deploy
	if b.Deploy {
		if err := a.deployBuilt(ctx, b, image); err != nil {
			fail("deploy: " + err.Error())
			return
		}
		log.step("deployment started")
	}
	if _, err := a.pool.Exec(ctx, `
		UPDATE builds SET status = 'done', commit = $2, image = $3, log = $4, finished_at = now() WHERE id = $1`,
		b.ID, commit, image, log.text()); err != nil {
		a.log.Error("record build", "err", err)
	}
	_ = auditAgent(ctx, a, b.OrgID, "build.done", "service:"+b.ServiceID, map[string]any{"build": b.Number, "image": image})
	a.log.Info("build done", "build", b.ID, "image", image)
}

// deployBuilt points the service at the new image and queues a deployment with its
// current settings, as the user who started the build.
func (a *Agent) deployBuilt(ctx context.Context, b buildJob, image string) error {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var spec model.Spec
	var envJSON []byte
	if err := tx.QueryRow(ctx, `
		UPDATE services SET image = $2 WHERE id = $1
		RETURNING port, cpu_millis, memory_mb, health_path, volume_path, env`, b.ServiceID, image).
		Scan(&spec.Port, &spec.CPUMillis, &spec.MemoryMB, &spec.HealthPath, &spec.VolumePath, &envJSON); err != nil {
		return err
	}
	_ = json.Unmarshal(envJSON, &spec.Env)
	spec.Image = image
	if err := spec.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(spec)
	if _, err := tx.Exec(ctx, `
		INSERT INTO deployments (org_id, service_id, revision, spec, created_by)
		VALUES ($1, $2, (SELECT coalesce(max(revision), 0) + 1 FROM deployments WHERE service_id = $2), $3, $4)`,
		b.OrgID, b.ServiceID, raw, b.CreatedBy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	a.poke()
	return nil
}

// runOnce runs a container to completion and returns its combined output. The caller
// removes the container (its files may still be needed).
func (a *Agent) runOnce(ctx context.Context, name string, cfg docker.ContainerConfig, timeout time.Duration, progress func(string)) (string, int, string, error) {
	id, err := a.dk.ContainerCreate(ctx, name, cfg)
	if err != nil {
		return "", 0, "", fmt.Errorf("create %s: %w", name, err)
	}
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		return id, 0, "", fmt.Errorf("start %s: %w", name, err)
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	done := make(chan struct{})
	if progress != nil { // the console shows the log while the step runs
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
		if errors.Is(werr, context.DeadlineExceeded) || wctx.Err() != nil {
			return id, code, out, fmt.Errorf("time limit of %s reached", timeout.Round(time.Minute))
		}
		return id, code, out, werr
	}
	return id, code, out, nil
}

func (a *Agent) ensureBuildNetwork(ctx context.Context) error {
	if _, err := a.dk.NetworkInspect(ctx, buildNetwork); err == nil {
		return nil
	}
	err := a.dk.NetworkCreateIsolated(ctx, buildNetwork, a.cfg.BuildSubnet, map[string]string{"platform.build-network": "true"})
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// checkRepo refuses repositories on private or internal addresses (the API checks the
// URL's form; this resolves the name at build time).
func (a *Agent) checkRepo(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("invalid repository address")
	}
	if a.cfg.BuildAllowInsecure {
		return nil
	}
	if u.Scheme != "https" {
		return errors.New("the repository address must use https")
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(addrs) == 0 {
		return errors.New("cannot resolve " + u.Hostname())
	}
	for _, ad := range addrs {
		if netguard.Blocked(netip.Addr(ad).Unmap()) {
			return errors.New("repositories on private or internal addresses are not allowed")
		}
	}
	return nil
}
