package web

import (
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	AlertKinds = []string{"service_down", "deploy_failed", "backup_failed", "critical_vulns", "cpu_high", "memory_high", "job_failed", "metric_alarm", "budget"}
	TopicRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type channelView struct {
	ID, Name, Kind, URL, Topic string
	HasSecret                  bool
	Events                     []string
	Enabled                    bool
	LastStatus                 string
	LastDelivery               *time.Time
}

type IncidentView struct {
	Kind, Status, Summary string
	Service               string
	StartedAt             time.Time
	ResolvedAt            *time.Time
}

func (i IncidentView) Title() string {
	switch i.Kind {
	case "service_down":
		return "Service down"
	case "cpu_high":
		return "High CPU"
	case "memory_high":
		return "High memory"
	case "deploy_failed":
		return "Deployment failed"
	case "backup_failed":
		return "Backup failed"
	case "critical_vulns":
		return "Critical vulnerabilities"
	case "disk_quota":
		return "Disk limit"
	case "job_failed":
		return "Scheduled job failed"
	}
	return "Test notification"
}

func loadChannels(r *http.Request, tx pgx.Tx) ([]channelView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT id, name, kind, url, topic, secret_enc IS NOT NULL, events, enabled, last_status, last_delivery_at
		FROM alert_channels ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[channelView])
}

// loadIncidents returns open incidents first, then the most recent ones.
func LoadIncidents(r *http.Request, tx pgx.Tx, serviceID string, limit int) ([]IncidentView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT i.kind, i.status, i.summary, coalesce(s.slug, ''), i.started_at, i.resolved_at
		FROM alert_incidents i LEFT JOIN services s ON s.id = i.service_id
		WHERE i.kind <> 'test' AND ($1 = '' OR i.service_id::text = $1)
		ORDER BY (i.status = 'firing') DESC, i.started_at DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[IncidentView])
}
