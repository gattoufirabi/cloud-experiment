package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/kube"
	"platform/internal/model"
	"platform/internal/registry"
)

// Kubernetes autoscaling backend (AUTOSCALE_BACKEND=kubernetes).
//
// Autoscaled web services of the primary node run on a k3s cluster on the same host
// instead of Docker containers: each revision is a Deployment in the project's namespace,
// its ready pods are the proxy's upstreams (recorded as the deployment's instances and
// refreshed every 2 s by kubeUpstreamLoop, the Service's ClusterIP being the fallback),
// and a HorizontalPodAutoscaler replaces the agent's CPU loop (autoscale.go) for them. Everything else (databases, workers,
// functions, builds, services with volumes or public ports, previews) stays on Docker.
//
// The isolation guarantees are kept: pods run under the RuntimeClass whose handler is
// runsc (checked at startup), with the Docker hardening (no token, capabilities dropped,
// no privilege escalation, limits) and Pod Security "baseline" enforced per namespace;
// one namespace per project with a default-deny NetworkPolicy (IsolationPolicy); the API
// still never talks to the cluster: only the agent holds the kubeconfig.
//
// Deploys keep the blue/green model of the Docker path: the new revision's Deployment
// starts beside the live one, the Service selector and the autoscaler switch to it once
// all its pods are ready, then the previous revision is deleted. A failed revision is
// deleted and the live one keeps serving.

const kubePrefix = "k8s:"

func kubeInstanceID(ns, name string) string { return kubePrefix + ns + "/" + name }

func parseKubeInstance(id string) (ns, name string, ok bool) {
	rest, found := strings.CutPrefix(id, kubePrefix)
	if !found {
		return "", "", false
	}
	return strings.Cut(rest, "/")
}

