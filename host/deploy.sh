#!/usr/bin/env bash
# One-shot installer: turns a fresh Debian 13 server into a running platform.
#
#   curl -fsSLO https://raw.githubusercontent.com/gattoufirabi/cloud-experiment/main/host/deploy.sh
#   sudo bash deploy.sh --console console.example.com --apps apps.example-cloud.net \
#        --registry registry.example-cloud.net [--org acme --org-name "Acme Ltd" --owner-email you@example.com]
#
# Or from a checkout:  sudo bash /opt/platform/host/deploy.sh --console ... (same options)
#
# What it does, in order (safe to re-run: every step checks what is already there):
#   1. checks the OS, DNS records and options
#   2. updates the system, enables automatic security updates
#   3. host firewall (nftables, default deny: SSH, 80, 443, optional WireGuard)
#   4. SSH by key only (skipped if no authorized key exists, so you cannot lock yourself out)
#   5. Docker Engine + Compose plugin (Docker's official apt repository)
#   6. the code in /opt/platform (git clone, or the checkout this script runs from)
#   7. gVisor, the tenant firewall, then the Phase 0 isolation gate (stops on failure)
#   8. deploy/.env with generated passwords and secrets (kept if it already exists)
#   9. master key and registry signing key (never overwritten)
#  10. starts the stack and waits for the console to answer over HTTPS
#  11. optionally creates the first organization and prints its owner invite link
#
# Optional features (off-site backups, invoices, builds, self-signup) are set in
# /opt/platform/deploy/.env afterwards; see the deployment guide.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
# Pipelines below never stop reading early (no "grep -q", "head", "awk ... exit"):
# with pipefail, a writer killed by SIGPIPE would abort the whole script.

# ---------- options ----------
CONSOLE="" APPS="" REGISTRY="" BACKUP_URL=""
REPO="https://github.com/gattoufirabi/cloud-experiment.git" BRANCH="main" DIR="/opt/platform"
SSH_PORT="" WG_PORT="" ORG="" ORG_NAME="" OWNER_EMAIL="" PLAN="starter"
SKIP_HARDEN=0 SKIP_GATE=0 ASSUME_YES=0

usage() {
  sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'EOF'
Options:
  --console HOST        console address (DNS A/AAAA -> this server)            required
  --apps DOMAIN         base of client app addresses (DNS *.DOMAIN -> server)  required
  --registry HOST       image registry address (DNS -> this server)            required
  --backup-url URL      off-site restic rest-server, e.g. rest:http://platform:PW@10.8.0.3:8000
  --org SLUG --org-name NAME --owner-email EMAIL [--plan starter|pro]
                        create the first organization at the end
  --repo URL --branch B code to install (default: the project's GitHub repository, main)
  --dir PATH            install directory (default /opt/platform)
  --ssh-port N          SSH port to keep open (default: detected from sshd)
  --wireguard-port N    also open this UDP port for WireGuard
  --skip-harden         leave SSH and the host firewall as they are
  --skip-gate           continue even if the isolation test fails (NOT for production)
  --yes                 do not stop on DNS warnings
EOF
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --console) CONSOLE=$2; shift 2 ;;
    --apps) APPS=$2; shift 2 ;;
    --registry) REGISTRY=$2; shift 2 ;;
    --backup-url) BACKUP_URL=$2; shift 2 ;;
    --repo) REPO=$2; shift 2 ;;
    --branch) BRANCH=$2; shift 2 ;;
    --dir) DIR=$2; shift 2 ;;
    --ssh-port) SSH_PORT=$2; shift 2 ;;
    --wireguard-port) WG_PORT=$2; shift 2 ;;
    --org) ORG=$2; shift 2 ;;
    --org-name) ORG_NAME=$2; shift 2 ;;
    --owner-email) OWNER_EMAIL=$2; shift 2 ;;
    --plan) PLAN=$2; shift 2 ;;
    --skip-harden) SKIP_HARDEN=1; shift ;;
    --skip-gate) SKIP_GATE=1; shift ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    -h|--help) usage 0 ;;
    *) echo "Unknown option: $1" >&2; usage 2 ;;
  esac
