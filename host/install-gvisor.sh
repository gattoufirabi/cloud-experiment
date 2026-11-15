#!/usr/bin/env bash
# Phase 0: install gVisor (runsc) and register it as a Docker runtime. Debian 12/13, run as root.
# gVisor is open source (Apache-2.0); packages come from the project's own release repository.
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "Run as root." >&2; exit 1; }

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg
install -d -m 755 /usr/share/keyrings
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor --yes -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
  > /etc/apt/sources.list.d/gvisor.list
apt-get update
apt-get install -y runsc

# Adds {"runtimes": {"runsc": {...}}} to /etc/docker/daemon.json, keeping existing settings.
runsc install
systemctl restart docker

echo "Checking that a container really runs inside gVisor..."
if docker run --rm --runtime=runsc alpine:3.20 dmesg 2>/dev/null | grep -qi gvisor; then
  echo "OK: runsc works. Set RUNTIME=runsc for the agent (the production default)."
else
  echo "FAILED: runsc did not start a sandboxed container. Check 'journalctl -u docker'." >&2
  exit 1
fi
