package kube

import (
	"encoding/json"
	"strings"
	"testing"
)

func testWorkload() Workload {
	return Workload{
		OrgID: "org-1", ProjectID: "11111111-2222-3333-4444-555555555555", ServiceID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		DeploymentID: "dddddddd-0000-0000-0000-000000000001", Revision: 7,
		Image: "registry.example.test/acme/web:1", Port: 8080, HealthPath: "/healthz",
		CPUMillis: 500, MemoryMB: 256,
		Env:        []string{"PORT=8080", "SECRET=s3cr=et"},
		Hosts:      []string{"pd-0123456789ab:10.208.1.201", "console.example.test:10.208.1.253", "s3.example.test:10.208.1.253"},
		PullServer: "registry.example.test", PullUser: "agent/acme", PullPassword: "pw",
		Replicas: 2, Min: 1, Max: 5, TargetCPU: 70, RuntimeClass: "runsc",
	}
}

// The pod spec is the security boundary on the cluster: it must carry the same
// hardening as the Docker containers whatever the inputs.
func TestDeploymentIsSandboxedAndHardened(t *testing.T) {
	d := testWorkload().Deployment()
	pod, _ := field(d, "spec", "template", "spec").(Obj)
	if pod["runtimeClassName"] != "runsc" {
		t.Fatal("pods must run under the gVisor RuntimeClass")
	}
	for k, want := range map[string]any{"automountServiceAccountToken": false, "hostNetwork": false, "hostPID": false, "hostIPC": false, "enableServiceLinks": false} {
		if pod[k] != want {
			t.Errorf("%s = %v, want %v", k, pod[k], want)
		}
	}
	if _, ok := pod["volumes"]; ok {
		t.Error("no volumes (and so no hostPath) may be mounted")
	}
	c := pod["containers"].([]any)[0].(Obj)
	sc := c["securityContext"].(Obj)
	if sc["privileged"] != false || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("security context %v", sc)
	}
	caps := sc["capabilities"].(Obj)
	if drop := caps["drop"].([]any); len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("drop %v", drop)
	}
	for _, a := range caps["add"].([]any) {
		switch a {
		case "NET_RAW", "SYS_ADMIN", "SYS_PTRACE", "NET_ADMIN", "MKNOD", "SYS_MODULE":
			t.Errorf("dangerous capability %v added", a)
		}
	}
	res := c["resources"].(Obj)
	if res["requests"].(Obj)["cpu"] != "500m" || res["limits"].(Obj)["cpu"] != "500m" || res["limits"].(Obj)["memory"] != "256Mi" {
		t.Errorf("resources %v", res)
	}
	if c["envFrom"].([]any)[0].(Obj)["secretRef"].(Obj)["name"] != "svc-aaaaaaaabbbb-r7-env" {
		t.Error("environment must come from the revision's Secret")
	}
	if _, inline := c["env"]; inline {
		t.Error("no inline env: values include decrypted secrets")
	}
	if pod["imagePullSecrets"].([]any)[0].(Obj)["name"] != "staysa-registry" {
		t.Error("pull secret missing")
	}
	aliases := pod["hostAliases"].([]any)
	if len(aliases) != 2 || aliases[1].(Obj)["ip"] != "10.208.1.253" || len(aliases[1].(Obj)["hostnames"].([]any)) != 2 {
		t.Errorf("host aliases %v", aliases)
	}
	if field(d, "spec", "replicas") != 2 || Str(d, "metadata", "name") != "svc-aaaaaaaabbbb-r7" || Str(d, "metadata", "namespace") != "sp-11111111222233334444555555555555" {
		t.Errorf("deployment metadata %v", d["metadata"])
	}
	// Selector and template labels must match, or the API server refuses the Deployment.
	sel := field(d, "spec", "selector", "matchLabels").(Obj)
	tl := field(d, "spec", "template", "metadata", "labels").(Obj)
	for k, v := range sel {
		if tl[k] != v {
			t.Errorf("selector label %s=%v not on the template", k, v)
		}
	}
}

