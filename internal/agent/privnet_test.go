package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"platform/internal/config"
)

func TestInternalRoutes(t *testing.T) {
	routes := []route{
		{Hosts: []string{"api-shop-acme.apps.test"}, Dials: []string{"ps-a-r1:8080"}, InternalHost: "api.shop.internal", AllowFrom: []string{"10.208.1.0/24", "10.208.2.0/24"}},
		{Hosts: []string{"pr-1.api-shop-acme.apps.test"}, Dials: []string{"ps-b-r1:8080"}},                             // preview: no private name
		{Hosts: []string{"web-blog-acme.apps.test"}, Dials: []string{"ps-c-r1:80"}, InternalHost: "web.blog.internal"}, // no network here yet
	}
	got := internalRoutes(routes)
	if len(got) != 1 {
		t.Fatalf("internal routes = %d, want 1", len(got))
	}
	b, _ := json.Marshal(got[0])
	for _, want := range []string{`"host":["api.shop.internal"]`, `"ranges":["10.208.1.0/24","10.208.2.0/24"]`, `"dial":"ps-a-r1:8080"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("route %s lacks %s", b, want)
		}
	}

	// plain HTTP install: the private routes come first in the main server
	dev := BuildCaddyConfig(config.Config{AppsDomain: "apps.test"}, routes)
	main := dev["apps"].(obj)["http"].(obj)["servers"].(obj)["main"].(obj)
	first, _ := json.Marshal(main["routes"].([]any)[0])
	if !strings.Contains(string(first), "api.shop.internal") {
		t.Errorf("dev: first route is %s, want the private name", first)
	}
	// HTTPS install: a separate plain-HTTP server on port 80
	prod := BuildCaddyConfig(config.Config{AppsDomain: "apps.test", TLSMode: "acme"}, routes)
	srv, ok := prod["apps"].(obj)["http"].(obj)["servers"].(obj)["internal"].(obj)
	if !ok {
		t.Fatal("acme: no internal server")
	}
	if l, _ := json.Marshal(srv["listen"]); string(l) != `[":80"]` {
		t.Errorf("acme: internal server listens on %s", l)
	}
	pm, _ := json.Marshal(prod["apps"].(obj)["http"].(obj)["servers"].(obj)["main"])
	if strings.Contains(string(pm), "api.shop.internal") {
		t.Error("acme: the private name must not be on the HTTPS server (no certificate for it)")
	}
}
