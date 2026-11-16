#!/usr/bin/env bash
# Run on the PRIMARY: prints a join bundle for a new worker node.
#
#   sudo bash /opt/platform/host/node-bundle.sh --node-id node-2 --node-address 10.8.0.2 [--cluster eu-west]
#
# The bundle contains the agent's database password, the master key and the registry
# secret: send it to the new server over SSH only, never by email or chat.
# Prerequisites: WireGuard between both servers, and CONTROL_PLANE_ADDRESS (this
# server's WireGuard address) set in /opt/platform/deploy/.env, then
# "docker compose -f compose.prod.yml up -d" so the control plane listens on it.
set -euo pipefail
DIR=${PLATFORM_DIR:-/opt/platform}
NODE_ID="" NODE_ADDRESS="" CLUSTER=default
while [[ $# -gt 0 ]]; do
  case "$1" in
    --node-id) NODE_ID=$2; shift 2 ;;
    --node-address) NODE_ADDRESS=$2; shift 2 ;;
    --cluster) CLUSTER=$2; shift 2 ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
done
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
[[ $NODE_ID =~ ^[a-z0-9][a-z0-9-]{1,30}$ ]] || { echo "--node-id: 2-31 lowercase letters, digits, dashes" >&2; exit 2; }
[[ -n $NODE_ADDRESS ]] || { echo "--node-address (the new server's WireGuard address) is required" >&2; exit 2; }
ENVF="$DIR/deploy/.env"
get() { awk -v k="$1" 'index($0, k"=")==1 {print substr($0, length(k)+2); exit}' "$ENVF"; }
CP=$(get CONTROL_PLANE_ADDRESS)
[[ -n $CP ]] || { echo "set CONTROL_PLANE_ADDRESS (this server's WireGuard address) in $ENVF first" >&2; exit 1; }
{
  echo "CONTROL_PLANE_ADDRESS=$CP"
  for k in AGENT_DB_PASSWORD CONSOLE_HOST APPS_DOMAIN REGISTRY_HOST REGISTRY_INTERNAL_SECRET TRIVY_TOKEN BACKUP_REST_URL MAX_CPU_MILLIS MAX_MEMORY_MB; do
    echo "$k=$(get "$k")"
  done
  echo "NODE_ID=$NODE_ID"
  echo "NODE_ADDRESS=$NODE_ADDRESS"
  echo "NODE_CLUSTER=$CLUSTER"
  echo "MASTER_KEY_B64=$(base64 -w0 "$DIR/deploy/secrets/master.key")"
} | gzip -9 | base64 -w0
echo
