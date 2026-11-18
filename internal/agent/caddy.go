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
	"strings"
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

// get reads a path of the admin API (used by the health check).
func (c *caddyClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://caddy"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("caddy admin: HTTP %d", resp.StatusCode)
	}
	return b, nil
}

type route struct {
	OrgID, ProjectID string   // for access logs (Zone is the service ID)
	Cache            bool     // edge cache: keep responses that allow shared caching
	InternalHost     string   // <service>.<project>.internal ("" for previews)
	AllowFrom        []string // subnets allowed to use the private name
	Hosts            []string
	Dials            []string // one per instance
	Zone             string   // rate-limit zone name (service ID)
	WAF              bool     // OWASP Core Rule Set
	RPM              int      // requests per minute per visitor IP; 0 = no limit
	Blocked          []string // CIDR ranges refused with 403
}

func (a *Agent) loadRoutes(ctx context.Context) ([]route, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.container_name, (d.spec->>'port')::int, s.slug, p.slug, o.slug,
		       coalesce(array_agg(dm.hostname ORDER BY dm.hostname) FILTER (WHERE dm.verified_at IS NOT NULL), '{}'),
		       s.id, s.waf_enabled, coalesce(s.rate_limit_rpm, 0), s.blocked_ips::text[],
		       coalesce(s.preview_name, ''), coalesce(ps.slug, ''), d.upstream, d.instances, o.id, p.id, s.edge_cache
		FROM deployments d
		JOIN services s ON s.id = d.service_id
		LEFT JOIN services ps ON ps.id = s.preview_of
		JOIN projects p ON p.id = s.project_id
		JOIN orgs o ON o.id = d.org_id
		LEFT JOIN domains dm ON dm.service_id = s.id
		WHERE d.status = 'live' AND NOT o.suspended AND d.container_name IS NOT NULL
		  AND NOT coalesce((d.spec->>'worker')::boolean, false) -- workers have no public address
		GROUP BY d.id, s.id, ps.slug, p.slug, o.slug, d.upstream, d.instances, o.id, p.id
		ORDER BY o.slug, p.slug, s.slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (route, error) {
		var name, svc, proj, org string
		var port int
		var custom, blocked []string
		var previewName, parentSlug, upstream string
		var insts []instance
		rt := route{}
		if err := r.Scan(&name, &port, &svc, &proj, &org, &custom, &rt.Zone, &rt.WAF, &rt.RPM, &blocked, &previewName, &parentSlug, &upstream, &insts, &rt.OrgID, &rt.ProjectID, &rt.Cache); err != nil {
			return route{}, err
		}
		host := model.DefaultHost(svc, proj, org, a.cfg.AppsDomain)
		if previewName != "" { // preview: <name>.<parent>-<project>-<org>.<apps domain>
			host = previewName + "." + model.DefaultHost(parentSlug, proj, org, a.cfg.AppsDomain)
		}
		rt.Hosts = append([]string{host}, custom...)
		if previewName == "" {
			rt.InternalHost = svc + "." + proj + internalSuffix
		}
		if len(insts) == 0 { // deployed before instances were recorded
			insts = []instance{{Name: name, Upstream: upstream}}
		}
		for _, in := range insts {
			if in.Upstream != "" { // runs on another node: reached on its private address
				rt.Dials = append(rt.Dials, in.Upstream)
			} else {
				rt.Dials = append(rt.Dials, in.Name+":"+strconv.Itoa(port))
			}
		}
		rt.Blocked = blocked
		return rt, nil
	})
}

