#!/usr/bin/env bash
# Phase 0: firewall rules for tenant networks. Tenant bridges are named pt-<12 hex>
# by the agent, so one interface pattern (pt-+) covers every tenant.
#
# Tenant containers may:   talk inside their own project network (app <-> db, proxy)
#                          reach the public internet (web APIs, package mirrors)
# Kubernetes pods (KUBE_POD_CIDR set): the same, plus their project's databases.
# Tenant containers may NOT: reach the host, other Docker networks, private ranges,
#                          cloud metadata (169.254.169.254), or send email directly.
#
# Applied by platform-tenant-firewall.service after Docker starts. Idempotent.
set -euo pipefail

apply() { # apply <iptables|ip6tables> <pod CIDR or ""> <private ranges...>
  local ipt=$1 pods=$2; shift 2
  $ipt -N PLATFORM-TENANTS 2>/dev/null || $ipt -F PLATFORM-TENANTS
  $ipt -C DOCKER-USER -j PLATFORM-TENANTS 2>/dev/null || $ipt -I DOCKER-USER 1 -j PLATFORM-TENANTS
  local R="$ipt -A PLATFORM-TENANTS"
  $R -i pt-+ -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
  # No direct outbound mail: the classic abuse path that gets a server's IP blacklisted.
  $R -i pt-+ -p tcp -m multiport --dports 25,465,587 -j REJECT
  # Leaving the tenant bridge towards private address space is refused.
  for net in "$@"; do
    $R -i pt-+ ! -o pt-+ -d "$net" -j REJECT
  done
  # Kubernetes pods (AUTOSCALE_BACKEND=kubernetes, host/k3s-install.sh): the same
  # outbound rules. Their only way into a tenant bridge is the project's databases;
  # each namespace's NetworkPolicy narrows that to its own project's addresses.
  if [[ -n "$pods" ]]; then
    $R -s "$pods" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
    $R -s "$pods" -p tcp -m multiport --dports 25,465,587 -j REJECT
    $R -s "$pods" -o pt-+ -j ACCEPT
    $R -s "$pods" -d "$pods" -j RETURN
    for net in "$@"; do
      $R -s "$pods" -d "$net" -j REJECT
    done
  fi
  $R -j RETURN
}

apply iptables "${KUBE_POD_CIDR:-}" 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 127.0.0.0/8
if command -v ip6tables >/dev/null && ip6tables -L DOCKER-USER >/dev/null 2>&1; then
  apply ip6tables "" fc00::/7 fe80::/10 ::1/128
fi

# Traffic from a tenant bridge to the host itself goes through INPUT, not DOCKER-USER.
# With the demo-env nftables firewall (table inet host_fw), refuse new connections from pt-*.
if nft list table inet host_fw >/dev/null 2>&1; then
  if ! nft list chain inet host_fw input | grep -q 'iifname "pt-\*"'; then
    nft insert rule inet host_fw input iifname "pt-*" ct state new drop
  fi
fi
echo "tenant firewall applied"
