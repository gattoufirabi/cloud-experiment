package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"platform/internal/config"
	"platform/internal/kube"
	"platform/internal/model"
)

func TestKubeInstanceID(t *testing.T) {
	id := kubeInstanceID("sp-abc", "svc-123-r4")
	ns, name, ok := parseKubeInstance(id)
	if !ok || ns != "sp-abc" || name != "svc-123-r4" {
		t.Fatalf("%q -> %q %q %v", id, ns, name, ok)
	}
	for _, s := range []string{"", "3f2a9c", "k8s:", "tunnel:abc"} {
		if _, _, ok := parseKubeInstance(s); ok && s != "k8s:" {
			t.Errorf("%q parsed as a Kubernetes instance", s)
		}
	}
}

func TestStartReplicas(t *testing.T) {
	for _, c := range []struct{ req, live, min, max, want int }{
		{1, 0, 1, 5, 1},
		{1, 4, 1, 5, 4}, // keep the capacity the autoscaler reached
		{3, 0, 2, 5, 3}, // scheduled scaling asked for 3
		{1, 0, 2, 5, 2}, // never below the minimum
		{9, 0, 1, 5, 5}, // never above the maximum
		{0, 0, 0, 0, 1},
	} {
		if got := startReplicas(c.req, c.live, c.min, c.max); got != c.want {
			t.Errorf("startReplicas(%d, %d, %d, %d) = %d, want %d", c.req, c.live, c.min, c.max, got, c.want)
		}
	}
}

func TestUseKube(t *testing.T) {
	web := job{AutoMin: 1, AutoMax: 4, AutoCPU: 70, Spec: model.Spec{Image: "x", Port: 8080, CPUMillis: 500, MemoryMB: 256}}
	a := &Agent{}
	if a.useKube(web) {
		t.Fatal("without the kubernetes backend everything stays on Docker")
	}
	a.kube = kube.NewForTest("http://unused", nil)
	if !a.useKube(web) {
		t.Fatal("an autoscaled web service runs on Kubernetes")
	}
	cases := map[string]func(j *job){
		"not autoscaled": func(j *job) { j.AutoMin = 0 },
		"preview":        func(j *job) { j.Preview = true },
		"worker":         func(j *job) { j.Spec.Worker = true },
		"volume":         func(j *job) { j.Spec.VolumePath = "/data" },
		"public port":    func(j *job) { j.Spec.Ports = []model.PublicPort{{PublicPort: 25565}} },
		"no CPU limit":   func(j *job) { j.Spec.CPUMillis = 0 },
	}
	for name, mod := range cases {
		j := web
		mod(&j)
		if a.useKube(j) {
			t.Errorf("%s: must stay on Docker", name)
		}
	}
}

