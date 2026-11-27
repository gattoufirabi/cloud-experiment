package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/components/builds/buildsrc"
	"platform/internal/components/builds/pipeline"
	"platform/internal/core/worker"
	"platform/internal/lib/docker"
	"platform/internal/lib/model"
	"platform/internal/lib/netguard"
	"platform/internal/lib/registry"
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
	PreviewID                                  *string // pull request build: deploy to this preview
	PR                                         *int
	IsZip                                      bool   // zip deployment: the source is an uploaded archive
	SourceName, SourceSHA                      string // the archive's file name and SHA-256
}

func (a *Worker) PokeBuilds() {
	select {
	case a.BuildWake <- struct{}{}:
	default:
	}
}

func (a *Worker) BuildLoop(ctx context.Context) {
	if !a.Cfg.BuildsEnabled || a.Cfg.RegistryHost == "" || !a.IsPrimary() {
		return
	}
	if _, err := a.Pool.Exec(ctx, `
		UPDATE builds SET status = 'failed', error = 'interrupted by a platform restart; start the build again', finished_at = now()
		WHERE status = 'running' AND runner = 'platform'`); err != nil {
		a.Log.Warn("reset interrupted builds", "err", err)
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		a.sweepServerBuilds(ctx)
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
		case <-a.BuildWake:
		case <-tick.C:
		}
	}
}

// sweepServerBuilds ends builds meant for an organization's own servers (migration 069)
// that a server stopped reporting on, or that no server took in time.
func (a *Worker) sweepServerBuilds(ctx context.Context) {
	if _, err := a.Pool.Exec(ctx, `
		UPDATE builds SET status = 'failed', finished_at = now(), push_token_hash = NULL, source_zip = NULL,
		       error = CASE WHEN status = 'running' THEN 'the build server stopped responding; start the build again'
		                    ELSE 'no build server took this build within an hour: check that one of your servers is online, runs builds and has the labels the service asks for' END
		WHERE runner = 'servers' AND ((status = 'running' AND heartbeat_at < now() - interval '3 minutes')
		                           OR (status = 'queued' AND created_at < now() - interval '1 hour'))`); err != nil {
		a.Log.Warn("sweep server builds", "err", err)
	}
}

