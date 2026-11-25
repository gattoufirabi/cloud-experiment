package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"platform/internal/core/worker"
)

// Health alerts probe a Kubernetes instance at its Service address, never through Docker.
func TestProbeKubeInstance(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	a := &Worker{Agent: &worker.Agent{}} // no Docker client: a Docker call would panic
	tg := probeTarget{ContainerID: "k8s:sp-x/svc-y-r1", Upstream: strings.TrimPrefix(srv.URL, "http://"), HealthPath: "/healthz"}
	if got := a.probe(context.Background(), srv.Client(), tg); got != "" {
		t.Fatalf("healthy service reported %q", got)
	}
	status = http.StatusServiceUnavailable
	if got := a.probe(context.Background(), srv.Client(), tg); !strings.Contains(got, "503") {
		t.Fatalf("got %q", got)
	}
	tg.Upstream = ""
	if got := a.probe(context.Background(), srv.Client(), tg); got != "no network address" {
		t.Fatalf("got %q", got)
	}
}