// initKube connects to the cluster when the backend is "kubernetes" and refuses to start
// when tenant pods would not be sandboxed.
func (a *Agent) initKube(ctx context.Context) error {
	switch a.cfg.AutoscaleBackend {
	case "", "docker":
		return nil
	case "kubernetes":
	default:
		return fmt.Errorf("AUTOSCALE_BACKEND must be docker or kubernetes, not %q", a.cfg.AutoscaleBackend)
	}
	if !a.isPrimary() {
		a.log.Info("AUTOSCALE_BACKEND=kubernetes applies to the primary node; this node autoscales with Docker")
		return nil
	}
	c, err := kube.Load(a.cfg.KubeConfig, a.cfg.KubeServer)
	if err != nil {
		return fmt.Errorf("AUTOSCALE_BACKEND=kubernetes: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rc, err := c.Get(cctx, "RuntimeClass", "", a.cfg.KubeRuntimeClass)
	if err != nil {
		return fmt.Errorf("AUTOSCALE_BACKEND=kubernetes: RuntimeClass %q: %w (host/k3s-install.sh creates it)", a.cfg.KubeRuntimeClass, err)
	}
	if h := kube.Str(rc, "handler"); h != "runsc" {
		if !a.cfg.AllowInsecureRuntime {
			return fmt.Errorf("refusing to start: RuntimeClass %q uses handler %q, tenant pods must run under gVisor (runsc)", a.cfg.KubeRuntimeClass, h)
		}
		a.log.Warn("INSECURE: tenant pods run WITHOUT the gVisor sandbox. Development only.", "runtime_class", a.cfg.KubeRuntimeClass, "handler", h)
	}
	a.kube = c
	a.kubeNodes = a.nodeAddresses(cctx)
	a.log.Info("autoscaled services run on Kubernetes", "runtime_class", a.cfg.KubeRuntimeClass, "nodes", len(a.kubeNodes))
	return nil
}

// useKube tells whether a revision runs on the cluster: an autoscaled web service with
// no volume, no public TCP/UDP port and no preview (the same services the Docker
// autoscaler handles).
func (a *Agent) useKube(j job) bool {
	s := j.Spec
	return a.kube != nil && j.AutoMin > 0 && j.AutoMax >= j.AutoMin && j.AutoCPU > 0 && !j.Preview &&
		!s.Worker && s.Port > 0 && s.VolumePath == "" && len(s.Ports) == 0 && s.CPUMillis > 0
}

func (a *Agent) workload(j job, env, hosts []string, replicas int) kube.Workload {
	w := kube.Workload{
		OrgID: j.OrgID, ProjectID: j.ProjectID, ServiceID: j.ServiceID, DeploymentID: j.ID, Revision: j.Revision,
		Image: j.Spec.Image, Port: j.Spec.Port, HealthPath: j.Spec.HealthPath,
		CPUMillis: j.Spec.CPUMillis, MemoryMB: j.Spec.MemoryMB,
		Env: env, Hosts: hosts,
		Replicas: replicas, Min: j.AutoMin, Max: j.AutoMax, TargetCPU: j.AutoCPU,
		RuntimeClass: a.cfg.KubeRuntimeClass,
	}
	if registry.IsPlatformImage(j.Spec.Image, a.cfg.RegistryHost) && a.cfg.RegistryInternalSecret != "" {
		w.PullServer = a.cfg.RegistryHost
		w.PullUser = registry.AgentUserPrefix + j.OrgSlug
		w.PullPassword = registry.InternalPassword(a.cfg.RegistryInternalSecret, j.OrgSlug)
	}
	return w
}

// startReplicas: the requested count, or more if the live revision already runs more
// pods (a deploy under load must not drop capacity), within the autoscaling bounds.
func startReplicas(requested, live, minN, maxN int) int {
	n := max(requested, live, minN, 1)
	if maxN > 0 && n > maxN {
		n = maxN
	}
	return n
}

func (a *Agent) deployKube(ctx context.Context, j job, netName string) error {
	if a.cfg.ScannerEnabled {
		if err := a.gate(ctx, j); err != nil {
			return err
		}
	}
	env, err := a.buildEnv(ctx, j)
	if err != nil {
		return err
	}
	hosts := a.projectHosts(ctx, j.ProjectID, netName)
	ns := kube.Namespace(j.ProjectID)
	svcName := kube.ServiceName(j.ServiceID)
	live := 0
	if deps, err := a.kube.List(ctx, "Deployment", ns, kube.LabelService+"="+j.ServiceID); err == nil {
		for _, d := range deps {
			live = max(live, kube.Int(d, "status", "replicas"))
		}
	}
	w := a.workload(j, env, hosts, startReplicas(j.Spec.Count(), live, j.AutoMin, j.AutoMax))
	name := kube.RevisionName(j.ServiceID, j.Revision)

	a.setMessage(ctx, j.ID, "preparing the Kubernetes namespace")
	if _, err := a.kube.Apply(ctx, kube.NamespaceObj(j.OrgID, j.ProjectID)); err != nil {
		return err
	}
	if _, err := a.kube.Apply(ctx, kube.IsolationPolicy(j.OrgID, j.ProjectID, kube.HostIPs(hosts), a.kubeNodes, a.kubeIngress(ctx))); err != nil {
		return err
	}
	if ps := w.PullSecret(); ps != nil {
		if _, err := a.kube.Apply(ctx, ps); err != nil {
			return err
		}
	}
	cleanup := func() {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.kube.Delete(bg, "Deployment", ns, name)
		_ = a.kube.Delete(bg, "Secret", ns, name+"-env")
	}
	if _, err := a.kube.Apply(ctx, w.EnvSecret()); err != nil {
		cleanup()
		return err
	}
	if w.Replicas > 1 {
		a.setMessage(ctx, j.ID, fmt.Sprintf("starting %d pods", w.Replicas))
	} else {
		a.setMessage(ctx, j.ID, "starting pod")
	}
	if _, err := a.kube.Apply(ctx, w.Deployment()); err != nil {
		cleanup()
		return err
	}
	a.setMessage(ctx, j.ID, "waiting for health check")
	if err := a.waitKubeReady(ctx, ns, name, j.ID, w.Replicas); err != nil {
		cleanup()
		return err
	}

	// The Service keeps its address across revisions: create it (pointing at this
	// revision) only the first time, otherwise switch it after the database says live.
	svc, err := a.kube.Get(ctx, "Service", ns, svcName)
	if errors.Is(err, kube.ErrNotFound) {
		svc, err = a.kube.Apply(ctx, w.Service())
	}
	if err != nil {
		cleanup()
		return err
	}
	ip := kube.Str(svc, "spec", "clusterIP")
	if ip == "" || ip == "None" {
		cleanup()
		return errors.New("the Kubernetes service has no cluster address")
	}
	// The proxy dials the new revision's ready pods directly (kubeUpstreamLoop keeps the
	// list current as the autoscaler adds and removes pods); the Service address is only
	// the fallback when no pod can be listed.
	pods, _ := a.kube.List(ctx, "Pod", ns, kube.LabelDeployment+"="+j.ID)
	insts := podInstances(ns, name, j.Spec.Port, pods)
	if len(insts) == 0 {
		insts = []instance{{ID: kubeInstanceID(ns, name), Name: svcName, Upstream: net.JoinHostPort(ip, strconv.Itoa(j.Spec.Port))}}
	}
	old, err := a.promote(ctx, j, insts)
	if err != nil {
		cleanup()
		return err
	}
	a.syncCaddy(ctx, true) // traffic moves to the new pods before the previous revision is deleted
	// Traffic and scaling move to the new revision.
	if _, err := a.kube.Apply(ctx, w.Service()); err != nil {
		a.log.Error("switch Kubernetes service", "deployment", j.ID, "err", err)
	}
	if _, err := a.kube.Apply(ctx, w.HPA()); err != nil {
		a.log.Error("apply autoscaler", "deployment", j.ID, "err", err)
	}
	// Let the proxy and kube-proxy pick up the change, then retire what ran before:
	// earlier revisions on the cluster and, when the service moved here, its containers.
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}
	a.retireKubeRevisions(ctx, ns, j.ServiceID, j.Revision)
	var oldNames []string
	for _, oid := range old {
		if _, _, isKube := parseKubeInstance(oid); oid == "" || isKube {
			continue
		}
		if info, err := a.dk.ContainerInspect(ctx, oid); err == nil {
			oldNames = append(oldNames, strings.TrimPrefix(info.Name, "/"))
		}
	}
	a.waitDrained(ctx, oldNames, 15*time.Second)
	for _, oid := range old {
		if _, _, isKube := parseKubeInstance(oid); oid != "" && !isKube {
			_ = a.dk.ContainerStop(ctx, oid, 10)
			_ = a.dk.ContainerRemove(ctx, oid)
		}
	}
	_, _ = a.pool.Exec(ctx, `UPDATE services SET instances = $2 WHERE id = $1`, j.ServiceID, w.Replicas)
	_ = db.Audit(ctx, a.pool, j.OrgID, "", "deployment.live", "service:"+j.ServiceID,
		map[string]any{"revision": j.Revision, "image": j.Spec.Image, "runtime": "kubernetes"}, "")
	a.log.Info("deployment live on Kubernetes", "deployment", j.ID, "namespace", ns, "name", name)
	a.reconcile(ctx)
	return nil
}

// retireKubeRevisions deletes a service's Deployments and env Secrets other than keepRev.
func (a *Agent) retireKubeRevisions(ctx context.Context, ns, serviceID string, keepRev int) {
	sel := kube.LabelService + "=" + serviceID + "," + kube.LabelRevision + "!=" + strconv.Itoa(keepRev)
	if deps, err := a.kube.List(ctx, "Deployment", ns, sel); err == nil {
		for _, d := range deps {
			_ = a.kube.Delete(ctx, "Deployment", ns, kube.Str(d, "metadata", "name"))
		}
	}
	if secs, err := a.kube.List(ctx, "Secret", ns, sel); err == nil {
		for _, s := range secs {
			_ = a.kube.Delete(ctx, "Secret", ns, kube.Str(s, "metadata", "name"))
		}
	}
}

// Pod states that will not fix themselves: fail the deploy at once instead of waiting.
var kubeFatal = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CrashLoopBackOff": true, "CreateContainerConfigError": true, "CreateContainerError": true,
	"RunContainerError": true,
}

