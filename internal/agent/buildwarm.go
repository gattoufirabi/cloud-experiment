package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"platform/internal/docker"
)

// Warm build cache. Each service has its own BuildKit cache (never shared between
// tenants), so the first Railpack build of a new service used to download the Railpack
// builder and runtime images (hundreds of MB) from the internet. The agent builds a tiny
// public sample project once per node into a "warm" cache volume; a new service's empty
// cache starts as a copy of it. The warm cache only ever holds public base images pulled
// by the platform itself — no tenant code or data reaches it.

const warmMarker = ".platform-warm-ok"

// warmVolume names the warm cache for the current BuildKit and Railpack versions, so an
// upgrade warms a new one (and the old one is removed).
func (a *Agent) warmVolume() string {
	h := sha256.Sum256([]byte(a.cfg.BuildkitImage + "|" + a.cfg.RailpackFrontend + "|" + a.cfg.GitImage))
	return "pbwarm-" + hex.EncodeToString(h[:])[:12]
}

// buildWarmLoop warms the cache shortly after start, then checks once a day (a failed
// attempt, e.g. no internet, is retried).
func (a *Agent) buildWarmLoop(ctx context.Context) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next := 24 * time.Hour
		if err := a.warmBuildCache(ctx); err != nil {
			slog.Warn("warming the build cache failed", "component", "agent", "err", err)
			next = time.Hour
		}
		timer.Reset(next)
	}
}

// warmReady reports whether the warm cache volume holds a complete warm build.
func (a *Agent) warmReady(ctx context.Context, warm string) bool {
	if ok, err := a.dk.VolumeExists(ctx, warm); err != nil || !ok {
		return false
	}
	id, code, _, err := a.runOnce(ctx, "build-warm-check-"+warm[7:], docker.ContainerConfig{
		Image: a.cfg.GitImage, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"test -f /warm/" + warmMarker},
		Labels: map[string]string{"platform.build-warm": "check"},
		HostConfig: docker.HostConfig{NetworkMode: "none", Memory: 64 << 20, PidsLimit: 32,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, ReadonlyRootfs: true,
			Mounts: []docker.Mount{{Type: "volume", Source: warm, Target: "/warm", ReadOnly: true}}},
	}, time.Minute, nil)
	if id != "" {
		a.dk.ContainerRemove(context.Background(), id)
	}
	return err == nil && code == 0
}