done

# ---------- helpers ----------
step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[33m    WARNING: %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }
ask() { # ask VAR "question": prompt only when a terminal is attached
  local var=$1 q=$2 v
  if [[ -z ${!var} && -t 0 ]]; then read -rp "    $q: " v; printf -v "$var" '%s' "$v"; fi
}
valid_host() { [[ $1 =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$ ]]; }
setenv() { # setenv FILE KEY VALUE: replace or append KEY=VALUE without sed escaping issues
  local f=$1 k=$2 v=$3 tmp
  tmp=$(mktemp)
  K="$k" V="$v" awk 'BEGIN{k=ENVIRON["K"]; v=ENVIRON["V"]} index($0, k"=")==1 {print k"="v; f=1; next} {print} END{if(!f) print k"="v}' "$f" > "$tmp"
  cat "$tmp" > "$f"; rm -f "$tmp"
}
getenv() { K="$2" awk 'index($0, ENVIRON["K"]"=")==1 && !f {print substr($0, length(ENVIRON["K"])+2); f=1}' "$1"; }
secret() { openssl rand -hex 32; }

# ---------- 1. checks ----------
step "1/11 Checking the server and options"
[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 ...)"
. /etc/os-release
[[ ${ID:-} == debian ]] || die "Debian is required (found ${PRETTY_NAME:-unknown})"
[[ ${VERSION_ID:-0} -ge 12 ]] || die "Debian 12 or 13 is required (found $PRETTY_NAME)"
[[ ${VERSION_ID} -eq 13 ]] || warn "tested on Debian 13; found $PRETTY_NAME"
[[ $(dpkg --print-architecture) == amd64 ]] || warn "tested on amd64 only"

ask CONSOLE "Console address (e.g. console.example.com)"
ask APPS "Apps domain (e.g. apps.example-cloud.net)"
ask REGISTRY "Registry address (e.g. registry.example-cloud.net)"
CONSOLE=${CONSOLE,,} APPS=${APPS,,} REGISTRY=${REGISTRY,,}
for h in "$CONSOLE" "$APPS" "$REGISTRY"; do valid_host "$h" || die "not a valid host name: '$h' (see --help)"; done
if [[ -n $ORG || -n $OWNER_EMAIL ]]; then
  [[ $ORG =~ ^[a-z0-9][a-z0-9-]{1,19}$ ]] || die "--org: 2-20 lowercase letters, digits, dashes"
  [[ $OWNER_EMAIL == *@*.* ]] || die "--owner-email is required with --org"
  [[ -n $ORG_NAME ]] || ORG_NAME=$ORG
fi
MEM_GB=$(awk '/MemTotal/ {printf "%d", $2/1024/1024}' /proc/meminfo)
CPUS=$(nproc)
info "server: $CPUS vCPU, ${MEM_GB} GB RAM, $(df -h / | awk 'NR==2 {print $4}') free on /"
(( MEM_GB >= 7 )) || warn "less than 8 GB RAM: fine for a demo, not for clients"

