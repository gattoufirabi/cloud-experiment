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
	go a.MetricsLoop(ctx)
	go a.AlertLoop(ctx)
	go a.ScrapeLoop(ctx)
	go a.AccessLogLoop(ctx)
	go a.AlarmLoop(ctx)
	go a.LogExportLoop(ctx)
}

// Notify handles a NOTIFY deployments payload sent by the API; it reports whether the payload was this component's.
func (a *Worker) Notify(ctx context.Context, payload string) bool {
	switch payload {
	case "alert":
		a.PokeAlerts()
		return true
	case "logexport":
		a.PokeLogExport()
		return true
	case "alarms":
		a.PokeAlarms()
		return true
	}
	return false
}
