#!/usr/bin/env bash
# Optional: k3s for AUTOSCALE_BACKEND=kubernetes (autoscaled web services run as
# Deployments + HorizontalPodAutoscalers instead of agent-managed containers).
# Debian 12/13, run as root on the PRIMARY node, after install-gvisor.sh and once the
# platform's compose stack has created the platform-core network.
#
# What it sets up:
#   - k3s (single binary, ~500 MB RAM) without Traefik or ServiceLB: Caddy stays the edge;
#     no NodePort or LoadBalancer is ever created by the agent (Services are ClusterIP).
#   - containerd runtime "runsc" (gVisor) and a RuntimeClass "runsc" using it.
#   - secrets encrypted at rest (tenant env Secrets hold decrypted values), pod PID limit.
#   - a least-privilege ServiceAccount for the agent and its kubeconfig in
#     /srv/platform/kube/agent.yaml (mounted read-only into the agent container).
#   - the tenant firewall rules for the pod network (KUBE_POD_CIDR).
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "Run as root." >&2; exit 1; }

K3S_VERSION=${K3S_VERSION:-v1.31.4+k3s1}
POD_CIDR=${POD_CIDR:-10.42.0.0/16}
SERVICE_CIDR=${SERVICE_CIDR:-10.43.0.0/16}
CORE_NET=${CORE_NET:-platform-core}
KUBE_DIR=/srv/platform/kube

command -v containerd-shim-runsc-v1 >/dev/null || { echo "gVisor is missing: run host/install-gvisor.sh first." >&2; exit 1; }
# The agent and the proxy reach the API server and Services from the platform-core bridge.
GATEWAY=$(docker network inspect "$CORE_NET" -f '{{(index .IPAM.Config 0).Gateway}}')
[[ -n "$GATEWAY" ]] || { echo "network $CORE_NET not found: start the platform first." >&2; exit 1; }

# containerd: add the runsc runtime to k3s's generated configuration.
install -d -m 755 /var/lib/rancher/k3s/agent/etc/containerd
cat > /var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl <<'EOF'
{{ template "base" . }}

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF

curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="$K3S_VERSION" sh -s - server \
  --disable traefik --disable servicelb \
  --write-kubeconfig-mode 600 \
  --tls-san "$GATEWAY" \
  --cluster-cidr "$POD_CIDR" --service-cidr "$SERVICE_CIDR" \
  --secrets-encryption \
  --kubelet-arg=pod-max-pids=256 \
  --kube-apiserver-arg=anonymous-auth=false

until k3s kubectl get nodes 2>/dev/null | grep -q ' Ready'; do sleep 2; done

k3s kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: runsc}
handler: runsc
---
apiVersion: v1
kind: Namespace
metadata: {name: staysa-system}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: staysa-agent, namespace: staysa-system}
automountServiceAccountToken: false
---
apiVersion: v1
kind: Secret
metadata:
  name: staysa-agent-token
  namespace: staysa-system
  annotations: {kubernetes.io/service-account.name: staysa-agent}
type: kubernetes.io/service-account-token
---
# Exactly what internal/agent/kube.go uses; no exec, no attach, no RBAC, no nodes write.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: staysa-agent}
rules:
- apiGroups: [""]
  resources: [namespaces, secrets, services]
  verbs: [get, list, create, patch, update, delete]
- apiGroups: [""]
  resources: [pods]
  verbs: [get, list]
- apiGroups: [""]
  resources: [pods/log]
  verbs: [get]
- apiGroups: [""]
  resources: [nodes]
  verbs: [get, list]
- apiGroups: [apps]
  resources: [deployments]
  verbs: [get, list, create, patch, update, delete]
- apiGroups: [apps]
  resources: [deployments/scale]
  verbs: [get, patch, update]
- apiGroups: [autoscaling]
  resources: [horizontalpodautoscalers]
  verbs: [get, list, create, patch, update, delete]
- apiGroups: [networking.k8s.io]
  resources: [networkpolicies]
  verbs: [get, list, create, patch, update, delete]
- apiGroups: [node.k8s.io]
  resources: [runtimeclasses]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: staysa-agent}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: staysa-agent}
subjects: [{kind: ServiceAccount, name: staysa-agent, namespace: staysa-system}]
EOF

until TOKEN=$(k3s kubectl -n staysa-system get secret staysa-agent-token -o jsonpath='{.data.token}' 2>/dev/null) && [[ -n "$TOKEN" ]]; do sleep 1; done
CA=$(k3s kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
install -d -m 700 "$KUBE_DIR"
umask 077
cat > "$KUBE_DIR/agent.yaml" <<EOF
apiVersion: v1
kind: Config
current-context: staysa
clusters:
- name: k3s
  cluster:
    server: https://$GATEWAY:6443
    certificate-authority-data: $CA
users:
- name: staysa-agent
  user:
    token: $(echo "$TOKEN" | base64 -d)
contexts:
- name: staysa
  context: {cluster: k3s, user: staysa-agent}
EOF

# Host firewall (default-drop input). The API server listens on every address: only the
# platform-core bridge (agent) may reach it from outside the cluster. Pods may reach the
# node only on the two ports the cluster add-ons need (CoreDNS and metrics-server talk to
# the API server and the kubelet): NetworkPolicy cannot stop a pod reaching its own node,
# so this rule is what keeps tenant pods off every other host port. Both ports refuse
# anonymous requests, and tenant pods carry no service account token.
if nft list table inet host_fw >/dev/null 2>&1; then
  BRIDGE="br-$(docker network inspect "$CORE_NET" -f '{{slice .Id 0 12}}')"
  if ! nft list chain inet host_fw input | grep -q "iifname \"$BRIDGE\" tcp dport 6443"; then
    nft insert rule inet host_fw input iifname "$BRIDGE" tcp dport 6443 accept
  fi
  if ! nft list chain inet host_fw input | grep -q "ip saddr $POD_CIDR tcp dport { 6443, 10250 }"; then
    nft insert rule inet host_fw input ip saddr "$POD_CIDR" tcp dport '{ 6443, 10250 }' accept
  fi
fi

# Tenant firewall: pods get the same outbound rules as tenant containers.
if [[ -f /etc/default/platform-tenant-firewall ]] && grep -q '^KUBE_POD_CIDR=' /etc/default/platform-tenant-firewall; then
  sed -i "s#^KUBE_POD_CIDR=.*#KUBE_POD_CIDR=$POD_CIDR#" /etc/default/platform-tenant-firewall
else
  echo "KUBE_POD_CIDR=$POD_CIDR" >> /etc/default/platform-tenant-firewall
fi
systemctl restart platform-tenant-firewall.service || true

cat <<EOF
k3s is ready. Set in the platform's .env and restart the agent:
  AUTOSCALE_BACKEND=kubernetes
(the compose file mounts $KUBE_DIR and points KUBECONFIG at agent.yaml.)
Check the sandbox:  k3s kubectl run gv --rm -it --restart=Never --image=alpine:3.20 \\
  --overrides='{"spec":{"runtimeClassName":"runsc"}}' -- dmesg | grep -i gvisor
EOF