func TestPodProblem(t *testing.T) {
	pull := kube.Obj{"status": map[string]any{"containerStatuses": []any{map[string]any{
		"state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "not found"}}}}}}
	if msg, fatal := podProblem(pull); !fatal || !strings.Contains(msg, "ImagePullBackOff") {
		t.Errorf("pull error: %q %v", msg, fatal)
	}
	oom := kube.Obj{"status": map[string]any{"containerStatuses": []any{map[string]any{
		"state":     map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
		"lastState": map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": 137.0}}}}}}
	if msg, fatal := podProblem(oom); !fatal || !strings.Contains(msg, "out of memory") {
		t.Errorf("oom: %q %v", msg, fatal)
	}
	starting := kube.Obj{"status": map[string]any{"containerStatuses": []any{map[string]any{
		"ready": false, "state": map[string]any{"running": map[string]any{}}}}}}
	if msg, fatal := podProblem(starting); fatal || msg == "" {
		t.Errorf("starting: %q %v", msg, fatal)
	}
	pending := kube.Obj{"status": map[string]any{"conditions": []any{map[string]any{
		"type": "PodScheduled", "status": "False", "message": "Insufficient cpu"}}}}
	if msg, fatal := podProblem(pending); fatal || !strings.Contains(msg, "Insufficient cpu") {
		t.Errorf("pending: %q %v", msg, fatal)
	}
}

// The agent must refuse to start when the RuntimeClass does not use gVisor.
func TestInitKubeRefusesUnsandboxedRuntimeClass(t *testing.T) {
	handler := "runc"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/node.k8s.io/v1/runtimeclasses/runsc":
			_, _ = io.WriteString(w, `{"kind":"RuntimeClass","handler":"`+handler+`"}`)
		case "/api/v1/nodes":
			_, _ = io.WriteString(w, `{"items":[{"status":{"addresses":[{"type":"InternalIP","address":"203.0.113.7"}]}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cfgYAML := "clusters: [{name: c, cluster: {server: '" + srv.URL + "'}}]\nusers: [{name: u, user: {token: t}}]\ncontexts: [{name: x, context: {cluster: c, user: u}}]\ncurrent-context: x\n"
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	newAgent := func(insecure bool) *Agent {
		return &Agent{log: slog.Default(), cfg: config.Config{AutoscaleBackend: "kubernetes", KubeConfig: path,
			KubeRuntimeClass: "runsc", AllowInsecureRuntime: insecure}}
	}
	a := newAgent(false)
	if err := a.initKube(context.Background()); err == nil || !strings.Contains(err.Error(), "gVisor") {
		t.Fatalf("a runc RuntimeClass must be refused, got %v", err)
	}
	if a.kube != nil {
		t.Fatal("no client may be kept after a refusal")
	}
	if a = newAgent(true); a.initKube(context.Background()) != nil || a.kube == nil {
		t.Fatal("development mode allows runc")
	}
	handler = "runsc"
	a = newAgent(false)
	if err := a.initKube(context.Background()); err != nil || a.kube == nil {
		t.Fatalf("runsc must be accepted: %v", err)
	}
	if len(a.kubeNodes) != 1 || a.kubeNodes[0] != "203.0.113.7" {
		t.Fatalf("node addresses %v", a.kubeNodes)
	}
	if err := (&Agent{cfg: config.Config{AutoscaleBackend: "nomad"}}).initKube(context.Background()); err == nil {
		t.Fatal("unknown backends must be refused")
	}
	if err := (&Agent{cfg: config.Config{AutoscaleBackend: "docker"}}).initKube(context.Background()); err != nil {
		t.Fatal(err)
	}
}

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
	a := &Agent{} // no Docker client: a Docker call would panic
	tg := probeTarget{ContainerID: kubeInstanceID("sp-x", "svc-y-r1"), Upstream: strings.TrimPrefix(srv.URL, "http://"), HealthPath: "/healthz"}
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

func TestPlatformAddresses(t *testing.T) {
	got := platformAddresses(map[string]string{"platform-core": "172.18.0.5", "pt-0123456789ab": "10.208.3.253", "bridge": "", "v6": "fd00::1"})
	if len(got) != 1 || got[0] != "172.18.0.5/32" {
		t.Fatalf("got %v: only the platform network address may be allowed in, never a tenant subnet", got)
	}
}

func testPod(name, ip, phase string, ready, deleting bool) kube.Obj {
	st := "False"
	if ready {
		st = "True"
	}
	meta := map[string]any{"name": name}
	if deleting {
		meta["deletionTimestamp"] = "2026-01-01T00:00:00Z"
	}
	return kube.Obj{"metadata": meta, "status": map[string]any{"phase": phase, "podIP": ip,
		"conditions": []any{map[string]any{"type": "Ready", "status": st}}}}
}

// The proxy dials ready pods; when none is ready it keeps every running one rather than
// none, and the first entry keeps the revision's instance ID.
func TestPodInstances(t *testing.T) {
	pods := []kube.Obj{
		testPod("web-b", "10.42.0.12", "Running", true, false),
		testPod("web-a", "10.42.0.11", "Running", true, false),
		testPod("web-c", "10.42.0.13", "Running", false, false), // busy or starting
		testPod("web-d", "10.42.0.14", "Running", true, true),   // being deleted
		testPod("web-e", "", "Pending", false, false),
	}
	got := podInstances("sp-x", "svc-1-r3", 8080, pods)
	want := []instance{
		{ID: "k8s:sp-x/svc-1-r3", Name: "web-a", Upstream: "10.42.0.11:8080"},
		{ID: "k8s:sp-x/web-b", Name: "web-b", Upstream: "10.42.0.12:8080"},
	}
	if !sameInstances(got, want) {
		t.Fatalf("ready pods: %+v", got)
	}
	none := podInstances("sp-x", "svc-1-r3", 8080, []kube.Obj{
		testPod("web-c", "10.42.0.13", "Running", false, false),
		testPod("web-d", "10.42.0.14", "Running", false, true),
	})
	if len(none) != 1 || none[0].Upstream != "10.42.0.13:8080" || none[0].ID != "k8s:sp-x/svc-1-r3" {
		t.Fatalf("no ready pod: %+v", none)
	}
	if len(podInstances("sp-x", "svc-1-r3", 8080, nil)) != 0 {
		t.Fatal("no pod: want no instance (the Service address is the fallback)")
	}
}