func (a *Worker) claimBuild(ctx context.Context) (buildJob, bool) {
	var b buildJob
	err := a.Pool.QueryRow(ctx, `
		WITH next AS (
			SELECT id FROM builds WHERE status = 'queued' AND runner = 'platform' ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE builds bu SET status = 'running', started_at = now()
		FROM next, services s, orgs o
		WHERE bu.id = next.id AND s.id = bu.service_id AND o.id = bu.org_id
		RETURNING bu.id, bu.org_id, o.slug, s.id, s.slug, bu.number, s.source_repo, coalesce(nullif(bu.branch, ''), s.source_branch), s.source_dir,
		          s.source_token_enc, bu.deploy, bu.created_by, bu.preview_service_id, bu.pr_number,
		          bu.source_zip IS NOT NULL, bu.source_name, bu.source_sha256`).
		Scan(&b.ID, &b.OrgID, &b.OrgSlug, &b.ServiceID, &b.ServiceSlug, &b.Number, &b.Repo, &b.Branch, &b.Dir,
			&b.TokenEnc, &b.Deploy, &b.CreatedBy, &b.PreviewID, &b.PR, &b.IsZip, &b.SourceName, &b.SourceSHA)
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
func (a *Worker) saveBuildLog(buildID, s string) {
	_, _ = a.Pool.Exec(context.Background(), `UPDATE builds SET log = $2 WHERE id = $1 AND status = 'running'`, buildID, trimLog(s))
}

func (a *Worker) runBuild(parent context.Context, b buildJob) {
	ctx, cancel := context.WithTimeout(parent, a.Cfg.BuildTimeout+10*time.Minute)
	defer cancel()
	var log buildLog
	log.save = func(s string) { a.saveBuildLog(b.ID, s) }
	fail := func(msg string) {
		log.step("failed: %s", msg)
		_, _ = a.Pool.Exec(context.Background(), `
			UPDATE builds SET status = 'failed', error = $2, log = $3, finished_at = now() WHERE id = $1`, b.ID, msg, log.text())
		_ = worker.AuditAgent(context.Background(), a.Agent, b.OrgID, "build.failed", "service:"+b.ServiceID, map[string]any{"build": b.Number, "error": msg})
		a.Log.Info("build failed", "build", b.ID, "err", msg)
	}

	if b.IsZip {
		// The archive is only needed for this build: drop it whatever happens.
		defer a.Pool.Exec(context.Background(), `UPDATE builds SET source_zip = NULL WHERE id = $1`, b.ID)
		b.Dir = ""
	}
	switch {
	case b.IsZip:
		log.step("build #%d of the uploaded archive %s (sha256 %s)", b.Number, b.SourceName, short12(b.SourceSHA))
	case b.PR != nil:
		log.step("build #%d of %s (pull request #%d, branch %s) for preview pr-%d", b.Number, b.Repo, *b.PR, b.Branch, *b.PR)
	default:
		log.step("build #%d of %s (branch %s)", b.Number, b.Repo, b.Branch)
	}
	if !b.IsZip {
		if err := a.checkRepo(ctx, b.Repo); err != nil {
			fail(err.Error())
			return
		}
	}
	token := ""
	if len(b.TokenEnc) > 0 && !b.IsZip {
		plain, err := a.Box.Open(b.TokenEnc, "source:"+b.ServiceID)
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
	for _, img := range []string{a.Cfg.GitImage, a.Cfg.BuildkitImage} {
		if _, err := a.Docker.ImageID(ctx, img); err != nil {
			if err := a.Docker.ImagePull(ctx, img, ""); err != nil {
				fail("pull " + img + ": " + err.Error())
				return
			}
		}
	}
	short := model.Compact(b.ID)[:12]
	vol := "pbuild-" + short
	cache := "pbcache-" + model.Compact(b.ServiceID)[:12]
	labels := map[string]string{"platform.build": b.ID, "platform.org": b.OrgID}
	if err := a.Docker.VolumeCreate(ctx, vol, labels); err != nil {
		fail("create build volume: " + err.Error())
		return
	}
	defer a.Docker.VolumeRemove(context.Background(), vol)
	if err := a.Docker.VolumeCreate(ctx, cache, map[string]string{"platform.build-cache": b.ServiceID, "platform.org": b.OrgID}); err != nil {
		fail("create cache volume: " + err.Error())
		return
	}

	// 1. clone (or unpack the uploaded archive)
	var tarball []byte
	if b.IsZip {
		var zipData []byte
		if err := a.Pool.QueryRow(ctx, `SELECT source_zip FROM builds WHERE id = $1`, b.ID).Scan(&zipData); err != nil || zipData == nil {
			fail("the uploaded archive is gone")
			return
		}
		var n int
		var err error
		if tarball, n, err = buildsrc.ZipToTar(zipData); err != nil {
			fail("archive: " + err.Error())
			return
		}
		log.step("unpacking %d files", n)
	} else {
		log.step("cloning")
	}
	script := buildsrc.Git + buildsrc.Common
	if b.IsZip {
		script = buildsrc.Zip + buildsrc.Common
	}
	cloneID, code, out, err := a.RunOnceFiles(ctx, "build-clone-"+short, docker.ContainerConfig{
		Image:      a.Cfg.GitImage,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{script},
		Env: []string{"REPO=" + b.Repo, "BRANCH=" + b.Branch, "DIR=" + b.Dir, "TOKEN=" + token, "SHA=" + b.SourceSHA,
			"HOME=/tmp", "GIT_TERMINAL_PROMPT=0"},
		Labels: labels,
		HostConfig: a.WithDNS(docker.HostConfig{
			Runtime: a.Cfg.BuildRuntime, NetworkMode: buildNetwork,
			Memory: 512 << 20, MemorySwap: 512 << 20, NanoCPUs: 1_000_000_000, PidsLimit: 256,
			CapDrop: []string{"ALL"}, CapAdd: []string{"CHOWN"}, SecurityOpt: []string{"no-new-privileges:true"},
			ReadonlyRootfs: true, Tmpfs: map[string]string{"/tmp": "rw,exec,size=1g"}, // Railpack runs its helper from /tmp
			Mounts:    []docker.Mount{{Type: "volume", Source: vol, Target: "/src"}, {Type: "volume", Source: cache, Target: "/cache"}},
			LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "4m"}},
		}),
	}, 5*time.Minute, nil, "/src", tarball)
	if cloneID != "" {
		defer a.Docker.ContainerRemove(context.Background(), cloneID)
	}
	commit, mode, pipe := "", "", ""
	for _, line := range strings.Split(out, "\n") {
		if c, ok := strings.CutPrefix(strings.TrimSpace(line), "COMMIT="); ok {
			commit = c
		} else if m, ok := strings.CutPrefix(strings.TrimSpace(line), "MODE="); ok {
			mode = m
		} else if pl, ok := strings.CutPrefix(strings.TrimSpace(line), "PIPELINE="); ok {
			pipe = pl
		} else if line != "" {
			log.WriteString(line + "\n")
		}
	}
	if err != nil || code != 0 || len(commit) < 12 {
		msg := buildsrc.FailureMessage(code, b.IsZip, b.Dir)
		if err != nil {
			msg += ": " + err.Error()
		}
		fail(msg)
		return
	}
	tag := "git-" + commit[:12]
	if b.IsZip {
		tag = "zip-" + commit[:12]
	} else {
		log.step("commit %s", commit)
	}

	// 2. the pipeline (staysa.yml), or the image build alone
	image := fmt.Sprintf("%s/%s/%s:%s", a.Cfg.RegistryHost, b.OrgSlug, b.ServiceSlug, tag)
	ctxDir := "/src/repo"
	if b.Dir != "" {
		ctxDir += "/" + b.Dir
	}
	pl := &pipeline.Pipeline{Steps: []pipeline.Step{{Name: "build", Build: true}}, Deploy: true}
	if pipe != "" {
		data, err := base64.StdEncoding.DecodeString(pipe)
		if err == nil {
			pl, err = pipeline.Parse(data)
		}
		if err != nil {
			fail(err.Error())
			return
		}
		log.step("pipeline %s: %d steps", pipeline.FileName, len(pl.Steps))
	}
	secrets, err := a.buildSecrets(ctx, b.ServiceID, pl.SecretNames())
	if err != nil {
		fail(err.Error())
		return
	}
	var size int64
	buildImage := func() (int64, error) {
		args := []string{"build", "--progress", "plain", "--local", "context=" + ctxDir,
			"--output", "type=docker,name=" + image + ",dest=/src/out/image.tar,compression=uncompressed"}
		if mode == "railpack" {
			if a.seedBuildCache(ctx, cache, short) {
				log.step("build cache started from the node's warm cache (Railpack base images)")
			}
			log.step("building %s with Railpack (no Dockerfile)", image)
			args = append(args, "--frontend", "gateway.v0", "--opt", "source="+a.Cfg.RailpackFrontend,
				"--local", "dockerfile=/src/plan", "--opt", "build-arg:cache-key="+model.Compact(b.ServiceID))
		} else {
			log.step("building %s from the Dockerfile", image)
			args = append(args, "--frontend", "dockerfile.v0", "--local", "dockerfile="+ctxDir)
		}
		runKit := func(flags string) (string, int, string, error) {
			return a.RunOnce(ctx, "build-kit-"+short, docker.ContainerConfig{
				Image:      a.Cfg.BuildkitImage,
				Entrypoint: []string{"buildctl-daemonless.sh"},
				Cmd:        args,
				Env:        []string{"BUILDKITD_FLAGS=" + flags},
				Labels:     labels,
				HostConfig: a.WithDNS(docker.HostConfig{
					Runtime: a.Cfg.BuildRuntime, NetworkMode: buildNetwork,
					Memory: int64(a.Cfg.BuildMemoryMB) << 20, MemorySwap: int64(a.Cfg.BuildMemoryMB) << 20,
					NanoCPUs: int64(a.Cfg.BuildCPUMillis) * 1_000_000, PidsLimit: 2048,
					// Rootless BuildKit maps its user namespace with newuidmap/newgidmap (setuid),
					// so no-new-privileges cannot be set; every other capability is dropped.
					CapDrop: []string{"ALL"}, CapAdd: []string{"SETUID", "SETGID"},
					Mounts: []docker.Mount{{Type: "volume", Source: vol, Target: "/src"},
						{Type: "volume", Source: cache, Target: "/home/user/.local/share/buildkit"}},
					LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "16m"}},
				}),
			}, a.Cfg.BuildTimeout, func(out string) {
				a.saveBuildLog(b.ID, log.String()+out)
			})
		}
		const kitFlags = "--oci-worker-no-process-sandbox"
		buildID, code, out, err := runKit(kitFlags)
		if err == nil && code != 0 && a.Cfg.BuildRuntime == "runsc" && needsNativeSnapshotter(out) {
			if buildID != "" {
				a.Docker.ContainerRemove(context.Background(), buildID)
			}
			log.WriteString(out)
			log.step("a step changed a file that its own processes had in use (for example apt-get upgrading libc6), " +
				"which the sandbox's overlay filesystem refuses: building again with BuildKit's native snapshotter (slower)")
			buildID, code, out, err = runKit(kitFlags + " --oci-worker-snapshotter=native")
		}
		if buildID != "" {
			defer a.Docker.ContainerRemove(context.Background(), buildID)
		}
		log.WriteString(out)
		if err != nil {
			return 0, err
		}
		if code != 0 {
			return 0, errors.New("the build failed (see the log above)")
		}

		// load the image here: later steps may run in it, then it is pushed
		rc, size, err := a.Docker.CopyFileOut(ctx, buildID, "/src/out/image.tar")
		if err != nil {
			return 0, fmt.Errorf("read built image: %w", err)
		}
		if size > int64(a.Cfg.BuildMaxImageMB)<<20 {
			rc.Close()
			return 0, fmt.Errorf("the image is %d MB; the limit is %d MB", size>>20, a.Cfg.BuildMaxImageMB)
		}
		err = a.Docker.ImageLoad(ctx, rc)
		rc.Close()
		if err != nil {
			return 0, fmt.Errorf("load built image: %w", err)
		}
		return size, nil
	}
	runner := &pipeline.Runner{DK: a.Docker, Prefix: "build-step-" + short, Volume: vol, Workdir: ctxDir, Labels: labels,
		Host: a.WithDNS(docker.HostConfig{
			Runtime: a.Cfg.BuildRuntime, NetworkMode: buildNetwork,
			Memory: int64(a.Cfg.BuildMemoryMB) << 20, MemorySwap: int64(a.Cfg.BuildMemoryMB) << 20,
			NanoCPUs:  int64(a.Cfg.BuildCPUMillis) * 1_000_000,
			LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "16m"}},
		}),
		Log:  func(s string) { log.WriteString(s) },
		Live: func(s string) { a.saveBuildLog(b.ID, log.String()+s) },
	}
	for i, st := range pl.Steps {
		if len(pl.Steps) > 1 {
			log.step("%s", pipeline.Describe(i+1, len(pl.Steps), st))
		}
		if st.Build {
			if size, err = buildImage(); err != nil {
				fail(err.Error())
				return
			}
			continue
		}
		img := st.Image
		if img == pipeline.BuiltImage {
			img = image
		}
		if err := runner.Run(ctx, i+1, st, img, secrets); err != nil {
			fail(err.Error())
			return
		}
	}

	// 3. push
	log.step("pushing")
	auth := docker.RegistryAuth(registry.BuilderUserPrefix+b.OrgSlug,
		registry.BuilderPassword(a.Cfg.RegistryInternalSecret, b.OrgSlug), a.Cfg.RegistryHost)
	if err := a.Docker.ImagePush(ctx, image, auth); err != nil {
		fail("push image: " + err.Error())
		return
	}
	log.step("pushed %s (%d MB)", image, size>>20)

	// 4. record and deploy
	switch {
	case b.PR != nil && b.PreviewID == nil:
		log.step("the preview was deleted meanwhile (pull request closed): nothing to deploy")
	case b.Deploy && !pl.Deploy:
		log.step("%s says deploy: false: the image is pushed, not deployed", pipeline.FileName)
	case b.Deploy:
		if err := a.deployBuilt(ctx, b, image); err != nil {
			fail("deploy: " + err.Error())
			return
		}
		if b.PR != nil {
			log.step("preview pr-%d deployment started", *b.PR)
		} else {
			log.step("deployment started")
		}
	}
	if _, err := a.Pool.Exec(ctx, `
		UPDATE builds SET status = 'done', commit = $2, image = $3, log = $4, finished_at = now() WHERE id = $1`,
		b.ID, commit, image, log.text()); err != nil {
		a.Log.Error("record build", "err", err)
	}
	_ = worker.AuditAgent(ctx, a.Agent, b.OrgID, "build.done", "service:"+b.ServiceID, map[string]any{"build": b.Number, "image": image})
	a.Log.Info("build done", "build", b.ID, "image", image)
}

