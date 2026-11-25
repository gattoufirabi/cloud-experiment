package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"platform/internal/lib/config"
)

// An application on a customer's server is routed to the API's tunnel listener, with the
// deployment in a header the proxy sets (overwriting the visitor's), and gets no private name.
func TestTunnelRoute(t *testing.T) {
	cfg := config.Config{AppsDomain: "apps.test", TunnelUpstream: "api:8082"}
	routes := []route{{Hosts: []string{"web.apps.test"}, Dials: []string{"tunnel:dep-1"}, Zone: "s1", Public: true,
		InternalHost: "web.p.internal", AllowFrom: []string{"10.0.0.0/24"}}}
	raw, _ := json.Marshal(BuildCaddyConfig(cfg, routes))
	s := string(raw)
	if !strings.Contains(s, `"dial":"api:8082"`) || !strings.Contains(s, `"X-Platform-Tunnel":["dep-1"]`) {
		t.Fatalf("tunnel route missing: %s", s)
	}
	if strings.Contains(s, `"dial":"tunnel:`) {
		t.Fatal("a tunnel upstream must never be dialed as an address")
	}
	if strings.Contains(s, "web.p.internal") {
		t.Fatal("applications on customer servers have no private name")
	}
	if !strings.Contains(s, `"stream_close_delay":"5m"`) {
		t.Fatal("console route must keep tunnels open across reloads")
	}
}
