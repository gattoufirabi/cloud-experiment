package kube

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Labels and annotations the agent puts on everything it creates. Objects without
// LabelManaged are never touched (or deleted) by the agent.
const (
	LabelManaged    = "staysa.io/managed"
	LabelOrg        = "staysa.io/org"
	LabelProject    = "staysa.io/project"
	LabelService    = "staysa.io/service"
	LabelDeployment = "staysa.io/deployment"
	LabelRevision   = "staysa.io/revision"

	PolicyName = "staysa-isolation"
)

// Capabilities a tenant container keeps: the same set as on Docker (agent.go), enough
// for common images to start and drop privileges. All are allowed by the "baseline"
// Pod Security level the namespaces enforce.
var tenantCaps = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID", "NET_BIND_SERVICE", "KILL"}

// Private and special ranges tenant pods may not reach (the host, other tenants, the
// cluster's own pods and services, cloud metadata). Mirrors host/tenant-firewall.sh.
var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8"}

// Namespace is the per-project namespace: "sp-" + the project ID without dashes.
func Namespace(projectID string) string { return "sp-" + compact(projectID) }

// ServiceName names a service's Kubernetes Service and HorizontalPodAutoscaler.
func ServiceName(serviceID string) string { return "svc-" + compact(serviceID)[:12] }

// RevisionName names the Deployment (and its env Secret) of one revision.
func RevisionName(serviceID string, revision int) string {
	return fmt.Sprintf("%s-r%d", ServiceName(serviceID), revision)
}

func compact(id string) string { return strings.ReplaceAll(id, "-", "") }

// Workload is one revision of an autoscaled web service.
type Workload struct {
	OrgID, ProjectID, ServiceID, DeploymentID string
	Revision                                  int

	Image      string
	Port       int
	HealthPath string
	CPUMillis  int
	MemoryMB   int
	Env        []string // NAME=value, already merged (project variables, env, secrets)
	Hosts      []string // name:ip entries (project databases, platform names via the proxy)

	// Registry credentials for the organization's namespace ("" = public image).
	PullServer, PullUser, PullPassword string

	Replicas, Min, Max, TargetCPU int
	RuntimeClass                  string
}

func (w Workload) ns() string { return Namespace(w.ProjectID) }

func (w Workload) baseLabels() map[string]any {
	return map[string]any{LabelManaged: "true", LabelOrg: w.OrgID, LabelProject: w.ProjectID, LabelService: w.ServiceID}
}

func (w Workload) revLabels() map[string]any {
	l := w.baseLabels()
	l[LabelDeployment] = w.DeploymentID
	l[LabelRevision] = strconv.Itoa(w.Revision)
	return l
}

// NamespaceObj is the project's namespace. Pod Security "baseline" is enforced as a
// second line behind the agent's own pod spec: no privileged pods, host namespaces,
// host paths or added capabilities beyond the safe set.
func NamespaceObj(orgID, projectID string) Obj {
	return Obj{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": Obj{"name": Namespace(projectID), "labels": Obj{
			LabelManaged: "true", LabelOrg: orgID, LabelProject: projectID,
			"pod-security.kubernetes.io/enforce":         "baseline",
			"pod-security.kubernetes.io/enforce-version": "latest",
			"pod-security.kubernetes.io/warn":            "restricted",
		}},
	}
}

// EnvSecret carries the revision's environment (it holds decrypted secrets, so it is a
// Secret, never a ConfigMap or inline env).
func (w Workload) EnvSecret() Obj {
	data := Obj{}
	for _, kv := range w.Env {
		k, v, _ := strings.Cut(kv, "=")
		data[k] = v
	}
	return Obj{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   Obj{"name": RevisionName(w.ServiceID, w.Revision) + "-env", "namespace": w.ns(), "labels": w.revLabels()},
		"stringData": data,
	}
}

// PullSecretName is the namespace's registry credential (nil object when not needed).
func (w Workload) PullSecretName() string { return "staysa-registry" }

// PullSecret is the organization-scoped registry login (read-only on its own namespace).
func (w Workload) PullSecret() Obj {
	if w.PullUser == "" {
		return nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(w.PullUser + ":" + w.PullPassword))
	cfg, _ := json.Marshal(map[string]any{"auths": map[string]any{w.PullServer: map[string]string{
		"username": w.PullUser, "password": w.PullPassword, "auth": auth}}})
	l := w.baseLabels()
	delete(l, LabelService)
	return Obj{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata":   Obj{"name": w.PullSecretName(), "namespace": w.ns(), "labels": l},
		"stringData": Obj{".dockerconfigjson": string(cfg)},
	}
}

