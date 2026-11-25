package web

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const OwnServerOnlyMsg = "This project runs on your own server: managed databases, functions and scheduled jobs run on the platform only. Use another project for them."

// serverOnline: a server that sent a heartbeat in the last 90 seconds.
const ServerOnlineSQL = `n.last_seen > now() - interval '90 seconds'`

type ServerView struct {
	ID, Name, OS, Arch, DockerVersion, AgentVersion string
	CPUs                                            int
	MemMB, MemAvailMB, DiskFreeMB                   int64
	Load1                                           float64
	Online, Connected                               bool
	LastSeen, Created                               time.Time
	Projects                                        []string
	Builds                                          bool     // runs the organization's builds (069)
	Labels                                          []string // matched against a service's build labels
	Building                                        []int    // numbers of the builds running now
	MaxBuilds                                       int      // builds at once (070)
	Clean                                           bool     // each build starts from an empty cache
}

// AutoLabels are the labels the server has without anyone typing them: its operating
// system and architecture, as its agent reports them.
func (v ServerView) AutoLabels() []string {
	var out []string
	for _, l := range []string{strings.ToLower(v.OS), strings.ToLower(v.Arch)} {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (v ServerView) LabelText() string { return strings.Join(v.Labels, ", ") }

// onOwnServer reports whether a project runs on one of the organization's servers.
func OnOwnServer(ctx context.Context, tx pgx.Tx, projectID string) (bool, error) {
	var yes bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects p JOIN nodes n ON n.id = p.node_id
		WHERE p.id = $1 AND n.role = 'customer')`, projectID).Scan(&yes)
	return yes, err
}
