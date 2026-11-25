package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"platform/internal/core/worker"
)

func TestAccessRecord(t *testing.T) {
	line := `{"level":"info","ts":1795000000.5,"logger":"http.log.access.tenant","msg":"handled request",
	"request":{"remote_ip":"203.0.113.7","client_ip":"203.0.113.7","proto":"HTTP/2.0","method":"GET","host":"Web-Shop-Acme.apps.example.test:443",
	"uri":"/cart?token=SECRET","headers":{"User-Agent":["Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile/15E148 Safari/604.1"],"Referer":["https://news.example.org/a"]}},
	"duration":0.0123,"size":512,"status":200}`
	idx := map[string]worker.HostRef{"web-shop-acme.apps.example.test": {OrgID: "o1", ProjectID: "p1", ServiceID: "s1"}}
	rec := accessRecord([]byte(line), idx, []byte("salt"))
	if rec == nil {
		t.Fatal("no record")
	}
	b, _ := json.Marshal(rec)
	got := string(b)
	for _, want := range []string{`"an_org":"o1"`, `"an_svc":"s1"`, `"path":"/cart"`, `"status":200`, `"class":"2xx"`, `"duration_ms":12`,
		`"browser":"Safari"`, `"os":"iOS"`, `"device":"mobile"`, `"referrer":"news.example.org"`, `"bot":"false"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "SECRET") || strings.Contains(got, "203.0.113.7") {
		t.Fatalf("query string or IP stored: %s", got)
	}
	if accessRecord([]byte(strings.Replace(line, "Web-Shop-Acme", "other", 1)), idx, []byte("salt")) != nil {
		t.Fatal("unknown host recorded")
	}
	if _, _, _, bot := parseUA("curl/8.5.0"); !bot {
		t.Fatal("curl not flagged as a bot")
	}
}