// deployBuilt points the service (or, for a pull request, its preview) at the new image
// and queues a deployment with its current settings, as the user who started the build.
func (a *Worker) deployBuilt(ctx context.Context, b buildJob, image string) error {
	target := b.ServiceID
	if b.PreviewID != nil {
		target = *b.PreviewID
	}
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var spec model.Spec
	var envJSON []byte
	if err := tx.QueryRow(ctx, `
		UPDATE services SET image = $2 WHERE id = $1
		RETURNING port, cpu_millis, memory_mb, health_path, volume_path, env, worker, instances, ports`, target, image).
		Scan(&spec.Port, &spec.CPUMillis, &spec.MemoryMB, &spec.HealthPath, &spec.VolumePath, &envJSON, &spec.Worker, &spec.Instances, &spec.Ports); err != nil {
		if errors.Is(err, pgx.ErrNoRows) && b.PreviewID != nil {
			return errors.New("the preview was deleted while building (pull request closed)")
		}
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
		b.OrgID, target, raw, b.CreatedBy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	a.Poke()
	return nil
}

func (a *Worker) ensureBuildNetwork(ctx context.Context) error {
	if _, err := a.Docker.NetworkInspect(ctx, buildNetwork); err == nil {
		return nil
	}
	err := a.Docker.NetworkCreateIsolated(ctx, buildNetwork, a.Cfg.BuildSubnet, map[string]string{"platform.build-network": "true"})
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// checkRepo refuses repositories on private or internal addresses (the API checks the
// URL's form; this resolves the name at build time).
func (a *Worker) checkRepo(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("invalid repository address")
	}
	if a.Cfg.BuildAllowInsecure {
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

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// buildSecrets decrypts the service's secrets a pipeline asks for (by name).
func (a *Worker) buildSecrets(ctx context.Context, serviceID string, names []string) (map[string]string, error) {
	out := map[string]string{}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := a.Pool.Query(ctx, `SELECT name, value_enc FROM secrets WHERE service_id = $1 AND name = ANY($2)`, serviceID, names)
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
		plain, err := a.Box.Open(enc, "secret:"+serviceID+":"+name)
		if err != nil {
			return nil, fmt.Errorf("cannot read the secret %s", name)
		}
		out[name] = string(plain)
	}
	return out, rows.Err()
}

// needsNativeSnapshotter reports whether a failed BuildKit run hit the sandbox's overlay
// limit. Under gVisor, BuildKit's overlayfs snapshotter is gVisor's own overlay, which
// cannot copy a lower-layer file up while a process has it mapped, and answers ENODEV: dpkg
// upgrading libc6 cannot hard-link the dynamic loader every running process uses. The
// native snapshotter (each layer a plain directory, no copy-up) does not have the limit.
func needsNativeSnapshotter(out string) bool {
	return strings.Contains(out, "No such device")
}
