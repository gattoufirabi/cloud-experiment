package agent

import (
	"strings"
	"testing"
)

func TestSanitizeExposition(t *testing.T) {
	in := `# HELP http_requests_total Requests.
# TYPE http_requests_total counter
http_requests_total{method="get",org_id="victim"} 12
http_requests_total{org_id="victim",service_id="x", code="200"} 3
up 1
queue_depth{service="evil",name="jobs"} 4

`
	out, n := sanitizeExposition(strings.NewReader(in))
	want := "http_requests_total{method=\"get\"} 12\nhttp_requests_total{code=\"200\"} 3\nup 1\nqueue_depth{name=\"jobs\"} 4\n"
	if n != 4 || string(out) != want {
		t.Fatalf("got %d lines:\n%s", n, out)
	}
	if strings.Contains(string(out), "victim") {
		t.Fatal("reserved label kept")
	}
}
