package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"platform/internal/config"
)

// A service with a limit of requests in progress gets it as the proxy's per-upstream
// request count, with one instance or several; without a limit nothing is added.
func TestOverloadLimitInProxyConfig(t *testing.T) {
	cfg := config.Config{AppsDomain: "apps.test"}
	for _, dials := range [][]string{{"a:80"}, {"a:80", "b:80"}} {
		raw, _ := json.Marshal(BuildCaddyConfig(cfg, []route{{Hosts: []string{"web.apps.test"}, Dials: dials, Zone: "s1", Public: true, MaxInflight: 20}}))
		if !strings.Contains(string(raw), `"unhealthy_request_count":20`) {
			t.Errorf("%d instance(s): no request limit in %s", len(dials), raw)
		}
		if len(dials) > 1 && !strings.Contains(string(raw), `"max_fails":1`) {
			t.Errorf("the failure check of several instances was lost: %s", raw)
		}
		raw, _ = json.Marshal(BuildCaddyConfig(cfg, []route{{Hosts: []string{"web.apps.test"}, Dials: dials, Zone: "s1", Public: true}}))
		if strings.Contains(string(raw), "unhealthy_request_count") {
			t.Errorf("limit set without being asked for: %s", raw)
		}
	}
}

func TestBusyUpstream(t *testing.T) {
	ups := []upstreamLoad{{"ps-old-r3:8080", 2}, {"ps-new-r4:8080", 5}, {"ps-idle-r2:8080", 0}}
	if !busyUpstream(ups, []string{"ps-old-r3"}) {
		t.Error("old instance with requests in flight not seen")
	}
	if busyUpstream(ups, []string{"ps-idle-r2", "ps-gone-r1"}) {
		t.Error("idle or missing instances reported busy")
	}
	if busyUpstream(ups, []string{"ps-old"}) {
		t.Error("a name prefix matched another container")
	}
}