func (a *Agent) syncCaddy(ctx context.Context, force bool) {
	if !a.isPrimary() {
		return // the proxy runs on the primary node
	}
	routes, err := a.loadRoutes(ctx)
	if err != nil {
		a.log.Error("load routes", "err", err)
		return
	}
	a.fillInternal(ctx, routes)
	idx := make(map[string]hostRef, len(routes))
	for _, r := range routes {
		for _, h := range r.Hosts {
			idx[strings.ToLower(h)] = hostRef{OrgID: r.OrgID, ProjectID: r.ProjectID, ServiceID: r.Zone}
		}
	}
	a.mu.Lock()
	a.hostIndex = idx
	a.mu.Unlock()
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
	// Function URLs: <function>-<project>-<org>.fn.<apps domain>, answered by the API's
	// function gateway.
	rs = append(rs, obj{
		"match":    []any{obj{"host": []string{"*.fn." + cfg.AppsDomain}}},
		"handle":   []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": cfg.APIUpstream}}}},
		"terminal": true,
	})
	if cfg.StorageEnabled() {
		// S3 API (path-style requests; no body limit, uploads stream through).
		rs = append(rs, obj{
			"match":    []any{obj{"host": storageHosts(cfg)}},
			"handle":   []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": cfg.S3Upstream}}}},
			"terminal": true,
		})
		// Public website buckets: <bucket>.web.<apps domain> is passed to Garage's web
		// endpoint as "<bucket>.web" (its configured root domain).
		label := strings.Count(cfg.WebDomain(), ".") + 1 // the bucket's label, counted from the right
		rs = append(rs, obj{
			"match": []any{obj{"host": []string{"*." + cfg.WebDomain()}}},
			"handle": []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": cfg.S3WebUpstream}},
				"headers": obj{"request": obj{"set": obj{"Host": []string{fmt.Sprintf("{http.request.host.labels.%d}.web", label)}}}}}},
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
		ups := make([]any, 0, len(r.Dials))
		for _, d := range r.Dials {
			ups = append(ups, obj{"dial": d})
		}
		rp := obj{"handler": "reverse_proxy", "upstreams": ups}
		if len(ups) > 1 { // spread requests; skip an instance that stops answering, retry elsewhere
			rp["load_balancing"] = obj{"selection_policy": obj{"policy": "round_robin"}, "try_duration": "3s", "try_interval": "250ms"}
			// An instance that fails a request (no answer, or a gateway error from the app's own
			// proxy) is skipped for 20 s and the request is retried on another one; a whole node
			// going silent is handled by node failover (scaling.go).
			rp["health_checks"] = obj{"passive": obj{"fail_duration": "20s", "max_fails": 1, "unhealthy_status": []int{502, 503, 504}}}
		}
		if r.Cache { // edge cache in front of the application (keyed by host, so never shared between services)
			sub = append(sub, obj{"handle": []any{obj{"handler": "cache"}, rp}})
		} else {
			sub = append(sub, obj{"handle": []any{rp}})
		}
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
	// Access logs of the applications' hosts only (logger "tenant", defined in Caddy's
	// startup config): the agent turns them into per-request analytics.
	loggers := obj{}
	for _, r := range routes {
		for _, h := range r.Hosts {
			loggers[h] = []string{"tenant"}
		}
	}
	if cfg.StorageEnabled() { // S3 requests: bucket triggers start functions on uploads and deletes
		for _, h := range storageHosts(cfg) {
			loggers[h] = []string{"s3"}
		}
	}
	if len(loggers) > 0 {
		server["logs"] = obj{"logger_names": loggers, "skip_unmapped_hosts": true}
	}
	apps := obj{"http": obj{"servers": obj{"main": server}}}
	for _, r := range routes {
		if r.Cache {
			// Only responses that explicitly allow shared caching are kept: a response
			// without Cache-Control is marked private (browsers behave as before).
			apps["cache"] = obj{"ttl": "120s", "mode": "strict", "default_cache_control": "private", "max_cacheable_body_bytes": 10 << 20}
			break
		}
	}
	internal := internalRoutes(routes)
	if cfg.TLSMode == "acme" {
		// Private names are plain HTTP on the project networks: their own server on port 80
		// (Caddy still adds its HTTP->HTTPS redirects for the public hosts after them).
		if len(internal) > 0 {
			apps["http"].(obj)["servers"].(obj)["internal"] = obj{"listen": []string{":80"}, "automatic_https": obj{"disable": true}, "routes": internal}
		}
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
		server["routes"] = append(internal, rs...)
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
	if cfg.StorageEnabled() {
		hosts = append(hosts, hostOnly(cfg.S3Host))
	}
	return hosts
}

// storageHosts are the names the S3 API answers to: the public host, and the one given
// to services when it differs (development).
func storageHosts(cfg config.Config) []string {
	hosts := []string{hostOnly(cfg.S3Host)}
	if h := hostOnly(s3ServiceHost(cfg)); h != hosts[0] {
		hosts = append(hosts, h)
	}
	return hosts
}

// s3ServiceHost is the host[:port] of the endpoint given to services.
func s3ServiceHost(cfg config.Config) string {
	u := cfg.S3Endpoint()
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	return strings.TrimRight(u, "/")
}
