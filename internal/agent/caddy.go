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
	Hosts   []string
	Dial    string
	Zone    string   // rate-limit zone name (service ID)
	WAF     bool     // OWASP Core Rule Set
	RPM     int      // requests per minute per visitor IP; 0 = no limit
	Blocked []string // CIDR ranges refused with 403
}

func (a *Agent) loadRoutes(ctx context.Context) ([]route, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.container_name, (d.spec->>'port')::int, s.slug, p.slug, o.slug,
		       coalesce(array_agg(dm.hostname ORDER BY dm.hostname) FILTER (WHERE dm.verified_at IS NOT NULL), '{}'),
		       s.id, s.waf_enabled, coalesce(s.rate_limit_rpm, 0), s.blocked_ips::text[],
		       coalesce(s.preview_name, ''), coalesce(ps.slug, '')
		FROM deployments d
		JOIN services s ON s.id = d.service_id
		LEFT JOIN services ps ON ps.id = s.preview_of
		JOIN projects p ON p.id = s.project_id
		JOIN orgs o ON o.id = d.org_id
		LEFT JOIN domains dm ON dm.service_id = s.id
		WHERE d.status = 'live' AND NOT o.suspended AND d.container_name IS NOT NULL
		GROUP BY d.id, s.id, ps.slug, p.slug, o.slug
		ORDER BY o.slug, p.slug, s.slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (route, error) {
		var name, svc, proj, org string
		var port int
		var custom, blocked []string
		var previewName, parentSlug string
		rt := route{}
		if err := r.Scan(&name, &port, &svc, &proj, &org, &custom, &rt.Zone, &rt.WAF, &rt.RPM, &blocked, &previewName, &parentSlug); err != nil {
			return route{}, err
		}
		host := model.DefaultHost(svc, proj, org, a.cfg.AppsDomain)
		if previewName != "" { // preview: <name>.<parent>-<project>-<org>.<apps domain>
			host = previewName + "." + model.DefaultHost(parentSlug, proj, org, a.cfg.AppsDomain)
		}
		rt.Hosts = append([]string{host}, custom...)
		rt.Dial = name + ":" + strconv.Itoa(port)
		rt.Blocked = blocked
		return rt, nil
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
	// One shared WAF route (not terminal) for every service that turned it on: the rule
	// set is compiled once instead of once per service, which keeps Caddy's memory flat.
	var wafHosts []string
	for _, r := range routes {
		if r.WAF {
			wafHosts = append(wafHosts, r.Hosts...)
		}
	}
	if len(wafHosts) > 0 {
		rs = append(rs, obj{
			"match":  []any{obj{"host": wafHosts}},
			"handle": []any{obj{"handler": "waf", "load_owasp_crs": true, "directives": wafDirectives}},
		})
	}
	for _, r := range routes {
		var sub []any
		if len(r.Blocked) > 0 {
			sub = append(sub, obj{
				"match":  []any{obj{"remote_ip": obj{"ranges": r.Blocked}}},
				"handle": []any{obj{"handler": "static_response", "status_code": 403, "body": "Access denied.\n"}},
			})
		}
		if r.RPM > 0 {
			sub = append(sub, obj{"handle": []any{obj{
				"handler": "rate_limit",
				"rate_limits": obj{"svc-" + r.Zone: obj{
					"key": "{http.request.remote.host}", "window": "1m", "max_events": r.RPM,
				}},
			}}})
		}
		sub = append(sub, obj{"handle": []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": r.Dial}}}}})
		rs = append(rs, obj{
			"match":    []any{obj{"host": r.Hosts}},
			"handle":   []any{obj{"handler": "subroute", "routes": sub}},
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

// wafDirectives: OWASP Core Rule Set at paranoia level 1 (the default, few false
// positives) in blocking mode. Response bodies are not inspected (memory, latency), and
// large uploads are inspected partially instead of refused.
const wafDirectives = `Include @coraza.conf-recommended
Include @crs-setup.conf.example
Include @owasp_crs/*.conf
SecRuleEngine On
SecResponseBodyAccess Off
SecRequestBodyLimitAction ProcessPartial
SecAuditEngine Off`

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
