#!/usr/bin/env bash
# Phase 0 gate: proves the host isolates tenants BEFORE any client is onboarded.
# Run as root on the production server after install-gvisor.sh and tenant-firewall.sh.
# Creates two throwaway "tenant" networks named like the agent's (pt-*), runs probes
# exactly as the agent would start a tenant container, then cleans up.
set -uo pipefail
[[ $EUID -eq 0 ]] || { echo "Run as root." >&2; exit 1; }

PASS=0; FAIL=0
ok()  { echo "  PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "  FAIL  $*"; FAIL=$((FAIL+1)); }
NA=pt-test0000000a; NB=pt-test0000000b
IMG=alpine:3.20
HOST_IP=$(ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')

cleanup() {
  docker rm -f iso-a iso-b iso-fork >/dev/null 2>&1
  docker network rm "$NA" "$NB" >/dev/null 2>&1
}
trap cleanup EXIT
cleanup
for n in "$NA" "$NB"; do
  docker network create -o com.docker.network.bridge.name="$n" "$n" >/dev/null
done
# Same hardening the agent applies (see internal/agent/agent.go containerConfig).
run() { # run <name> <network> <cmd...>
  local name=$1 net=$2; shift 2
  docker run -d --name "$name" --runtime=runsc --network "$net" --memory 128m --memory-swap 128m \
    --cpus 0.25 --pids-limit 256 --cap-drop ALL --cap-add CHOWN --cap-add SETUID --cap-add SETGID \
    --security-opt no-new-privileges:true "$IMG" "$@" >/dev/null
}
run iso-a "$NA" sh -c 'httpd -f -p 8080 -h / 2>/dev/null || (while true; do echo -e "HTTP/1.0 200 OK\r\n\r\nok" | nc -l -p 8080; done)'
run iso-b "$NB" sleep 600
sleep 2
IP_A=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NA\").IPAddress}}" iso-a)

echo "== Sandbox"
if docker exec iso-b dmesg 2>/dev/null | grep -qi gvisor; then ok "tenant container runs inside gVisor"; else bad "container is NOT sandboxed by gVisor"; fi
if docker exec iso-b cat /proc/1/cgroup >/dev/null 2>&1 && ! docker exec iso-b ls /host >/dev/null 2>&1; then ok "no host filesystem visible"; fi

echo "== Network"
if ! docker exec iso-a wget -q -T 3 -O /dev/null "http://127.0.0.1:8080/" 2>/dev/null; then
  bad "control: tenant A's test server is not answering, so the cross-tenant check below proves nothing"
fi
if docker exec iso-b wget -q -T 3 -O /dev/null "http://$IP_A:8080/" 2>/dev/null; then bad "tenant B reached tenant A ($IP_A)"; else ok "tenant B cannot reach tenant A"; fi
if docker exec iso-b wget -q -T 3 -O /dev/null "http://169.254.169.254/" 2>/dev/null; then bad "cloud metadata endpoint reachable"; else ok "cloud metadata endpoint blocked"; fi
if docker exec iso-b nc -z -w 3 "$HOST_IP" 22 2>/dev/null; then bad "host SSH reachable from tenant"; else ok "host services unreachable from tenant ($HOST_IP)"; fi
if docker exec iso-b nc -z -w 3 1.1.1.1 25 2>/dev/null; then bad "outbound SMTP (25) allowed"; else ok "outbound SMTP blocked"; fi
if docker exec iso-b wget -q -T 5 -O /dev/null https://deb.debian.org/ 2>/dev/null; then ok "public internet reachable (expected)"; else echo "  INFO  public internet not reachable from tenants (check DNS/egress if apps need it)"; fi

echo "== Resource limits"
docker run -d --name iso-fork --runtime=runsc --network "$NB" --pids-limit 64 --memory 64m "$IMG" \
  sh -c ':(){ :|:& };: ; sleep 30' >/dev/null 2>&1
sleep 5
if timeout 5 docker exec iso-b true; then ok "fork bomb in one container does not affect others"; else bad "host or neighbour became unresponsive"; fi
MEM=$(docker exec iso-b sh -c 'cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null')
echo "  INFO  memory limit seen inside container: ${MEM:-unknown}"

echo
echo "Result: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] && echo "Phase 0 gate: PASSED" || echo "Phase 0 gate: FAILED - do not onboard clients"
[ "$FAIL" -eq 0 ]