func (a *Agent) warmBuildCache(ctx context.Context) error {
	warm := a.warmVolume()
	if sizes, err := a.dk.VolumeSizes(ctx); err == nil { // remove warm caches of older versions
		for name := range sizes {
			if strings.HasPrefix(name, "pbwarm-") && name != warm {
				a.dk.VolumeRemove(ctx, name)
			}
		}
	}
	if a.warmReady(ctx, warm) {
		return nil
	}
	if err := a.ensureBuildNetwork(ctx); err != nil {
		return err
	}
	for _, img := range []string{a.cfg.GitImage, a.cfg.BuildkitImage} {
		if _, err := a.dk.ImageID(ctx, img); err != nil {
			if err := a.dk.ImagePull(ctx, img, ""); err != nil {
				return err
			}
		}
	}
	// containers left by an attempt the agent did not finish (it was restarted)
	for _, n := range []string{"build-warm-prepare-", "build-warm-kit-"} {
		a.dk.ContainerRemove(ctx, n+warm[7:])
	}
	slog.Info("warming the build cache", "component", "agent", "volume", warm)
	start := time.Now()
	labels := map[string]string{"platform.build-warm": warm}
	vol := warm + "-src"
	a.dk.VolumeRemove(ctx, vol)
	if err := a.dk.VolumeCreate(ctx, vol, labels); err != nil {
		return err
	}
	defer a.dk.VolumeRemove(context.Background(), vol)
	if err := a.dk.VolumeCreate(ctx, warm, labels); err != nil {
		return err
	}
	// 1. a sample Node.js project and its Railpack plan (the most common case)
	script := `set -eu
cd /src && mkdir -p repo plan out && chmod 777 out
printf '%s\n' '{"name":"warm","version":"1.0.0","private":true,"scripts":{"start":"node server.js"}}' > repo/package.json
printf '%s\n' "require('http').createServer((q, s) => s.end('ok')).listen(process.env.PORT || 3000);" > repo/server.js
cp -a /opt/railpack-tmp/. /tmp/ 2>/dev/null || true
railpack prepare repo --plan-out plan/railpack-plan.json >/tmp/prepare.out 2>&1 || { cat /tmp/prepare.out >&2; exit 3; }
chmod -R a+rX repo plan
chown 1000:1000 /cache`
	id, code, out, err := a.runOnce(ctx, "build-warm-prepare-"+warm[7:], docker.ContainerConfig{
		Image: a.cfg.GitImage, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{script},
		Env: []string{"HOME=/tmp"}, Labels: labels,
		HostConfig: a.withDNS(docker.HostConfig{
			Runtime: a.cfg.BuildRuntime, NetworkMode: buildNetwork,
			Memory: 512 << 20, MemorySwap: 512 << 20, NanoCPUs: 1_000_000_000, PidsLimit: 256,
			CapDrop: []string{"ALL"}, CapAdd: []string{"CHOWN"}, SecurityOpt: []string{"no-new-privileges:true"},
			ReadonlyRootfs: true, Tmpfs: map[string]string{"/tmp": "rw,exec,size=1g"},
			Mounts: []docker.Mount{{Type: "volume", Source: vol, Target: "/src"}, {Type: "volume", Source: warm, Target: "/cache"}},
		}),
	}, 5*time.Minute, nil)
	if id != "" {
		a.dk.ContainerRemove(context.Background(), id)
	}
	if err != nil || code != 0 {
		return warmErr("prepare", err, code, out)
	}
	// 2. build it with the same BuildKit settings as tenant builds, into the warm cache
	args := []string{"build", "--progress", "plain", "--local", "context=/src/repo",
		"--output", "type=docker,name=warm,dest=/src/out/image.tar,compression=uncompressed",
		"--frontend", "gateway.v0", "--opt", "source=" + a.cfg.RailpackFrontend,
		"--local", "dockerfile=/src/plan", "--opt", "build-arg:cache-key=warm"}
	id, code, out, err = a.runOnce(ctx, "build-warm-kit-"+warm[7:], docker.ContainerConfig{
		Image:      a.cfg.BuildkitImage,
		Entrypoint: []string{"/bin/sh", "-c", `buildctl-daemonless.sh "$@" && touch /home/user/.local/share/buildkit/` + warmMarker, "sh"},
		Cmd:        args,
		Env:        []string{"BUILDKITD_FLAGS=--oci-worker-no-process-sandbox"},
		Labels:     labels,
		HostConfig: a.withDNS(docker.HostConfig{
			Runtime: a.cfg.BuildRuntime, NetworkMode: buildNetwork,
			Memory: int64(a.cfg.BuildMemoryMB) << 20, MemorySwap: int64(a.cfg.BuildMemoryMB) << 20,
			NanoCPUs: int64(a.cfg.BuildCPUMillis) * 1_000_000, PidsLimit: 2048,
			CapDrop: []string{"ALL"}, CapAdd: []string{"SETUID", "SETGID"},
			Mounts: []docker.Mount{{Type: "volume", Source: vol, Target: "/src"},
				{Type: "volume", Source: warm, Target: "/home/user/.local/share/buildkit"}},
			LogConfig: docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "4m"}},
		}),
	}, 45*time.Minute, nil)
	if id != "" {
		a.dk.ContainerRemove(context.Background(), id)
	}
	if err != nil || code != 0 {
		a.dk.VolumeRemove(context.Background(), warm) // a half-filled cache is not used; start over next time
		return warmErr("build", err, code, out)
	}
	slog.Info("build cache warmed", "component", "agent", "volume", warm, "took", time.Since(start).Round(time.Second).String())
	return nil
}

func warmErr(step string, err error, code int, out string) error {
	if err != nil {
		return fmt.Errorf("warm %s: %w", step, err)
	}
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return fmt.Errorf("warm %s exited with code %d: %s", step, code, out)
}

// seedMarker in a service's cache records that it was started from the warm cache.
const seedMarker = ".platform-seeded"

// seedBuildCache starts a service's build cache from the warm cache before its first
// Railpack build (a copy: the two never share files afterwards). BuildKit cannot merge two
// caches, so whatever the service had (earlier Dockerfile builds) is replaced once; later
// builds keep their cache. It does nothing when no warm cache is ready; a failure only
// means the build downloads the images itself.
func (a *Agent) seedBuildCache(ctx context.Context, cache, short string) bool {
	warm := a.warmVolume()
	if ok, err := a.dk.VolumeExists(ctx, warm); err != nil || !ok {
		return false
	}
	script := `set -eu
test -f /warm/` + warmMarker + `
if [ -f /cache/` + seedMarker + ` ]; then exit 0; fi
find /cache -mindepth 1 -maxdepth 1 -exec rm -rf {} +
cp -a /warm/. /cache/
rm -f /cache/` + warmMarker + `
touch /cache/` + seedMarker + `
echo SEEDED`
	id, code, out, err := a.runOnce(ctx, "build-seed-"+short, docker.ContainerConfig{
		Image: a.cfg.GitImage, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{script},
		Labels: map[string]string{"platform.build-warm": "seed"},
		HostConfig: docker.HostConfig{NetworkMode: "none", Memory: 256 << 20, PidsLimit: 64,
			// cp -a keeps BuildKit's file owners and modes (rootless BuildKit maps sub-uids)
			CapDrop: []string{"ALL"}, CapAdd: []string{"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "MKNOD"},
			SecurityOpt: []string{"no-new-privileges:true"}, ReadonlyRootfs: true,
			Mounts: []docker.Mount{{Type: "volume", Source: warm, Target: "/warm", ReadOnly: true},
				{Type: "volume", Source: cache, Target: "/cache"}}},
	}, 10*time.Minute, nil)
	if id != "" {
		a.dk.ContainerRemove(context.Background(), id)
	}
	return err == nil && code == 0 && strings.Contains(out, "SEEDED")
}
