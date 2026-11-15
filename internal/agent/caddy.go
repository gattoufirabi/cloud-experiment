package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/config"
	"platform/internal/model"
)

// caddyClient talks to Caddy's admin API over a Unix socket shared only with the agent.
type caddyClient struct{ hc *http.Client }

func newCaddyClient(socket string) *caddyClient {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &caddyClient{hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// load sets (or replaces) only the "apps" subtree. Replacing the whole config (POST /load)
// makes Caddy restart its admin endpoint mid-request; the admin block comes from Caddy's
// startup config and is never changed at runtime.
func (c *caddyClient) load(ctx context.Context, body []byte) error {
	// Host "caddy" must match admin.origins in Caddy's startup config.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://caddy/config/apps", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy load: HTTP %d: %s", resp.StatusCode, msg)
	}
	return nil
}

type route struct {
	Hosts []string
	Dial  string
}

func (a *Agent) loadRoutes(ctx context.Context) ([]route, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.container_name, (d.spec->>'port')::int, s.slug, p.slug, o.slug,
		       coalesce(array_agg(dm.hostname ORDER BY dm.hostname) FILTER (WHERE dm.verified_at IS NOT NULL), '{}')
		FROM deployments d
		JOIN services s ON s.id = d.service_id
		JOIN projects p ON p.id = s.project_id
		JOIN orgs o ON o.id = d.org_id
		LEFT JOIN domains dm ON dm.service_id = s.id
		WHERE d.status = 'live' AND NOT o.suspended AND d.container_name IS NOT NULL
		GROUP BY d.id, s.slug, p.slug, o.slug
		ORDER BY o.slug, p.slug, s.slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (route, error) {
		var name, svc, proj, org string
		var port int
		var custom []string
		if err := r.Scan(&name, &port, &svc, &proj, &org, &custom); err != nil {
			return route{}, err
		}
		hosts := append([]string{model.DefaultHost(svc, proj, org, a.cfg.AppsDomain)}, custom...)
		return route{Hosts: hosts, Dial: name + ":" + strconv.Itoa(port)}, nil
	})
}

func (a *Agent) syncCaddy(ctx context.Context, force bool) {
	routes, err := a.loadRoutes(ctx)
	if err != nil {
		a.log.Error("load routes", "err", err)
		return
	}
	body, err := json.Marshal(BuildCaddyConfig(a.cfg, routes)["apps"])
	if err != nil {
		a.log.Error("marshal caddy config", "err", err)
		return
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	a.mu.Lock()
	same := hash == a.caddyHash
	a.mu.Unlock()
	if same && !force {
		return
	}
	if err := a.caddy.load(ctx, body); err != nil {
		a.log.Error("push caddy config", "err", err)
		return
	}
	a.mu.Lock()
	a.caddyHash = hash
	a.mu.Unlock()
	if !same {
		a.log.Info("proxy routes updated", "routes", len(routes))
	}
}

type obj = map[string]any

// BuildCaddyConfig renders the complete Caddy JSON config (the agent pushes its "apps"
// part; the admin block documents what Caddy's startup config must contain).
func BuildCaddyConfig(cfg config.Config, routes []route) obj {
	rs := []any{obj{
		"match": []any{obj{"host": []string{cfg.ConsoleHost}}},
		"handle": []any{obj{"handler": "subroute", "routes": []any{
			obj{"match": []any{obj{"path": []string{"/internal/*"}}},
				"handle": []any{obj{"handler": "static_response", "status_code": 404}}},
			obj{"handle": []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": cfg.APIUpstream}}}}},
		}}},
		"terminal": true,
	}}
	if cfg.RegistryHost != "" {
		// Registry host: /token is the API's token endpoint, everything else the registry.
		rs = append(rs, obj{
			"match": []any{obj{"host": []string{hostOnly(cfg.RegistryHost)}}},
			"handle": []any{obj{"handler": "subroute", "routes": []any{
				obj{"match": []any{obj{"path": []string{"/token"}}},
					"handle": []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": cfg.APIUpstream}}}}},
				obj{"handle": []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": cfg.RegistryUpstream}}}}},
			}}},
			"terminal": true,
		})
	}
	for _, r := range routes {
		rs = append(rs, obj{
			"match": []any{obj{"host": r.Hosts}},
			"handle": []any{obj{
				"handler":   "reverse_proxy",
				"upstreams": []any{obj{"dial": r.Dial}},
			}},
			"terminal": true,
		})
	}
	rs = append(rs, obj{"handle": []any{obj{
		"handler": "static_response", "status_code": 404, "body": "No application is published at this address.\n",
	}}})

	server := obj{"routes": rs}
	apps := obj{"http": obj{"servers": obj{"main": server}}}
	if cfg.TLSMode == "acme" {
		server["listen"] = []string{":443"}
		issuer := []any{obj{"module": "acme", "ca": cfg.ACMECA}}
		apps["tls"] = obj{"automation": obj{
			// A certificate for an unknown host is only requested if the API approves it.
			"on_demand": obj{"permission": obj{"module": "http", "endpoint": cfg.APIInternalURL + "/internal/tls-ask"}},
			"policies": []any{
				obj{"subjects": platformHosts(cfg), "issuers": issuer},
				obj{"on_demand": true, "issuers": issuer},
			},
		}}
	} else {
		server["listen"] = []string{":80"}
		server["automatic_https"] = obj{"disable": true}
	}
	return obj{
		"admin": obj{"listen": "unix/" + cfg.CaddyAdminSocket, "origins": []string{"caddy"}},
		"apps":  apps,
	}
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func platformHosts(cfg config.Config) []string {
	hosts := []string{cfg.ConsoleHost}
	if cfg.RegistryHost != "" {
		hosts = append(hosts, hostOnly(cfg.RegistryHost))
	}
	return hosts
}
