package agent

import (
	"context"
	"strings"
	"time"
)

// Restarts of platform services asked for in the admin panel (platform_restarts): the
// primary's agent restarts the compose service's container and records the outcome.

func (a *Agent) runRestarts(ctx context.Context) {
	if !a.isPrimary() {
		return
	}
	rows, err := a.pool.Query(ctx, `
		UPDATE platform_restarts SET done_at = now() WHERE done_at IS NULL AND requested_at > now() - interval '10 minutes'
		RETURNING id, service`)
	if err != nil {
		return
	}
	type req struct {
		ID      int64
		Service string
	}
	var list []req
	for rows.Next() {
		var q req
		if rows.Scan(&q.ID, &q.Service) == nil {
			list = append(list, q)
		}
	}
	rows.Close()
	for _, q := range list {
		msg := a.restartService(ctx, q.Service)
		if q.Service == "caddy" && msg == "" { // it starts from its initial config: push the routes again
			time.Sleep(3 * time.Second)
			a.syncCaddy(ctx, true)
		}
		_, _ = a.pool.Exec(ctx, `UPDATE platform_restarts SET error = $2, done_at = now() WHERE id = $1`, q.ID, msg)
		if msg == "" {
			a.log.Info("platform service restarted from the admin panel", "service", q.Service)
		} else {
			a.log.Warn("platform service restart", "service", q.Service, "err", msg)
		}
	}
}

// restartService restarts the containers of a compose service; it returns "" or the error.
func (a *Agent) restartService(ctx context.Context, service string) string {
	cs, err := a.dk.ContainerList(ctx, "com.docker.compose.service="+service)
	if err != nil {
		return err.Error()
	}
	if len(cs) == 0 {
		return "no container for " + service
	}
	var errs []string
	for _, c := range cs {
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		if err := a.dk.ContainerStop(cctx, c.ID, 20); err != nil {
			errs = append(errs, err.Error())
		} else if err := a.dk.ContainerStart(cctx, c.ID); err != nil {
			errs = append(errs, err.Error())
		}
		cancel()
	}
	return strings.Join(errs, "; ")
}
