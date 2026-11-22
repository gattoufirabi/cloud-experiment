#!/usr/bin/env bash
# Runs the dev end-to-end suite on a Linux test server instead of this computer: copies the
# working tree (committed or not), starts the dev stack there under its own compose project,
# runs scripts/e2e-dev.sh, and brings the log back. Nothing runs locally but ssh and tar.
#
#   E2E_HOST=user@test-server scripts/e2e-remote.sh [--setup] [--down] [-- ONLY="regex" KEEP=1 ...]
#
#   --setup   first time: installs Docker Engine, Go, python3, curl, dig, nc, zip, node, wget
#   --down    stop the dev stack afterwards (keeps its volumes and images for the next run)
#   --purge   stop it and delete its volumes (a clean slate next time)
#
# The server may run other things (Dokploy, other apps): the stack lives in /opt/platform-e2e
# under the compose project "platform-dev", publishes only 127.0.0.1 ports plus 8088/1465, and
# its agent only touches containers labelled by this platform. Results: ./e2e-remote.log
set -euo pipefail
cd "$(dirname "$0")/.."
: "${E2E_HOST:?set E2E_HOST=user@server}"
SSH_KEY=${E2E_SSH_KEY:-$HOME/.ssh/rabi-perso}
SSH=(ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o ServerAliveInterval=30 "$E2E_HOST")
DIR=${E2E_DIR:-/opt/platform-e2e}
SETUP=0 DOWN=0 PURGE=0 ENVS=()
while [ $# -gt 0 ]; do
  case $1 in
    --setup) SETUP=1 ;; --down) DOWN=1 ;; --purge) PURGE=1 ;;
    --) shift; ENVS=("$@"); break ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
  shift
done
SUDO=sudo; [ "${E2E_HOST%%@*}" = root ] && SUDO=

if [ $SETUP = 1 ]; then
  echo "== setup of $E2E_HOST"
  "${SSH[@]}" "$SUDO bash -s" <<'EOF'
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates python3 dnsutils netcat-openbsd zip unzip wget git nodejs >/dev/null
command -v docker >/dev/null || curl -fsSL https://get.docker.com | sh
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q 'go1\.2[3-9]'; then
  v=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
  curl -fsSL "https://go.dev/dl/$v.linux-$(dpkg --print-architecture).tar.gz" | tar -C /usr/local -xz
fi
ln -sf /usr/local/go/bin/go /usr/local/bin/go
echo "setup done: $(docker --version), $(go version)"
EOF
fi

echo "== copy the working tree to $E2E_HOST:$DIR"
"${SSH[@]}" "$SUDO mkdir -p $DIR && $SUDO chown \$(id -u):\$(id -g) $DIR"
# the tree as it is (uncommitted changes too), plus the dev-only secrets the dev stack needs;
# never the production secrets
git ls-files -co --exclude-standard -z | tar --null -T - -czf - deploy/dev-secrets \
  --exclude='deploy/secrets' --exclude='*.exe' --exclude='.env' \
  | "${SSH[@]}" "tar -xzf - -C $DIR"

echo "== start the dev stack (compose project platform-dev)"
"${SSH[@]}" "cd $DIR/deploy && $SUDO docker compose -f compose.dev.yml -f compose.dev.linux.yml up -d --build --remove-orphans 2>&1 | tail -3"

echo "== run the suite (log: e2e-remote.log; follows until the result)"
"${SSH[@]}" "cd $DIR && rm -f /tmp/e2e-remote.log && $SUDO env E2E_COMPOSE='-f compose.dev.yml -f compose.dev.linux.yml' ${ENVS[*]:-} setsid nohup bash scripts/e2e-dev.sh > /tmp/e2e-remote.log 2>&1 < /dev/null & disown"
until "${SSH[@]}" "grep -q '^Result:' /tmp/e2e-remote.log 2>/dev/null"; do
  sleep 30
  "${SSH[@]}" "grep -c '  PASS' /tmp/e2e-remote.log 2>/dev/null; grep '  FAIL' /tmp/e2e-remote.log 2>/dev/null | tail -1; grep '^== ' /tmp/e2e-remote.log | tail -1" | tr '\n' ' ' | sed 's/^/   passed so far: /'; echo
done
"${SSH[@]}" "cat /tmp/e2e-remote.log" > e2e-remote.log
grep -E '  FAIL|^Result:' e2e-remote.log || true

if [ $PURGE = 1 ]; then
  "${SSH[@]}" "cd $DIR/deploy && $SUDO docker compose -f compose.dev.yml -f compose.dev.linux.yml --profile multinode down -v --remove-orphans >/dev/null 2>&1"
elif [ $DOWN = 1 ]; then
  "${SSH[@]}" "cd $DIR/deploy && $SUDO docker compose -f compose.dev.yml -f compose.dev.linux.yml --profile multinode down --remove-orphans >/dev/null 2>&1"
fi
grep -q '^Result: .* 0 failed' e2e-remote.log
