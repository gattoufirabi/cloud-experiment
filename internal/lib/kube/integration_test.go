//go:build kube_integration

// Integration test against a real (throwaway) k3s cluster. Run with:
//
//	KUBE_TEST_CONFIG=/path/to/k3s.yaml [KUBE_TEST_SERVER=https://127.0.0.1:16443] \
//	  go test -tags kube_integration -run Integration -v -timeout 15m ./internal/kube/
//
// It needs a RuntimeClass named by KUBE_TEST_RUNTIME_CLASS (default "runsc"), outbound
// internet for the images, and creates/deletes two namespaces of fictitious projects.
// KUBE_TEST_HPA=1 also loads the service and waits for the autoscaler to add pods.
package kube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	itProjectA = "0000000a-0000-4000-8000-00000000000a"
	itProjectB = "0000000b-0000-4000-8000-00000000000b"
	itService  = "000000c1-0000-4000-8000-0000000000c1"
	itOther    = "000000c2-0000-4000-8000-0000000000c2"
	itOrg      = "000000f1-0000-4000-8000-0000000000f1"
	itOrgB     = "000000f2-0000-4000-8000-0000000000f2"
)

func itClient(t *testing.T) *Client {
	path := os.Getenv("KUBE_TEST_CONFIG")
	if path == "" {
		t.Skip("KUBE_TEST_CONFIG not set")
	}
	c, err := Load(path, os.Getenv("KUBE_TEST_SERVER"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func itRuntimeClass() string {
	if rc := os.Getenv("KUBE_TEST_RUNTIME_CLASS"); rc != "" {
		return rc
	}
	return "runsc"
}

func itWorkload(project, org, service string, rev int, image string, port int) Workload {
	return Workload{
		OrgID: org, ProjectID: project, ServiceID: service,
		DeploymentID: fmt.Sprintf("%s-%d", service[:8], rev), Revision: rev,
		Image: image, Port: port, HealthPath: "/",
		CPUMillis: 200, MemoryMB: 128,
		Env:      []string{"PORT=" + fmt.Sprint(port), "GREETING=revision-" + fmt.Sprint(rev)},
		Replicas: 1, Min: 1, Max: 4, TargetCPU: 50, RuntimeClass: itRuntimeClass(),
	}
}

func waitReady(t *testing.T, c *Client, ns, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := c.Get(context.Background(), "Deployment", ns, name)
		if err == nil && Int(d, "status", "readyReplicas") >= want && Int(d, "status", "observedGeneration") >= Int(d, "metadata", "generation") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	pods, _ := c.List(context.Background(), "Pod", ns, "")
	t.Fatalf("%s/%s not ready; pods: %v", ns, name, pods)
}

// probe runs a one-shot pod in ns (under the tenant RuntimeClass) and reports whether
// its command succeeded.
func probe(t *testing.T, c *Client, ns, name, cmd string) bool {
	t.Helper()
	return probeImage(t, c, ns, name, "busybox:1.36", cmd)
}

func probeImage(t *testing.T, c *Client, ns, name, image, cmd string) bool {
	t.Helper()
	ctx := context.Background()
	pod := Obj{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": Obj{"name": name, "namespace": ns, "labels": Obj{LabelManaged: "true", "probe": "true"}},
		"spec": Obj{
			"restartPolicy": "Never", "runtimeClassName": itRuntimeClass(), "automountServiceAccountToken": false,
			"containers": []any{Obj{"name": "p", "image": image, "command": []any{"sh", "-c", cmd},
				"securityContext": Obj{"allowPrivilegeEscalation": false, "capabilities": Obj{"drop": []any{"ALL"}}}}},
		},
	}
	_ = c.Delete(ctx, "Pod", ns, name)
	for i := 0; i < 30; i++ { // wait for a previous pod of the same name to go away
		if _, err := c.Get(ctx, "Pod", ns, name); errors.Is(err, ErrNotFound) {
			break
		}
		time.Sleep(time.Second)
	}
	if _, err := c.Apply(ctx, pod); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		p, err := c.Get(ctx, "Pod", ns, name)
		if err == nil {
			switch Str(p, "status", "phase") {
			case "Succeeded":
				return true
			case "Failed":
				return false
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("probe %s did not finish", name)
	return false
}

func TestIntegrationBlueGreenIsolationAndHPA(t *testing.T) {
	c := itClient(t)
	ctx := context.Background()
	nsA, nsB := Namespace(itProjectA), Namespace(itProjectB)
	t.Cleanup(func() {
		if os.Getenv("KUBE_TEST_KEEP") == "" {
			_ = c.Delete(context.Background(), "Namespace", "", nsA)
			_ = c.Delete(context.Background(), "Namespace", "", nsB)
		}
	})
	if _, err := c.Get(ctx, "RuntimeClass", "", itRuntimeClass()); err != nil {
		t.Fatalf("RuntimeClass %s: %v", itRuntimeClass(), err)
	}
	nodes, err := c.List(ctx, "Node", "", "")
	if err != nil || len(nodes) == 0 {
		t.Fatalf("nodes: %v", err)
	}
	var nodeIPs []string
	for _, n := range nodes {
		for _, a := range Slice(n, "status", "addresses") {
			if m := a.(map[string]any); m["type"] == "InternalIP" {
				nodeIPs = append(nodeIPs, m["address"].(string))
			}
		}
	}

	// Two projects of two organizations, each with one web service.
	deploy := func(w Workload) {
		t.Helper()
		for _, o := range []Obj{NamespaceObj(w.OrgID, w.ProjectID), IsolationPolicy(w.OrgID, w.ProjectID, nil, nodeIPs, nil), w.EnvSecret(), w.Deployment()} {
			if _, err := c.Apply(ctx, o); err != nil {
				t.Fatal(err)
			}
		}
		waitReady(t, c, Namespace(w.ProjectID), RevisionName(w.ServiceID, w.Revision), w.Replicas)
	}
	w1 := itWorkload(itProjectA, itOrg, itService, 1, "registry.k8s.io/hpa-example:latest", 80)
	deploy(w1)
	svc, err := c.Apply(ctx, w1.Service())
	if err != nil {
		t.Fatal(err)
	}
	ip := Str(svc, "spec", "clusterIP")
	if _, err := c.Apply(ctx, w1.HPA()); err != nil {
		t.Fatal(err)
	}
	other := itWorkload(itProjectB, itOrgB, itOther, 1, "nginx:1.27-alpine", 80)
	deploy(other)
	otherSvc, err := c.Apply(ctx, other.Service())
	if err != nil {
		t.Fatal(err)
	}
	otherIP := Str(otherSvc, "spec", "clusterIP")

	// Isolation, seen from a pod of project A.
	if !probe(t, c, nsA, "same-project", "wget -T 5 -qO- http://"+ip) {
		t.Error("a pod must reach its own project's service")
	}
	if probe(t, c, nsA, "cross-project", "wget -T 5 -qO- http://"+otherIP) {
		t.Error("ISOLATION: project A reached project B's service")
	}
	// NetworkPolicy does not cover a pod's own node (the host firewall does, see
	// k3s-install.sh); what a pod can reach there must refuse it without credentials.
	for i, nip := range nodeIPs {
		for port, path := range map[string]string{"6443": "/api", "10250": "/pods"} { // API server, kubelet
			if !probeImage(t, c, nsA, fmt.Sprintf("node-anon-%d-%s", i, port), "curlimages/curl:8.11.1",
				"test \"$(curl -sk -o /dev/null -w '%{http_code}' https://"+nip+":"+port+path+")\" = 401") {
				t.Errorf("ISOLATION: %s:%s did not refuse an anonymous request from a tenant pod", nip, port)
			}
		}
	}
	if probe(t, c, nsA, "metadata", "wget -T 3 -qO- http://169.254.169.254/") {
		t.Error("ISOLATION: a tenant pod reached the metadata address")
	}
	if !probe(t, c, nsA, "dns-internet", "nslookup example.com && wget -T 8 -qO- http://example.com >/dev/null") {
		t.Error("a tenant pod must resolve names and reach the internet")
	}
	// A pod outside the tenant namespaces (where the proxy sits) reaches the service
	// only through what the policy allows: from the default namespace it is refused.
	if probe(t, c, "default", "outsider", "wget -T 5 -qO- http://"+ip) {
		t.Error("ISOLATION: a pod of another namespace reached the tenant service")
	}

	// Blue/green: revision 2 starts beside revision 1, then the Service and HPA switch.
	w2 := itWorkload(itProjectA, itOrg, itService, 2, "registry.k8s.io/hpa-example:latest", 80)
	deploy(w2)
	if _, err := c.Apply(ctx, w2.Service()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, w2.HPA()); err != nil {
		t.Fatal(err)
	}
	svc2, _ := c.Get(ctx, "Service", nsA, ServiceName(itService))
	if Str(svc2, "spec", "clusterIP") != ip {
		t.Fatal("the Service address must not change across revisions")
	}
	sel := LabelService + "=" + itService + "," + LabelRevision + "!=2"
	old, _ := c.List(ctx, "Deployment", nsA, sel)
	if len(old) != 1 {
		t.Fatalf("expected revision 1 still present, got %d", len(old))
	}
	for _, d := range old {
		_ = c.Delete(ctx, "Deployment", nsA, Str(d, "metadata", "name"))
	}
	if !probe(t, c, nsA, "after-switch", "wget -T 5 -qO- http://"+ip) {
		t.Error("the service must answer after the switch")
	}

	if os.Getenv("KUBE_TEST_HPA") == "" {
		return
	}
	// Load: the HorizontalPodAutoscaler must add pods (metrics-server reports every 15 s).
	load := Obj{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": Obj{"name": "load", "namespace": nsA},
		"spec": Obj{"runtimeClassName": itRuntimeClass(), "automountServiceAccountToken": false,
			"containers": []any{Obj{"name": "l", "image": "busybox:1.36", "command": []any{"sh", "-c",
				"for i in 1 2 3 4; do (while true; do wget -q -O- http://" + ip + " >/dev/null; done) & done; wait"}}}},
	}
	if _, err := c.Apply(ctx, load); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		h, err := c.Get(ctx, "HorizontalPodAutoscaler", nsA, ServiceName(itService))
		if err == nil && Int(h, "status", "desiredReplicas") > 1 {
			t.Logf("autoscaler: %d -> %d pods", Int(h, "status", "currentReplicas"), Int(h, "status", "desiredReplicas"))
			return
		}
		time.Sleep(10 * time.Second)
	}
	h, _ := c.Get(ctx, "HorizontalPodAutoscaler", nsA, ServiceName(itService))
	t.Fatalf("the autoscaler did not scale up: %s", strings.TrimSpace(fmt.Sprint(h["status"])))
}
