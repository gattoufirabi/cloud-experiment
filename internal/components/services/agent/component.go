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
	go a.JobLoop(ctx)
	go a.MoveLoop(ctx)
	go a.AutoscaleLoop(ctx)
	go a.ScalingLoop(ctx)
}

// Notify handles a NOTIFY deployments payload sent by the API; it reports whether the payload was this component's.
func (a *Worker) Notify(ctx context.Context, payload string) bool {
	switch payload {
	case "jobs":
		a.PokeJobs()
		return true
	case "move":
		a.PokeMoves()
		a.Poke()
		return true
	}
	return false
}
