package agent

import (
	"context"
	"time"
)

// Several servers: each project is placed on one node (all its services and databases
// together), and each agent only runs the projects placed on its own node. The primary
// node runs the control plane, Caddy, and the scheduler below.
//
// Ownership in SQL: a project is "mine" when projects.node_id is this node, or when it
// is not placed yet and this is the primary (so a single-server install behaves exactly
// as before).

// mineSQL is a condition on a projects row aliased p; arguments $1 = node ID, $2 = primary.
const mineSQL = `(p.node_id = $1 OR (p.node_id IS NULL AND $2::boolean))`

func (a *Agent) self() string    { return a.selfID }
func (a *Agent) isPrimary() bool { return a.cfg.NodeRole != "worker" }
func (a *Agent) mineArgs() []any { return []any{a.selfID, a.isPrimary()} }

// placeProjects (primary only) gives a node to every project that has something to
// run and no node yet: the online, uncordoned node with the most free memory, in the
// organization's cluster when it is pinned to one.
func (a *Agent) placeProjects(ctx context.Context) {
	if !a.isPrimary() {
		return
	}
	rows, err := a.pool.Query(ctx, `
		SELECT p.id, o.cluster,
		       coalesce((SELECT sum(s.memory_mb * s.instances) FROM services s WHERE s.project_id = p.id), 0)
		         + coalesce((SELECT sum(db.memory_mb) FROM databases db WHERE db.project_id = p.id), 0)
		FROM projects p JOIN orgs o ON o.id = p.org_id
		WHERE p.node_id IS NULL AND (
		      EXISTS (SELECT 1 FROM deployments d JOIN services s ON s.id = d.service_id
		              WHERE s.project_id = p.id AND d.status IN ('pending', 'deploying', 'live'))
		   OR EXISTS (SELECT 1 FROM databases db WHERE db.project_id = p.id))`)
	if err != nil {
		return
	}
	type pending struct {
		id, cluster string
		needMB      int64
	}
	var list []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.cluster, &p.needMB) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		var node string
		err := a.pool.QueryRow(ctx, `
			SELECT n.id FROM nodes n
			WHERE n.last_seen > now() - interval '2 minutes' AND NOT n.cordoned
			  AND ($1 = '' OR n.cluster = $1)
			ORDER BY n.mem_mb * 0.8
			       - coalesce((SELECT sum(s.memory_mb * s.instances) FROM services s JOIN projects pp ON pp.id = s.project_id WHERE pp.node_id = n.id), 0)
			       - coalesce((SELECT sum(db.memory_mb) FROM databases db JOIN projects pp ON pp.id = db.project_id WHERE pp.node_id = n.id), 0)
			       DESC, n.role = 'primary' DESC, n.id
			LIMIT 1`, p.cluster).Scan(&node)
		if err != nil {
			continue // no eligible node yet (none online in that cluster): retried next pass
		}
		if _, err := a.pool.Exec(ctx, `UPDATE projects SET node_id = $2 WHERE id = $1 AND node_id IS NULL`, p.id, node); err == nil {
			a.log.Info("project placed", "project", p.id, "node", node, "memory_mb", p.needMB)
			_, _ = a.pool.Exec(ctx, `SELECT pg_notify('deployments', '')`) // wake the node's agent
		}
	}
}

// resolveSelf fixes this agent's node ID once at start (NODE_ID or the Docker host name).
func (a *Agent) resolveSelf(ctx context.Context) {
	for ctx.Err() == nil {
		id, _ := a.nodeID(ctx)
		if id != "" {
			a.selfID = id
			return
		}
		time.Sleep(2 * time.Second)
	}
}