apt-get update -qq
apt-get install -y -qq curl ca-certificates dnsutils openssl git >/dev/null
PUBLIC_IP=$(curl -fsS4 --max-time 10 https://ifconfig.me 2>/dev/null || ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
info "public IPv4: $PUBLIC_IP"
DNS_OK=1
for h in "$CONSOLE" "$REGISTRY" "dnscheck-$RANDOM.$APPS"; do
  got=$(dig +short A "$h" | awk 'END {print}')
  if [[ $got == "$PUBLIC_IP" ]]; then info "DNS ok: $h -> $got"; else warn "DNS: $h -> '${got:-nothing}', expected $PUBLIC_IP"; DNS_OK=0; fi
done
if (( ! DNS_OK )); then
  warn "HTTPS certificates need these records (the last one checks the *.$APPS wildcard)."
  (( ASSUME_YES )) || die "fix DNS and run again, or pass --yes to continue anyway"
fi

# ---------- 2. system updates ----------
step "2/11 Updating the system and enabling automatic security updates"
DEBIAN_FRONTEND=noninteractive apt-get -y -qq full-upgrade >/dev/null
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq unattended-upgrades nftables wireguard-tools gnupg >/dev/null
echo 'APT::Periodic::Update-Package-Lists "1"; APT::Periodic::Unattended-Upgrade "1";' > /etc/apt/apt.conf.d/20auto-upgrades
systemctl enable --now unattended-upgrades >/dev/null 2>&1 || true
info "done"

# ---------- 3. host firewall ----------
if [[ -z $SSH_PORT ]]; then SSH_PORT=$(sshd -T 2>/dev/null | awk '/^port / && !p {p=$2} END {print p}' || true); SSH_PORT=${SSH_PORT:-22}; fi
if (( SKIP_HARDEN )); then
  step "3/11 Host firewall: skipped (--skip-harden)"
else
  step "3/11 Host firewall (default deny; open: SSH $SSH_PORT, 80, 443${WG_PORT:+, WireGuard $WG_PORT/udp})"
  # Only our own table is replaced: Docker's rules (and the tenant firewall's) are left alone.
  cat > /etc/nftables.d-platform.nft <<EOF
table inet host_fw
delete table inet host_fw
table inet host_fw {
  chain input {
    type filter hook input priority 0; policy drop;
    ct state established,related accept
    ct state invalid drop
    iif lo accept
    meta l4proto { icmp, ipv6-icmp } accept
    tcp dport $SSH_PORT ct state new limit rate 30/minute accept
    tcp dport { 80, 443 } accept
    udp dport 443 accept
    ${WG_PORT:+udp dport $WG_PORT accept}
  }
}
EOF
  nft -f /etc/nftables.d-platform.nft
  grep -q 'nftables.d-platform.nft' /etc/nftables.conf 2>/dev/null || echo 'include "/etc/nftables.d-platform.nft"' >> /etc/nftables.conf
  # Never "flush ruleset" here: it would wipe Docker's NAT rules on every reload.
  sed -i 's/^flush ruleset/# flush ruleset (disabled: Docker manages its own rules)/' /etc/nftables.conf
  systemctl enable nftables >/dev/null 2>&1
  info "active; SSH stays open on port $SSH_PORT"
fi

# ---------- 4. SSH ----------
if (( SKIP_HARDEN )); then
  step "4/11 SSH: skipped (--skip-harden)"
else
  step "4/11 SSH: key login only"
  admin_user=${SUDO_USER:-root}
  keys="$(getent passwd "$admin_user" | cut -d: -f6)/.ssh/authorized_keys"
  if [[ -s $keys ]]; then
    cat > /etc/ssh/sshd_config.d/10-platform.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
EOF
    sshd -t && systemctl reload ssh
    info "password login disabled (key found for $admin_user). Test a new SSH session before closing this one."
  else
    warn "no SSH key in $keys: password login left ON so you are not locked out. Add a key and re-run."
  fi
fi

# ---------- 5. Docker ----------
step "5/11 Docker Engine and Compose"
if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian ${VERSION_CODENAME} stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
fi
systemctl enable --now docker >/dev/null
info "$(docker --version); $(docker compose version)"

# ---------- 6. code ----------
step "6/11 Platform code in $DIR"
SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." 2>/dev/null && pwd || true)
if [[ -f $DIR/deploy/compose.prod.yml ]]; then
  info "already installed; updating"
  git -C "$DIR" pull --ff-only || warn "git pull failed (local changes?); continuing with the current code"
elif [[ -n $SELF_DIR && -f $SELF_DIR/deploy/compose.prod.yml && $SELF_DIR != "$DIR" ]]; then
  info "copying the checkout this script runs from ($SELF_DIR)"
  mkdir -p "$DIR" && cp -a "$SELF_DIR/." "$DIR/"
elif [[ -n $SELF_DIR && $SELF_DIR == "$DIR" ]]; then
  info "running from $DIR"
else
  git clone --branch "$BRANCH" "$REPO" "$DIR" || die "could not clone $REPO (private repository? use a deploy key or copy the code to $DIR)"
fi
chmod +x "$DIR"/host/*.sh

# ---------- 7. isolation ----------
step "7/11 gVisor, tenant firewall, Phase 0 isolation gate"
if [[ $(docker info --format '{{json .Runtimes}}' 2>/dev/null) == *runsc* ]]; then
  info "gVisor already registered"
else
  bash "$DIR/host/install-gvisor.sh"
fi
install -m 0644 "$DIR/host/platform-tenant-firewall.service" /etc/systemd/system/platform-tenant-firewall.service
sed -i "s#/opt/platform/host/tenant-firewall.sh#$DIR/host/tenant-firewall.sh#" /etc/systemd/system/platform-tenant-firewall.service
systemctl daemon-reload
systemctl enable platform-tenant-firewall >/dev/null 2>&1
systemctl restart platform-tenant-firewall
GATE=$(bash "$DIR/host/isolation-test.sh" 2>&1 | tee /dev/stderr | tail -1)
if [[ $GATE == *"Phase 0 gate: PASSED"* ]]; then
  info "isolation gate passed"
elif (( SKIP_GATE )); then
  warn "isolation gate FAILED; continuing because of --skip-gate. Do not onboard clients."
else
  die "isolation gate failed: fix the failures above (or --skip-gate for a throwaway test server)"
fi

# ---------- 8. configuration ----------
step "8/11 Configuration (deploy/.env)"
ENVF="$DIR/deploy/.env"
if [[ -f $ENVF ]]; then
  info "keeping the existing $ENVF (only empty values are filled in)"
else
  cp "$DIR/deploy/.env.example" "$ENVF"
fi
chmod 600 "$ENVF"
setenv "$ENVF" CONSOLE_HOST "$CONSOLE"
setenv "$ENVF" APPS_DOMAIN "$APPS"
setenv "$ENVF" REGISTRY_HOST "$REGISTRY"
for k in OWNER_DB_PASSWORD API_DB_PASSWORD AGENT_DB_PASSWORD REGISTRY_INTERNAL_SECRET REGISTRY_EVENTS_SECRET TRIVY_TOKEN; do
  [[ -n $(getenv "$ENVF" "$k") ]] || setenv "$ENVF" "$k" "$(secret)"
done
[[ -z $BACKUP_URL ]] || setenv "$ENVF" BACKUP_REST_URL "$BACKUP_URL"
# Sandboxed (gVisor) containers cannot use Docker's embedded DNS on project networks:
# give them the host's upstream resolvers, public ones only (the tenant firewall blocks
# private ranges), falling back to Quad9.
mkdir -p /etc/platform
RESOLVERS=$(awk '/^nameserver/ {print $2}' /run/systemd/resolve/resolv.conf /etc/resolv.conf 2>/dev/null |
  awk '!seen[$0]++' | awk '!/^(127\.|10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|169\.254\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|::1|fe80:|fc|fd)/')
[[ -n $RESOLVERS ]] || RESOLVERS="9.9.9.9 149.112.112.112"
# shellcheck disable=SC2086 # one resolver per word
{ echo "# Resolvers for sandboxed tenant containers (written by host/deploy.sh)"; printf 'nameserver %s\n' $RESOLVERS; echo "options timeout:2 attempts:2"; } > /etc/platform/resolv.conf
chmod 644 /etc/platform/resolv.conf
setenv "$ENVF" TENANT_RESOLV_CONF /etc/platform/resolv.conf
info "tenant DNS: $(echo $RESOLVERS)"
[[ -n $(getenv "$ENVF" BACKUP_REST_URL) ]] || warn "BACKUP_REST_URL is empty: client backups stay on this server (not off-site)"
info "written with generated passwords (mode 600)"

# ---------- 9. keys ----------
step "9/11 Building images and creating the keys"
cd "$DIR/deploy"
mkdir -p /srv/platform/{postgres,caddy,vlogs,vmdata,registry,trivy} secrets
docker compose -f compose.prod.yml build
IMAGE="platform:$(getenv "$ENVF" PLATFORM_VERSION)"; [[ $IMAGE != "platform:" ]] || IMAGE=platform:local
if [[ ! -s secrets/master.key ]]; then
  docker run --rm --user 0 -v "$PWD/secrets:/out" "$IMAGE" admin init-key --file /out/master.key
fi
if [[ ! -s secrets/registry-token.key ]]; then
  docker run --rm --user 0 -v "$PWD/secrets:/out" "$IMAGE" admin init-registry-key --file /out/registry-token.key --cert /out/registry-token.crt
fi
chmod 444 secrets/master.key secrets/registry-token.key
chmod 700 secrets
info "keys in $DIR/deploy/secrets (never overwritten)"

# ---------- 10. start ----------
step "10/11 Starting the platform"
docker compose -f compose.prod.yml up -d
info "waiting for https://$CONSOLE (first start pulls images and gets certificates; up to 5 minutes)"
up=0
for _ in $(seq 1 60); do
  if curl -fsS -o /dev/null --max-time 10 "https://$CONSOLE/login"; then up=1; break; fi
  sleep 5
done
docker compose -f compose.prod.yml ps
if (( up )); then
  info "console answers over HTTPS with a valid certificate"
else
  warn "console not reachable over HTTPS yet. Check DNS and: docker compose -f compose.prod.yml logs caddy api"
fi

info "enabling the nightly database backup (/var/backups/platform, 14 days) and weekly registry clean-up"
for u in platform-db-backup.service platform-db-backup.timer platform-registry-gc.service platform-registry-gc.timer; do
  install -m 0644 "$DIR/host/$u" "/etc/systemd/system/$u"
  sed -i "s#/opt/platform/host/#$DIR/host/#" "/etc/systemd/system/$u"
done
systemctl daemon-reload
systemctl enable --now platform-db-backup.timer platform-registry-gc.timer >/dev/null 2>&1

# ---------- 11. first organization ----------
step "11/11 First organization"
if [[ -n $ORG ]]; then
  if [[ $(docker compose -f compose.prod.yml exec -T postgres psql -qtA -U platform_owner platform -c "SELECT 1 FROM orgs WHERE slug = '$ORG'") == 1 ]]; then
    info "organization $ORG already exists"
  else
    docker compose -f compose.prod.yml exec -T api /platform admin create-org --slug "$ORG" --name "$ORG_NAME" --plan "$PLAN" --owner-email "$OWNER_EMAIL"
  fi
else
  info "skipped. Later: docker compose -f compose.prod.yml exec api /platform admin create-org --slug acme --name \"Acme\" --owner-email you@example.com"
fi

cat <<EOF

$(printf '\033[1m')Done.$(printf '\033[0m') Console: https://$CONSOLE   Registry: $REGISTRY   Apps: *.$APPS

Now, before onboarding clients:
  1. Copy $DIR/deploy/.env and $DIR/deploy/secrets/ to an offline place.
     Without secrets/master.key, client secrets, two-step keys and backups cannot be decrypted.
  2. Do the checks in "Step 4: Verify before onboarding clients" of the deployment guide.
  3. Copy /var/backups/platform (nightly database dumps) off the server regularly.
  4. Optional features (off-site backups, invoices, builds, self-signup): edit deploy/.env,
     then: cd $DIR/deploy && docker compose -f compose.prod.yml up -d
EOF
