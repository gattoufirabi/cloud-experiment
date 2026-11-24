package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"platform/internal/docker"
)

// Runner runs a pipeline's command steps in containers. The builder that owns the
// build sets the workspace and the container limits; the image build itself stays with
// the builder (it calls Runner only for the other steps).
type Runner struct {
	DK      *docker.Client
	Prefix  string            // container names: <Prefix>-<step number>
	Volume  string            // the build's workspace volume, mounted at /src
	Workdir string            // where steps start: /src/repo or /src/repo/<folder>
	Labels  map[string]string // labels of every build container
	Host    docker.HostConfig // network, runtime, CPU and memory, DNS, log driver
	Log     func(string)      // appends to the build log
	Live    func(string)      // output of the running step, shown while it runs
}

// Run runs one command step in image (for a step with image "built", the caller
// passes the image just built, already loaded). secrets are the values of the step's
// secrets; they are hidden from the log.
func (r *Runner) Run(ctx context.Context, n int, s Step, image string, secrets map[string]string) error {
	if s.Image == BuiltImage {
		// the image just built is already here
	} else if _, err := r.DK.ImageID(ctx, image); err != nil {
		r.Log(fmt.Sprintf("pulling %s\n", image))
		if err := r.DK.ImagePull(ctx, image, ""); err != nil {
			return fmt.Errorf("step %q: could not pull %s: %v", s.Name, image, err)
		}
	}
	env := []string{"CI=true", "STAYSA_CI=true", "STAYSA_STEP=" + s.Name, "HOME=/tmp"}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+s.Env[k])
	}
	for _, name := range s.Secrets {
		v, ok := secrets[name]
		if !ok {
			return fmt.Errorf("step %q: the service has no secret %s (add it in the service's Variables)", s.Name, name)
		}
		env = append(env, name+"="+v)
	}
	host := r.Host
	host.Mounts = []docker.Mount{{Type: "volume", Source: r.Volume, Target: "/src"}}
	host.CapDrop = []string{"ALL"}
	host.SecurityOpt = append([]string{"no-new-privileges:true"}, host.SecurityOpt...)
	host.Tmpfs = map[string]string{"/tmp": "rw,exec,size=1g"}
	if host.PidsLimit == 0 {
		host.PidsLimit = 1024
	}
	name := fmt.Sprintf("%s-%d", r.Prefix, n)
	_ = r.DK.ContainerRemove(ctx, name) // leftover from an interrupted build
	id, err := r.DK.ContainerCreate(ctx, name, docker.ContainerConfig{
		Image: image, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{s.Run},
		Env: env, Labels: r.Labels, WorkingDir: r.Workdir, HostConfig: host,
	})
	if err != nil {
		return fmt.Errorf("step %q: %v", s.Name, err)
	}
	defer r.DK.ContainerRemove(context.Background(), id)
	hide := hider(secrets, s.Secrets)
	if err := r.DK.ContainerStart(ctx, id); err != nil {
		if strings.Contains(err.Error(), "/bin/sh") {
			return fmt.Errorf("step %q: image %s has no /bin/sh; use an image with a shell", s.Name, image)
		}
		return fmt.Errorf("step %q: %v", s.Name, err)
	}
	wctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if o, e, err := r.DK.ContainerOutput(context.Background(), id); err == nil && r.Live != nil {
					r.Live(hide(string(o) + string(e)))
				}
			}
		}
	}()
	code, werr := r.DK.ContainerWait(wctx, id)
	close(done)
	if werr != nil {
		_ = r.DK.ContainerStop(context.Background(), id, 1)
	}
	o, e, _ := r.DK.ContainerOutput(context.Background(), id)
	r.Log(hide(string(o) + string(e)))
	switch {
	case werr != nil && ctx.Err() != nil:
		return errors.New("the build was stopped")
	case werr != nil && wctx.Err() != nil:
		return fmt.Errorf("step %q took longer than its timeout of %s", s.Name, s.Timeout)
	case werr != nil:
		return fmt.Errorf("step %q: %v", s.Name, werr)
	case code == 127 && strings.Contains(string(e), "/bin/sh"):
		return fmt.Errorf("step %q: image %s has no /bin/sh; use an image with a shell", s.Name, image)
	case code != 0:
		return fmt.Errorf("step %q failed with exit code %d (see the log above)", s.Name, code)
	}
	return nil
}

// hider replaces the step's secret values in its output.
func hider(secrets map[string]string, names []string) func(string) string {
	var vals []string
	for _, n := range names {
		if v := secrets[n]; len(v) >= 4 {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	return func(s string) string {
		for _, v := range vals {
			s = strings.ReplaceAll(s, v, "***")
		}
		return s
	}
}

// Describe is the log line that opens a step.
func Describe(n, total int, s Step) string {
	if s.Build {
		return fmt.Sprintf("step %d/%d: build the image", n, total)
	}
	return fmt.Sprintf("step %d/%d: %s (%s)", n, total, s.Name, s.Image)
}