// Deployment runs the revision's pods under gVisor with the same hardening as the Docker
// containers: no service account token, no privilege escalation, capabilities dropped,
// CPU and memory requests equal to limits (so CPU utilization is "% of the limit", the
// unit of the platform's autoscale target).
func (w Workload) Deployment() Obj {
	name := RevisionName(w.ServiceID, w.Revision)
	cpu := strconv.Itoa(w.CPUMillis) + "m"
	mem := strconv.Itoa(w.MemoryMB) + "Mi"
	container := Obj{
		"name":            "app",
		"image":           w.Image,
		"imagePullPolicy": "Always", // tags can move; the registry decides, never a node cache
		"envFrom":         []any{Obj{"secretRef": Obj{"name": name + "-env"}}},
		"ports":           []any{Obj{"name": "http", "containerPort": w.Port, "protocol": "TCP"}},
		"resources": Obj{
			"requests": Obj{"cpu": cpu, "memory": mem},
			"limits":   Obj{"cpu": cpu, "memory": mem},
		},
		"readinessProbe": Obj{
			"httpGet":          Obj{"path": healthPath(w.HealthPath), "port": w.Port},
			"periodSeconds":    5,
			"timeoutSeconds":   3,
			"failureThreshold": 3,
		},
		"securityContext": Obj{
			"privileged":               false,
			"allowPrivilegeEscalation": false,
			"capabilities":             Obj{"drop": []any{"ALL"}, "add": toAny(tenantCaps)},
		},
	}
	pod := Obj{
		"runtimeClassName":              w.RuntimeClass,
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false, // no env vars describing other services
		"hostNetwork":                   false,
		"hostPID":                       false,
		"hostIPC":                       false,
		"terminationGracePeriodSeconds": 30,
		"securityContext":               Obj{"seccompProfile": Obj{"type": "RuntimeDefault"}},
		"containers":                    []any{container},
	}
	if aliases := hostAliases(w.Hosts); len(aliases) > 0 {
		pod["hostAliases"] = aliases
	}
	if w.PullUser != "" {
		pod["imagePullSecrets"] = []any{Obj{"name": w.PullSecretName()}}
	}
	sel := Obj{LabelService: w.ServiceID, LabelRevision: strconv.Itoa(w.Revision)}
	return Obj{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": Obj{"name": name, "namespace": w.ns(), "labels": w.revLabels()},
		"spec": Obj{
			"replicas":             w.Replicas,
			"revisionHistoryLimit": 0,
			"selector":             Obj{"matchLabels": sel},
			"strategy":             Obj{"type": "RollingUpdate", "rollingUpdate": Obj{"maxUnavailable": 0, "maxSurge": "25%"}},
			"template": Obj{
				"metadata": Obj{"labels": w.revLabels()},
				"spec":     pod,
			},
		},
	}
}

// Service is the stable address the proxy dials (its ClusterIP); its selector points at
// the live revision, so switching revisions moves traffic at once.
func (w Workload) Service() Obj {
	return Obj{
		"apiVersion": "v1", "kind": "Service",
		"metadata": Obj{"name": ServiceName(w.ServiceID), "namespace": w.ns(), "labels": w.baseLabels()},
		"spec": Obj{
			"type":     "ClusterIP",
			"selector": Obj{LabelService: w.ServiceID, LabelRevision: strconv.Itoa(w.Revision)},
			"ports":    []any{Obj{"name": "http", "port": w.Port, "targetPort": w.Port, "protocol": "TCP"}},
		},
	}
}

// HPA scales the live revision on CPU, like the Docker autoscaler: up quickly (doubling
// allowed every 15 s), down only after 5 minutes of lower demand.
func (w Workload) HPA() Obj {
	return Obj{
		"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler",
		"metadata": Obj{"name": ServiceName(w.ServiceID), "namespace": w.ns(), "labels": w.baseLabels()},
		"spec": Obj{
			"scaleTargetRef": Obj{"apiVersion": "apps/v1", "kind": "Deployment", "name": RevisionName(w.ServiceID, w.Revision)},
			"minReplicas":    w.Min,
			"maxReplicas":    w.Max,
			"metrics": []any{Obj{"type": "Resource", "resource": Obj{
				"name": "cpu", "target": Obj{"type": "Utilization", "averageUtilization": w.TargetCPU}}}},
			"behavior": Obj{
				"scaleUp": Obj{"stabilizationWindowSeconds": 0, "selectPolicy": "Max", "policies": []any{
					Obj{"type": "Percent", "value": 100, "periodSeconds": 15},
					Obj{"type": "Pods", "value": 2, "periodSeconds": 15}}},
				"scaleDown": Obj{"stabilizationWindowSeconds": 300},
			},
		},
	}
}

