package agent

import (
	"context"

	"platform/internal/core/worker"
)

// Worker is this component's part of the node agent: it embeds the agent's core (internal/core/worker)
// and adds this feature's background work.
type Worker struct{ *worker.Agent }

// Start runs this component's background loops until ctx ends.
func (a *Worker) Start(ctx context.Context) {
	go a.ScanLoop(ctx)
	go a.RegistryCleanLoop(ctx)
}

// Notify handles a NOTIFY deployments payload sent by the API; it reports whether the payload was this component's.
func (a *Worker) Notify(ctx context.Context, payload string) bool {
	switch payload {
	case "scan":
		a.PokeScans()
		return true
	case "registry-clean":
		a.PokeRegistryClean()
		return true
	}
	return false
}
