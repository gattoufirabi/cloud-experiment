package agent

import (
	"context"
	"strconv"
	"strings"
)

// Exposure of services on the proxy, on top of the route of each live deployment:
//   - a private service keeps its private name but has no public address or domains;
//   - extra HTTP ports: <label>.<the service's address>, each forwarded to that port;
//   - path rules: a custom domain sends a path prefix (and what is below it) to another
//     service of the organization, optionally without the prefix.

type pathRule struct {
	Prefix string
	Strip  bool
	Dials  []string
}

// expandRoutes applies the services' exposure settings to the loaded routes.
func (a *Agent) expandRoutes(ctx context.Context, routes []route) []route {
	byService := map[string]int{} // service ID -> index of its main route (previews excluded)
	for i, r := range routes {
		if r.InternalHost != "" {
			byService[r.Zone] = i
		}
	}
	// extra HTTP ports (only for instances on this node: other nodes publish the main port only)
	if rows, err := a.pool.Query(ctx, `SELECT service_id::text, label, port FROM service_ports ORDER BY service_id, label`); err == nil {
		var extra []route
		for rows.Next() {
			var sid, label string
			var port int
			if rows.Scan(&sid, &label, &port) != nil {
				continue
			}
			i, ok := byService[sid]
			if !ok || !routes[i].Public || len(routes[i].Names) == 0 || len(routes[i].Names) != len(routes[i].Dials) {
				continue
			}
			base := routes[i]
			r := base
			r.Hosts = []string{label + "." + base.Hosts[0]}
			r.Dials = nil
			for _, n := range base.Names {
				r.Dials = append(r.Dials, n+":"+strconv.Itoa(port))
			}
			r.InternalHost, r.ZoneSuffix, r.Cache, r.PathRules = "", "-"+label, false, nil
			extra = append(extra, r)
		}
		rows.Close()
		routes = append(routes, extra...)
	}
	// path rules of custom domains
	type rule struct {
		host, owner, target, prefix string
		strip                       bool
	}
	var rules []rule
	if rows, err := a.pool.Query(ctx, `
		SELECT dm.hostname, dm.service_id::text, dp.service_id::text, dp.prefix, dp.strip
		FROM domain_paths dp JOIN domains dm ON dm.id = dp.domain_id
		WHERE dm.verified_at IS NOT NULL
		ORDER BY dm.hostname, length(dp.prefix) DESC`); err == nil {
		for rows.Next() {
			var rl rule
			if rows.Scan(&rl.host, &rl.owner, &rl.target, &rl.prefix, &rl.strip) == nil {
				rules = append(rules, rl)
			}
		}
		rows.Close()
	}
	byHost := map[string][]pathRule{}
	owners := map[string]string{}
	for _, rl := range rules {
		ti, ok := byService[rl.target]
		if !ok || len(routes[ti].Dials) == 0 {
			continue // the target is not live: its paths fall through to the domain's service
		}
		byHost[rl.host] = append(byHost[rl.host], pathRule{Prefix: rl.prefix, Strip: rl.strip, Dials: routes[ti].Dials})
		owners[rl.host] = rl.owner
	}
	for host, prs := range byHost {
		oi, ok := byService[owners[host]]
		if !ok || !routes[oi].Public {
			continue
		}
		// the domain gets its own route: the owner's protections, its paths, then the owner
		kept := routes[oi].Hosts[:0:0]
		for _, h := range routes[oi].Hosts {
			if !strings.EqualFold(h, host) {
				kept = append(kept, h)
			}
		}
		routes[oi].Hosts = kept
		r := routes[oi]
		r.Hosts, r.InternalHost, r.PathRules = []string{host}, "", prs
		routes = append(routes, r)
	}
	// private services: no public host at all (custom domains included)
	for i := range routes {
		if !routes[i].Public {
			routes[i].Hosts = nil
		}
	}
	return routes
}