// IsolationPolicy is the namespace's only NetworkPolicy, selecting every pod:
//   - ingress: from pods of the same project, and from the proxy and the agent's health
//     probes (ingressFrom: their containers' addresses on the platform network; the node
//     forwards their requests to the Service with that source address);
//   - egress: cluster DNS, the project's own databases and the proxy's address on the
//     project network (allowIPs), and the public internet minus private ranges and the
//     nodes' own addresses (denyIPs: the API server and kubelet listen there).
//
// Other projects' pods, other nodes and cloud metadata are therefore unreachable.
// NetworkPolicy never covers traffic between a pod and its own node: on the node, the
// host firewall (host/k3s-install.sh) only lets pods reach the API server and kubelet
// ports, which refuse anonymous requests.
func IsolationPolicy(orgID, projectID string, allowIPs, denyIPs, ingressFrom []string) Obj {
	ingressPeers := []any{Obj{"podSelector": Obj{}}}
	for _, c := range ingressFrom {
		ingressPeers = append(ingressPeers, Obj{"ipBlock": Obj{"cidr": c}})
	}
	except := append([]string(nil), privateRanges...)
	for _, ip := range denyIPs {
		a, err := netip.ParseAddr(ip)
		if err != nil || !a.Is4() || inPrivate(a) {
			continue // already excluded (or not IPv4)
		}
		except = append(except, a.String()+"/32")
	}
	egress := []any{
		Obj{ // cluster DNS (CoreDNS in kube-system)
			"to": []any{Obj{
				"namespaceSelector": Obj{"matchLabels": Obj{"kubernetes.io/metadata.name": "kube-system"}},
				"podSelector":       Obj{"matchLabels": Obj{"k8s-app": "kube-dns"}},
			}},
			"ports": []any{Obj{"protocol": "UDP", "port": 53}, Obj{"protocol": "TCP", "port": 53}},
		},
		Obj{"to": []any{Obj{"podSelector": Obj{}}}}, // the project's other services
		Obj{"to": []any{Obj{"ipBlock": Obj{"cidr": "0.0.0.0/0", "except": toAny(except)}}}},
	}
	var own []any
	seen := map[string]bool{}
	ips := append([]string(nil), allowIPs...)
	sort.Strings(ips)
	for _, ip := range ips {
		if a, err := netip.ParseAddr(ip); err == nil && a.Is4() && !seen[ip] {
			seen[ip] = true
			own = append(own, Obj{"ipBlock": Obj{"cidr": a.String() + "/32"}})
		}
	}
	if len(own) > 0 {
		egress = append(egress, Obj{"to": own})
	}
	return Obj{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": Obj{"name": PolicyName, "namespace": Namespace(projectID), "labels": Obj{
			LabelManaged: "true", LabelOrg: orgID, LabelProject: projectID}},
		"spec": Obj{
			"podSelector": Obj{},
			"policyTypes": []any{"Ingress", "Egress"},
			"ingress":     []any{Obj{"from": ingressPeers}},
			"egress":      egress,
		},
	}
}

func inPrivate(a netip.Addr) bool {
	for _, c := range privateRanges {
		if netip.MustParsePrefix(c).Contains(a) {
			return true
		}
	}
	return false
}

// HostIPs returns the IP part of name:ip host entries.
func HostIPs(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if i := strings.LastIndexByte(h, ':'); i > 0 {
			out = append(out, h[i+1:])
		}
	}
	return out
}

func hostAliases(hosts []string) []any {
	byIP := map[string][]string{}
	var order []string
	for _, h := range hosts {
		i := strings.LastIndexByte(h, ':')
		if i <= 0 {
			continue
		}
		name, ip := h[:i], h[i+1:]
		if _, err := netip.ParseAddr(ip); err != nil {
			continue
		}
		if _, ok := byIP[ip]; !ok {
			order = append(order, ip)
		}
		byIP[ip] = append(byIP[ip], name)
	}
	out := make([]any, 0, len(order))
	for _, ip := range order {
		out = append(out, Obj{"ip": ip, "hostnames": toAny(byIP[ip])})
	}
	return out
}

func healthPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
