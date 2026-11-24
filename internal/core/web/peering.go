package web

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Private connections between projects: each web service is <service>.<project>.internal
// on the private network, reachable from its own project and from the projects connected
// to it. Connecting or disconnecting redeploys both projects' services so their
// containers learn (or forget) the names. Admins manage connections.

type PeerView struct {
	Project string   `json:"project"`
	Names   []string `json:"names"` // the private names its services answer on
}

// loadPeers lists the projects connected to projectID and the names they expose.
func LoadPeers(ctx context.Context, tx pgx.Tx, projectID string) ([]PeerView, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.slug, coalesce(array_agg(s.slug || '.' || p.slug || '.internal' ORDER BY s.slug) FILTER (WHERE s.id IS NOT NULL), '{}')
		FROM project_peerings g
		JOIN projects p ON p.id = CASE WHEN g.project_a = $1 THEN g.project_b ELSE g.project_a END
		LEFT JOIN services s ON s.project_id = p.id AND s.preview_of IS NULL
		WHERE $1 IN (g.project_a, g.project_b)
		GROUP BY p.slug ORDER BY p.slug`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[PeerView])
	if out == nil {
		out = []PeerView{}
	}
	return out, err
}
