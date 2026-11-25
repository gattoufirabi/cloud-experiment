package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"platform/internal/lib/docker"
)

// runOnce runs a container to completion and returns its combined output. The caller
// removes the container (its files may still be needed).
func (a *Agent) RunOnce(ctx context.Context, name string, cfg docker.ContainerConfig, timeout time.Duration, progress func(string)) (string, int, string, error) {
	return a.RunOnceFiles(ctx, name, cfg, timeout, progress, "", nil)
}

// runOnceFiles is runOnce with a tar archive copied into the container at dir before it
// starts (into its volume when dir is a mount point).
func (a *Agent) RunOnceFiles(ctx context.Context, name string, cfg docker.ContainerConfig, timeout time.Duration, progress func(string), dir string, tarball []byte) (string, int, string, error) {
	id, err := a.Docker.ContainerCreate(ctx, name, cfg)
	if err != nil {
		return "", 0, "", fmt.Errorf("create %s: %w", name, err)
	}
	if tarball != nil {
		if err := a.Docker.ContainerPutArchive(ctx, id, dir, tarball); err != nil {
			return id, 0, "", fmt.Errorf("copy files into %s: %w", name, err)
		}
	}
	if err := a.Docker.ContainerStart(ctx, id); err != nil {
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
					if o, e, err := a.Docker.ContainerOutput(context.Background(), id); err == nil {
						progress(string(o) + string(e))
					}
				}
			}
		}()
	}
	code, werr := a.Docker.ContainerWait(wctx, id)
	close(done)
	cancel()
	if werr != nil {
		_ = a.Docker.ContainerStop(context.Background(), id, 1)
	}
	stdout, stderr, _ := a.Docker.ContainerOutput(context.Background(), id)
	out := string(stdout) + string(stderr)
	if werr != nil {
		if errors.Is(werr, context.DeadlineExceeded) || wctx.Err() != nil {
			return id, code, out, fmt.Errorf("time limit of %s reached", timeout.Round(time.Minute))
		}
		return id, code, out, werr
	}
	return id, code, out, nil
}
