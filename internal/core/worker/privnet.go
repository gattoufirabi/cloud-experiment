package worker

import (
	"context"
	"sort"
	"strings"
)

// Private names between services: every web service is <service>.<project>.internal on
// the private network. The name resolves (in the containers' hosts file) to the proxy's
// address on the caller's project network, and the proxy forwards the request to the
// service only when it comes from the service's own project or from a project connected
// to it (project_peerings); everything else falls through to the public routes. Traffic
// never leaves the server's private networks.

const internalSuffix = ".internal"

// fillInternal sets the subnets allowed to reach each route's private name: the networks
// of its own project and of the projects connected to it (those present on this node).
func (a *Agent) fillInternal(ctx context.Context, routes []route) {
	subnets := map[string]string{} // project ID -> subnet of its network
	if nets, err := a.Docker.NetworkList(ctx, "platform.managed=true"); err == nil {
		for _, n := range nets {
			if pid := n.Labels["platform.project"]; pid != "" && len(n.IPAM.Config) > 0 {
				subnets[pid] = n.IPAM.Config[0].Subnet
			}
		}
	}
	peers := map[string][]string{}
	if rows, err := a.Pool.Query(ctx, `SELECT project_a::text, project_b::text FROM project_peerings`); err == nil {
		for rows.Next() {
			var x, y string
			if rows.Scan(&x, &y) == nil {
				peers[x] = append(peers[x], y)
				peers[y] = append(peers[y], x)
			}
		}
		rows.Close()
	}
	for i := range routes {
		r := &routes[i]
		if r.InternalHost == "" {
			continue
		}
		var allow []string
		for _, pid := range append([]string{r.ProjectID}, peers[r.ProjectID]...) {
			if sn := subnets[pid]; sn != "" {
				allow = append(allow, sn)
			}
		}
		sort.Strings(allow)
		r.AllowFrom = allow
	}
}

// internalNames lists the private names a container of the project may call: the web
// services of its own project and of the projects connected to it.
func (a *Agent) internalNames(ctx context.Context, projectID string) []string {
	rows, err := a.Pool.Query(ctx, `
		SELECT s.slug || '.' || p.slug || '`+internalSuffix+`'
		FROM services s JOIN projects p ON p.id = s.project_id
		WHERE s.preview_of IS NULL AND (s.project_id = $1 OR s.project_id IN (
		    SELECT CASE WHEN g.project_a = $1 THEN g.project_b ELSE g.project_a END
		    FROM project_peerings g WHERE $1 IN (g.project_a, g.project_b)))
		ORDER BY 1`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out
}

// internalRoutes renders the proxy routes of the private names.
func internalRoutes(routes []route) []any {
	var out []any
	for _, r := range routes {
		// (applications on a customer's own server have no private name on the platform)
		if r.InternalHost == "" || len(r.AllowFrom) == 0 || len(r.Dials) == 0 || strings.HasPrefix(r.Dials[0], tunnelPrefix) {
			continue
		}
		ups := make([]any, 0, len(r.Dials))
		for _, d := range r.Dials {
			ups = append(ups, obj{"dial": d})
		}
		rp := obj{"handler": "reverse_proxy", "upstreams": ups}
		if len(ups) > 1 {
			rp["load_balancing"] = obj{"selection_policy": obj{"policy": "round_robin"}, "try_duration": "3s", "try_interval": "250ms"}
			rp["health_checks"] = obj{"passive": obj{"fail_duration": "20s", "max_fails": 1, "unhealthy_status": []int{502, 503, 504}}}
		}
		out = append(out, obj{
			"match":    []any{obj{"host": []string{r.InternalHost}, "remote_ip": obj{"ranges": r.AllowFrom}}},
			"handle":   []any{rp},
			"terminal": true,
		})
	}
	return out
}
