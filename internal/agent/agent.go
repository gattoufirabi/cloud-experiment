// Package agent starts the node agent: the only part of the platform that touches Docker
// and Caddy. It reads desired state from PostgreSQL and converges the host to it,
// enforcing a hard policy that does not trust the database: even a compromised API
// cannot get a privileged container.
//
// The engine (deployments, the proxy, placement, nodes) is in internal/core/worker; each
// feature's background work is in internal/components/<feature>/agent. This package
// only wires them together.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	backups "platform/internal/components/backups/agent"
	billing "platform/internal/components/billing/agent"
	builds "platform/internal/components/builds/agent"
	dns "platform/internal/components/dns/agent"
	email "platform/internal/components/email/agent"
	functions "platform/internal/components/functions/agent"
	monitoring "platform/internal/components/monitoring/agent"
	queues "platform/internal/components/queues/agent"
	registry "platform/internal/components/registry/agent"
	services "platform/internal/components/services/agent"
	storage "platform/internal/components/storage/agent"
	"platform/internal/core/worker"
	"platform/internal/lib/config"
	"platform/internal/lib/db"
	"platform/internal/lib/docker"
	"platform/internal/lib/secure"
)

// component is the part of a feature that runs in the agent.
type component interface {
	// Start runs the component's background loops until ctx ends.
	Start(ctx context.Context)
	// Notify handles a NOTIFY payload; false when it is not the component's.
	Notify(ctx context.Context, payload string) bool
}

func components(a *worker.Agent) []component {
	return []component{
		&monitoring.Worker{Agent: a},
		&registry.Worker{Agent: a},
		&backups.Worker{Agent: a},
		&billing.Worker{Agent: a},
		&builds.Worker{Agent: a},
		&services.Worker{Agent: a},
		&storage.Worker{Agent: a},
		&functions.Worker{Agent: a},
		&queues.Worker{Agent: a},
		&email.Worker{Agent: a},
		&dns.Worker{Agent: a},
	}
}

func Run(ctx context.Context, cfg config.Config) error {
	log := slog.Default().With("component", "agent")
	if cfg.Runtime != "runsc" {
		if !cfg.AllowInsecureRuntime {
			return errors.New("refusing to start: RUNTIME must be runsc (gVisor); set ALLOW_INSECURE_RUNTIME=true only for local development")
		}
		log.Warn("INSECURE: tenant containers run WITHOUT the gVisor sandbox. Development only.", "runtime", cfg.Runtime)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	box, err := secure.LoadBox(cfg.MasterKeyFile)
	if err != nil {
		return err
	}
	dk := docker.New(cfg.DockerSocket)
	wake := func() chan struct{} { return make(chan struct{}, 1) }
	a := &worker.Agent{
		Cfg: cfg, Pool: pool, Docker: dk, Box: box,
		Caddy: worker.NewCaddyClient(cfg.CaddyAdminSocket),
		Logs:  monitoring.NewLogShipper(dk, cfg.VictoriaLogsURL, log),
		Wake:  wake(),
		Log:   log,

		ScanWake:      wake(),
		BackupWake:    wake(),
		AlertWake:     wake(),
		BuildWake:     wake(),
		CleanWake:     wake(),
		DiskWake:      wake(),
		JobWake:       wake(),
		StorageWake:   wake(),
		FnWake:        wake(),
		LogExportWake: wake(),
		MoveWake:      wake(),
		QueueWake:     wake(),
		EmailWake:     wake(),
		AlarmWake:     wake(),
		BudgetWake:    wake(),
		DNSWake:       wake(),
		Fn:            functions.NewFnState(),
		AlertHTTP:     monitoring.NewAlertHTTP(cfg.AlertAllowPrivate),
		ProbeFails:    map[string]int{},
		Attached:      map[string]bool{},
		RestoringDB:   map[string]bool{},
	}

	a.ResolveSelf(ctx)
	a.Heartbeat(ctx) // register the node before anything is placed on it

	// A deploy interrupted by a restart is retried from the start (only this node's).
	if _, err := pool.Exec(ctx, `
		UPDATE deployments d SET status = 'pending', message = 'retrying after agent restart', updated_at = now()
		FROM services s, projects p WHERE s.id = d.service_id AND p.id = s.project_id AND d.status = 'deploying' AND `+worker.MineSQL,
		a.MineArgs()...); err != nil {
		return err
	}

	comps := components(a)
	go listen(ctx, a, comps)
	go a.Logs.Run(ctx)
	go a.DiskLoop(ctx)
	go a.JanitorLoop(ctx)
	go a.NodeLoop(ctx)
	for _, c := range comps {
		c.Start(ctx)
	}

	a.Reconcile(ctx)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		a.PlaceProjects(ctx)
		a.ProcessPending(ctx)
		a.SyncCaddy(ctx, false)
		a.ReconcileDatabases(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-a.Wake:
		case <-tick.C:
			a.Reconcile(ctx)
		}
	}
}

// listen wakes the main loop, or the component concerned, on NOTIFY deployments (sent by the API).
func listen(ctx context.Context, a *worker.Agent, comps []component) {
	for ctx.Err() == nil {
		conn, err := a.Pool.Acquire(ctx)
		if err == nil {
			if _, err = conn.Exec(ctx, "LISTEN deployments"); err == nil {
				for err == nil {
					var n *pgconn.Notification
					n, err = conn.Conn().WaitForNotification(ctx)
					if err == nil {
						dispatch(ctx, a, comps, n.Payload)
					}
				}
			}
			conn.Release()
		}
		if ctx.Err() == nil {
			a.Log.Warn("notification listener restarting", "err", err)
			time.Sleep(2 * time.Second)
		}
	}
}

func dispatch(ctx context.Context, a *worker.Agent, comps []component, payload string) {
	for _, c := range comps {
		if c.Notify(ctx, payload) {
			return
		}
	}
	switch payload {
	case "disk":
		a.PokeDisk()
	case "restart":
		go a.RunRestarts(ctx)
	default:
		a.Poke()
	}
}