func TestEnvSecretKeepsValuesWithEquals(t *testing.T) {
	s := testWorkload().EnvSecret()
	data := s["stringData"].(Obj)
	if data["SECRET"] != "s3cr=et" || data["PORT"] != "8080" {
		t.Fatalf("stringData %v", data)
	}
	if testWorkload().PullSecret()["type"] != "kubernetes.io/dockerconfigjson" {
		t.Fatal("pull secret type")
	}
	w := testWorkload()
	w.PullUser = ""
	if w.PullSecret() != nil {
		t.Fatal("public images need no pull secret")
	}
}

func TestServiceAndHPAFollowTheRevision(t *testing.T) {
	w := testWorkload()
	svc := w.Service()
	if field(svc, "spec", "type") != "ClusterIP" {
		t.Fatal("the service must not be exposed on node ports")
	}
	if field(svc, "spec", "selector").(Obj)[LabelRevision] != "7" {
		t.Fatal("service selector must point at the revision")
	}
	h := w.HPA()
	if Str(h, "spec", "scaleTargetRef", "name") != "svc-aaaaaaaabbbb-r7" || field(h, "spec", "minReplicas") != 1 || field(h, "spec", "maxReplicas") != 5 {
		t.Fatalf("hpa %v", h["spec"])
	}
	b, _ := json.Marshal(h)
	if !strings.Contains(string(b), `"averageUtilization":70`) || !strings.Contains(string(b), `"stabilizationWindowSeconds":300`) {
		t.Fatalf("hpa %s", b)
	}
}

func TestNamespaceEnforcesPodSecurity(t *testing.T) {
	n := NamespaceObj("org-1", "11111111-2222-3333-4444-555555555555")
	if Labels(n)["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Fatal("namespaces must enforce Pod Security baseline")
	}
}

func TestIsolationPolicy(t *testing.T) {
	p := IsolationPolicy("org-1", "11111111-2222-3333-4444-555555555555",
		[]string{"10.208.1.201", "10.208.1.253", "10.208.1.253", "not-an-ip"},
		[]string{"203.0.113.7", "10.0.0.5", "2001:db8::1"},
		[]string{"172.18.0.0/16"})
	if Str(p, "metadata", "namespace") != "sp-11111111222233334444555555555555" {
		t.Fatal("policy namespace")
	}
	if sel := field(p, "spec", "podSelector").(Obj); len(sel) != 0 {
		t.Fatal("the policy must select every pod of the namespace")
	}
	types := field(p, "spec", "policyTypes").([]any)
	if len(types) != 2 {
		t.Fatal("both ingress and egress must be restricted")
	}
	b, _ := json.Marshal(p)
	s := string(b)
	for _, want := range []string{
		`"cidr":"0.0.0.0/0"`, `"10.0.0.0/8"`, `"169.254.0.0/16"`, `"172.16.0.0/12"`, `"192.168.0.0/16"`,
		`"203.0.113.7/32"`, // the node's public address: API server and kubelet
		`{"ipBlock":{"cidr":"10.208.1.201/32"}}`, `{"ipBlock":{"cidr":"10.208.1.253/32"}}`,
		`{"ipBlock":{"cidr":"172.18.0.0/16"}}`, `"k8s-app":"kube-dns"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("policy lacks %s", want)
		}
	}
	if strings.Contains(s, "not-an-ip") || strings.Contains(s, "2001:db8") || strings.Contains(s, "10.0.0.5/32") {
		t.Errorf("invalid or redundant entries leaked into the policy: %s", s)
	}
	if strings.Count(s, "10.208.1.253/32") != 1 {
		t.Error("duplicate allow entries")
	}
}

func TestNames(t *testing.T) {
	if n := Namespace("11111111-2222-3333-4444-555555555555"); len(n) > 63 {
		t.Fatalf("namespace name too long: %s", n)
	}
	if got := RevisionName("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", 12); got != "svc-aaaaaaaabbbb-r12" {
		t.Fatal(got)
	}
	if ips := HostIPs([]string{"pd-x:10.0.0.1", "bad"}); len(ips) != 1 || ips[0] != "10.0.0.1" {
		t.Fatal(ips)
	}
}