func (a *Agent) waitKubeReady(ctx context.Context, ns, name, depID string, want int) error {
	deadline := time.Now().Add(4 * time.Minute) // includes the image pull
	lastErr := "no pod ready yet"
	for time.Now().Before(deadline) {
		d, err := a.kube.Get(ctx, "Deployment", ns, name)
		if err != nil {
			return fmt.Errorf("read deployment: %w", err)
		}
		if kube.Int(d, "status", "observedGeneration") >= kube.Int(d, "metadata", "generation") &&
			kube.Int(d, "status", "readyReplicas") >= want {
			return nil
		}
		for _, c := range kube.Slice(d, "status", "conditions") {
			if m, ok := c.(map[string]any); ok && m["type"] == "ReplicaFailure" && m["status"] == "True" {
				return fmt.Errorf("the cluster refused the pods: %v", m["message"])
			}
		}
		pods, _ := a.kube.List(ctx, "Pod", ns, kube.LabelDeployment+"="+depID)
		for _, p := range pods {
			if msg, fatal := podProblem(p); msg != "" {
				lastErr = msg
				if fatal {
					return fmt.Errorf("%s%s", msg, a.podTail(ctx, ns, kube.Str(p, "metadata", "name")))
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("not healthy after 4 minutes: %s", lastErr)
}

// podProblem describes why a pod is not ready, and whether waiting longer is pointless.
func podProblem(p kube.Obj) (string, bool) {
	for _, c := range kube.Slice(p, "status", "containerStatuses") {
		cs, _ := c.(map[string]any)
		if w, ok := kubeMap(cs, "state", "waiting"); ok {
			reason, _ := w["reason"].(string)
			msg, _ := w["message"].(string)
			text := strings.TrimSpace(reason + ": " + truncate(msg, 300))
			if reason == "CrashLoopBackOff" {
				if t, ok := kubeMap(cs, "lastState", "terminated"); ok {
					if r, _ := t["reason"].(string); r == "OOMKilled" {
						text = "container stopped (killed: out of memory, raise the memory limit)"
					} else {
						text = fmt.Sprintf("container stopped (exit code %v)", t["exitCode"])
					}
				}
			}
			return text, kubeFatal[reason]
		}
		if ready, _ := cs["ready"].(bool); !ready {
			if _, running := kubeMap(cs, "state", "running"); running {
				return "health check not passing yet", false
			}
		}
	}
	for _, c := range kube.Slice(p, "status", "conditions") {
		m, _ := c.(map[string]any)
		if m["type"] == "PodScheduled" && m["status"] == "False" {
			return fmt.Sprintf("pod not scheduled: %v", m["message"]), false
		}
	}
	return "", false
}

func kubeMap(m map[string]any, path ...string) (map[string]any, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = mm[p]
	}
	out, ok := cur.(map[string]any)
	return out, ok
}

// podTail returns the last log lines of a pod, for failure messages shown to the client.
func (a *Agent) podTail(ctx context.Context, ns, pod string) string {
	rc, err := a.kube.Logs(ctx, ns, pod, time.Time{}, false, 15)
	if err != nil {
		return ""
	}
	defer rc.Close()
	var lines []string
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		line := sc.Text()
		if _, msg, ok := strings.Cut(line, " "); ok {
			line = msg
		}
		lines = append(lines, truncate(line, 200))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n--- last log lines ---\n" + strings.Join(lines, "\n")
}

// ---------- proxy upstreams ----------

// kubeUpstreamLoop keeps the proxy's upstreams of Kubernetes services equal to their
// live revision's ready pods, every 2 seconds. Dialing pods directly (instead of the
// Service's ClusterIP) gives the same behavior as containers: round robin per request
// rather than per connection (kube-proxy pins a kept-alive connection to one pod),
// passive health checks, retries on another pod, and the per-instance in-flight limit.
// A pod the autoscaler adds receives traffic within seconds of passing its readiness
// probe instead of waiting for the next reconcile.
func (a *Agent) kubeUpstreamLoop(ctx context.Context) {
	if a.kube == nil || !a.isPrimary() {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.syncKubeUpstreams(ctx)
	}
}

func (a *Agent) syncKubeUpstreams(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.id::text, s.id::text, coalesce((d.spec->>'port')::int, 0), d.instances
		FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = d.org_id
		WHERE d.status = 'live' AND NOT o.suspended AND coalesce(d.instances->0->>'id', '') LIKE 'k8s:%' AND `+mineSQL, a.mineArgs()...)
	if err != nil {
		return
	}
	type live struct {
		DepID, ServiceID string
		Port             int
		Insts            []instance
	}
	lives, err := pgx.CollectRows(rows, pgx.RowToStructByPos[live])
	if err != nil || len(lives) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	all, err := a.kube.List(cctx, "Pod", "", kube.LabelManaged+"=true")
	if err != nil {
		return
	}
	byDep := map[string][]kube.Obj{}
	for _, p := range all {
		dep := kube.Labels(p)[kube.LabelDeployment]
		byDep[dep] = append(byDep[dep], p)
	}
	changed := false
	for _, l := range lives {
		ns, name, ok := parseKubeInstance(l.Insts[0].ID)
		if !ok || l.Port == 0 {
			continue
		}
		want := podInstances(ns, name, l.Port, byDep[l.DepID])
		if len(want) == 0 { // no pod running: the Service address (it answers once one is back)
			svc, err := a.kube.Get(cctx, "Service", ns, kube.ServiceName(l.ServiceID))
			ip := kube.Str(svc, "spec", "clusterIP")
			if err != nil || ip == "" || ip == "None" {
				continue
			}
			want = []instance{{ID: kubeInstanceID(ns, name), Name: kube.ServiceName(l.ServiceID), Upstream: net.JoinHostPort(ip, strconv.Itoa(l.Port))}}
		}
		if sameInstances(want, l.Insts) {
			continue
		}
		raw, _ := json.Marshal(want)
		prev, _ := json.Marshal(l.Insts)
		tag, err := a.pool.Exec(ctx, `
			UPDATE deployments SET instances = $2, upstream = $3, updated_at = now()
			WHERE id = $1 AND status = 'live' AND instances = $4::jsonb`, l.DepID, raw, want[0].Upstream, prev)
		if err == nil && tag.RowsAffected() > 0 {
			changed = true
		}
	}
	if changed {
		a.syncCaddy(ctx, false)
	}
}

// podInstances lists a revision's pods as proxy upstreams: the ready ones, or, when none
// is ready (all failing their health check under overload), every running one, so the
// service keeps answering what it can instead of returning 502 for everything. The first
// entry keeps the revision's instance ID (reconcile finds the Deployment through it).
func podInstances(ns, depName string, port int, pods []kube.Obj) []instance {
	type pod struct{ name, ip string }
	var ready, running []pod
	for _, p := range pods {
		if kube.Str(p, "metadata", "deletionTimestamp") != "" || kube.Str(p, "status", "phase") != "Running" {
			continue
		}
		ip := kube.Str(p, "status", "podIP")
		if ip == "" {
			continue
		}
		x := pod{kube.Str(p, "metadata", "name"), ip}
		running = append(running, x)
		if podReady(p) {
			ready = append(ready, x)
		}
	}
	use := ready
	if len(use) == 0 {
		use = running
	}
	sort.Slice(use, func(i, j int) bool { return use[i].name < use[j].name })
	out := make([]instance, 0, len(use))
	for i, p := range use {
		id := kubeInstanceID(ns, p.name)
		if i == 0 {
			id = kubeInstanceID(ns, depName)
		}
		out = append(out, instance{ID: id, Name: p.name, Upstream: net.JoinHostPort(p.ip, strconv.Itoa(port))})
	}
	return out
}

func podReady(p kube.Obj) bool {
	for _, c := range kube.Slice(p, "status", "conditions") {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			return m["status"] == "True"
		}
	}
	return false
}

func sameInstances(a, b []instance) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------- reconcile ----------

type kubeLive struct {
	DepID, OrgID, ProjectID, ServiceID, Namespace, Name string
	Suspended                                           bool
}

// reconcileKube keeps live Kubernetes revisions running (redeploying lost ones,
// scaling suspended organizations to zero), records the autoscaler's pod count, and
// returns the running pods for log shipping. Every minute (force) it also refreshes the
// namespaces' network policies and deletes what no live revision needs.
func (a *Agent) reconcileKube(ctx context.Context, lives []kubeLive, keep map[string]bool, force bool) []target {
	if a.kube == nil {
		// The backend was switched back to docker: move these services back to containers.
		for _, l := range lives {
			if !l.Suspended {
				a.redeploy(ctx, l.DepID, l.OrgID, l.ServiceID, "autoscaling backend is docker")
			}
		}
		return nil
	}
	var pods []target
	for _, l := range lives {
		d, err := a.kube.Get(ctx, "Deployment", l.Namespace, l.Name)
		if errors.Is(err, kube.ErrNotFound) {
			if !l.Suspended {
				a.redeploy(ctx, l.DepID, l.OrgID, l.ServiceID, "its Kubernetes deployment was lost")
			}
			continue
		}
		if err != nil {
			a.log.Warn("reconcile: read Kubernetes deployment", "deployment", l.DepID, "err", err)
			continue
		}
		replicas := kube.Int(d, "spec", "replicas")
		if l.Suspended {
			if replicas > 0 {
				a.log.Info("scaling Kubernetes deployment of suspended organization to zero", "org", l.OrgID)
				_ = a.kube.Scale(ctx, l.Namespace, l.Name, 0)
			}
			continue
		}
		if replicas == 0 { // resumed: the autoscaler is idle at zero, give it one pod to start from
			_ = a.kube.Scale(ctx, l.Namespace, l.Name, 1)
		}
		if n := kube.Int(d, "status", "replicas"); n > 0 {
			_, _ = a.pool.Exec(ctx, `UPDATE services SET instances = $2 WHERE id = $1 AND instances <> $2`, l.ServiceID, n)
		}
		list, err := a.kube.List(ctx, "Pod", l.Namespace, kube.LabelDeployment+"="+l.DepID)
		if err != nil {
			continue
		}
		for _, p := range list {
			if kube.Str(p, "status", "phase") == "Running" {
				name := kube.Str(p, "metadata", "name")
				pods = append(pods, target{ContainerID: kubeInstanceID(l.Namespace, name), DeploymentID: l.DepID,
					OrgID: l.OrgID, ProjectID: l.ProjectID, ServiceID: l.ServiceID, Instance: name})
			}
		}
	}
	if force {
		a.kubeNodes = a.nodeAddresses(ctx)
		a.refreshKubePolicies(ctx, lives)
		a.collectKube(ctx, keep)
	}
	return pods
}

// refreshKubePolicies re-applies each project's NetworkPolicy: its databases' addresses
// change when they are recreated.
func (a *Agent) refreshKubePolicies(ctx context.Context, lives []kubeLive) {
	done := map[string]bool{}
	ingress := a.kubeIngress(ctx)
	for _, l := range lives {
		if done[l.ProjectID] {
			continue
		}
		done[l.ProjectID] = true
		hosts := a.projectHosts(ctx, l.ProjectID, model.NetworkName(l.ProjectID))
		if _, err := a.kube.Apply(ctx, kube.IsolationPolicy(l.OrgID, l.ProjectID, kube.HostIPs(hosts), a.kubeNodes, ingress)); err != nil {
			a.log.Warn("refresh network policy", "project", l.ProjectID, "err", err)
		}
	}
}

// collectKube deletes the agent's objects no active deployment needs: revisions of
// deleted or moved services, their Services and autoscalers, and empty namespaces.
func (a *Agent) collectKube(ctx context.Context, keep map[string]bool) {
	sel := kube.LabelManaged + "=true"
	deps, err := a.kube.List(ctx, "Deployment", "", sel)
	if err != nil {
		return
	}
	usedSvc, usedNS := map[string]bool{}, map[string]bool{}
	for _, d := range deps {
		l := kube.Labels(d)
		ns, name := kube.Str(d, "metadata", "namespace"), kube.Str(d, "metadata", "name")
		if !keep[l[kube.LabelDeployment]] {
			a.log.Info("removing unused Kubernetes deployment", "namespace", ns, "name", name)
			_ = a.kube.Delete(ctx, "Deployment", ns, name)
			continue
		}
		usedSvc[l[kube.LabelService]] = true
		usedNS[ns] = true
	}
	if secs, err := a.kube.List(ctx, "Secret", "", sel+","+kube.LabelDeployment); err == nil {
		for _, s := range secs {
			if !keep[kube.Labels(s)[kube.LabelDeployment]] {
				_ = a.kube.Delete(ctx, "Secret", kube.Str(s, "metadata", "namespace"), kube.Str(s, "metadata", "name"))
			}
		}
	}
	for _, kind := range []string{"Service", "HorizontalPodAutoscaler"} {
		objs, err := a.kube.List(ctx, kind, "", sel)
		if err != nil {
			continue
		}
		for _, o := range objs {
			if !usedSvc[kube.Labels(o)[kube.LabelService]] {
				_ = a.kube.Delete(ctx, kind, kube.Str(o, "metadata", "namespace"), kube.Str(o, "metadata", "name"))
			}
		}
	}
	nss, err := a.kube.List(ctx, "Namespace", "", sel)
	if err != nil {
		return
	}
	for _, n := range nss {
		name := kube.Str(n, "metadata", "name")
		created, _ := time.Parse(time.RFC3339, kube.Str(n, "metadata", "creationTimestamp"))
		// A namespace is created a moment before its first Deployment: leave young ones.
		if usedNS[name] || time.Since(created) < 10*time.Minute {
			continue
		}
		a.log.Info("removing unused Kubernetes namespace", "namespace", name)
		_ = a.kube.Delete(ctx, "Namespace", "", name)
	}
}

// kubeIngress is where requests to tenant pods come from: the proxy (and the agent's
// health probes) dial the Service address from their containers on the platform's own
// Docker network, and the node forwards that traffic with the container's address as
// the source. Their addresses on tenant networks are left out: tenant containers share
// those subnets. Plus KUBE_INGRESS_FROM.
func (a *Agent) kubeIngress(ctx context.Context) []string {
	out := append([]string(nil), a.cfg.KubeIngressFrom...)
	for _, name := range []string{a.cfg.CaddyContainer, a.cfg.AgentContainer} {
		if name == "" {
			continue
		}
		info, err := a.dk.ContainerInspect(ctx, name)
		if err != nil {
			continue
		}
		ips := map[string]string{}
		for net, ep := range info.NetworkSettings.Networks {
			ips[net] = ep.IPAddress
		}
		out = append(out, platformAddresses(ips)...)
	}
	sort.Strings(out)
	return out
}

// platformAddresses returns a container's /32 addresses on non-tenant networks.
func platformAddresses(nets map[string]string) []string {
	var out []string
	for name, addr := range nets {
		if strings.HasPrefix(name, "pt-") {
			continue
		}
		if ip, err := netip.ParseAddr(addr); err == nil && ip.Is4() {
			out = append(out, ip.String()+"/32")
		}
	}
	return out
}

// nodeAddresses lists the cluster nodes' addresses: tenant pods may not reach them (the
// API server and kubelet listen there), even when they are public.
func (a *Agent) nodeAddresses(ctx context.Context) []string {
	nodes, err := a.kube.List(ctx, "Node", "", "")
	if err != nil {
		return a.kubeNodes
	}
	var out []string
	for _, n := range nodes {
		for _, ad := range kube.Slice(n, "status", "addresses") {
			if m, ok := ad.(map[string]any); ok && (m["type"] == "InternalIP" || m["type"] == "ExternalIP") {
				if s, _ := m["address"].(string); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
