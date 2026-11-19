package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"platform/internal/config"
)

// A mail domain's own server name gets the webmail route (first) and a certificate policy.
func TestMailServerNames(t *testing.T) {
	cfg := config.Config{AppsDomain: "apps.test", ConsoleHost: "console.test", TLSMode: "acme", WebmailUpstream: "webmail:8090"}
	conf := BuildCaddyConfig(cfg, nil)
	addMailServerNames(conf, cfg, []string{"mail.shop.test"})
	b, _ := json.Marshal(conf["apps"])
	out := string(b)
	if !strings.Contains(out, `"subjects":["mail.shop.test"]`) {
		t.Errorf("no certificate policy for the name: %s", out)
	}
	routes := conf["apps"].(obj)["http"].(obj)["servers"].(obj)["main"].(obj)["routes"].([]any)
	first, _ := json.Marshal(routes[0])
	if !strings.Contains(string(first), "mail.shop.test") || !strings.Contains(string(first), "webmail:8090") {
		t.Errorf("the webmail route is not first: %s", first)
	}
	// nothing changes without names
	conf2 := BuildCaddyConfig(cfg, nil)
	addMailServerNames(conf2, cfg, nil)
	b2, _ := json.Marshal(conf2["apps"])
	if strings.Contains(string(b2), "webmail:8090") {
		t.Error("a webmail route without names")
	}
}
