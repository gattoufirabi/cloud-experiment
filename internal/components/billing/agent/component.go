package agent

import (
	"context"
	"strings"

	"platform/internal/core/worker"
)

// Worker is this component's part of the node agent: it embeds the agent's core (internal/core/worker)
// and adds this feature's background work.
type Worker struct{ *worker.Agent }

// Start runs this component's background loops until ctx ends.
func (a *Worker) Start(ctx context.Context) {
	go a.MeterLoop(ctx)
	go a.BudgetLoop(ctx)
}

// Notify handles a NOTIFY deployments payload sent by the API; it reports whether the payload was this component's.
func (a *Worker) Notify(ctx context.Context, payload string) bool {
	if month, ok := strings.CutPrefix(payload, "invoices:"); ok {
		go a.GenerateInvoices(ctx, month)
		return true
	}
	switch payload {
	case "budgets":
		a.PokeBudgets()
		return true
	}
	return false
}
