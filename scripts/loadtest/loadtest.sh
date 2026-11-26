#!/usr/bin/env bash
# Production capacity test: how much the current server and setup can take.
#
# Runs from a workstation (Linux, macOS or Git Bash on Windows) with Docker, ssh, curl and
# python3. Load comes from k6 in Docker and reaches the platform over HTTPS like real
# visitors: on this machine, or on another server with GEN_SSH=user@host (recommended: a
# home connection runs out of bandwidth long before the platform does). The platform
# server is reached over SSH only to create the test account and to sample its CPU,
# memory and network. It creates its own organization
# (loadtest<run>) on a temporary "loadtest" plan with no prices, and deletes both at the
# end, even when interrupted.
#
#   bash scripts/loadtest/loadtest.sh                    every phase (about 45 minutes)
#   PHASES="light cpu" bash scripts/loadtest/loadtest.sh just those phases
#
# Phases (each one runs until the platform breaks or its last step):
#   light    /       requests per second through the proxy and the sandbox, on LIGHT_INSTANCES
#                    fixed instances (proxy, TLS and gVisor overhead; the app does nothing)
#   cpu      /work   CPU-bound app autoscaling from 1 instance up to CPU_MAX
#   conns    /sleep  concurrent open connections, each request held 1 s
#   bw       /bytes  bandwidth: 1 MiB responses (limited by the generator's link too)
#   density  -       how many small services fit: deploys DENSITY_BATCH at a time until one
#                    fails, memory passes DENSITY_MEM_PCT % or DENSITY_MAX is reached; each
#                    one is checked from the proxy (no public certificate is requested)
#
# A step "holds" when 95% of requests answer under 500 ms with under 1% errors; the run
# stops a phase at the first step with over 10% errors or a p95 over 5 s.
#
# Settings (environment): SSH_TARGET (debian@37.59.106.54), SSH_KEY (~/.ssh/rabi-perso),
# PLATFORM_DIR (/opt/platform), GEN_SSH / GEN_KEY / GEN_MEMORY (k6's memory cap, 1500m)
# for a remote load generator, and the step lists LIGHT_STEPS, CPU_STEPS, CONN_STEPS,
# BW_STEPS with their HOLD times. Results: scripts/loadtest/results/<time>/ (report.md).
set -uo pipefail
D=$(cd "$(dirname "$0")" && pwd)
SSH_TARGET=${SSH_TARGET:-debian@37.59.106.54}
SSH_KEY=${SSH_KEY:-$HOME/.ssh/rabi-perso}
PLATFORM_DIR=${PLATFORM_DIR:-/opt/platform}
PHASES=${PHASES:-light cpu conns bw density}
LIGHT_INSTANCES=${LIGHT_INSTANCES:-6}
LIGHT_STEPS=${LIGHT_STEPS:-500,1000,2000,3000,4000,5000,6000,8000,10000,12000,15000}; LIGHT_HOLD=${LIGHT_HOLD:-30}
CPU_MAX=${CPU_MAX:-10}; CPU_MILLIS=${CPU_MILLIS:-1000}
CPU_STEPS=${CPU_STEPS:-50,100,200,300,400,500,600,800,1000,1200,1500,2000}; CPU_HOLD=${CPU_HOLD:-45}
CONN_STEPS=${CONN_STEPS:-250,500,1000,2000,3000,4000,6000}; CONN_HOLD=${CONN_HOLD:-30}
BW_STEPS=${BW_STEPS:-5,10,25,50,100,200}; BW_HOLD=${BW_HOLD:-20}
DENSITY_BATCH=${DENSITY_BATCH:-10}; DENSITY_MAX=${DENSITY_MAX:-600}; DENSITY_PER_PROJECT=${DENSITY_PER_PROJECT:-50}; DENSITY_MEM_PCT=${DENSITY_MEM_PCT:-90}
K6_IMAGE=grafana/k6:0.57.0
GEN_SSH=${GEN_SSH:-}; GEN_KEY=${GEN_KEY:-$SSH_KEY}; GEN_MEMORY=${GEN_MEMORY:-1500m}; GEN_DOCKER=${GEN_DOCKER:-sudo docker}

RUN=$(date +%Y%m%d-%H%M%S)
OUT=$D/results/$RUN; mkdir -p "$OUT"
TMP=$(mktemp -d)
ORG=loadtest$(date +%s | tail -c 6)
log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$OUT/run.log"; }
# Git Bash: Docker wants Windows paths, and must not rewrite /scripts or /out
winpath() { if command -v cygpath >/dev/null; then cygpath -w "$1"; else echo "$1"; fi; }


R() { ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ServerAliveInterval=15 "$SSH_TARGET" "$@"; }
PG=${PG_CONTAINER:-platform-postgres-1}; API=${API_CONTAINER:-platform-api-1}
owner_sql() { R "sudo docker exec -i $PG psql -qtA -U platform_owner platform" <<<"$1" | tr -d '\r'; }
admin() { R "sudo docker exec -i $API /platform admin $*" | tr -d '\r'; }
remote_env() { R "sudo grep -m1 '^$1=' $PLATFORM_DIR/deploy/.env | cut -d= -f2-" | tr -d '\r'; }

CONSOLE=$(remote_env CONSOLE_HOST); APPS=$(remote_env APPS_DOMAIN); REG=$(remote_env REGISTRY_HOST)
[ -n "$CONSOLE" ] && [ -n "$APPS" ] && [ -n "$REG" ] || { echo "cannot read CONSOLE_HOST, APPS_DOMAIN and REGISTRY_HOST over SSH"; exit 2; }
BASE=https://$CONSOLE
c() { local jar=$1; shift; curl -sS -b "$TMP/$jar" -c "$TMP/$jar" "$@"; }
code_of() { c "$1" -o /dev/null -w '%{http_code}' "${@:2}"; }
csrf_of() { grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"$//'; }

# ---------- clean-up (always) ----------
cleanup() {
  trap - EXIT INT TERM
  touch "$TMP/stop"; R "sudo touch /tmp/loadtest-stop" 2>/dev/null
  [ -f "$TMP/sampler.pid" ] && kill "$(cat "$TMP/sampler.pid")" 2>/dev/null
  sleep 3; R "sudo rm -f /tmp/loadtest-stop" 2>/dev/null
  log "clean-up: deleting organization $ORG and the loadtest plan"
  local csrf sel=() OID
  csrf=$(c j "$BASE/o/$ORG/compute" 2>/dev/null | csrf_of)
  OID=$(owner_sql "SELECT id FROM orgs WHERE slug = '$ORG'")
  if [ -n "$csrf" ]; then
    for row in $(owner_sql "SELECT p.slug || '/' || s.slug FROM services s JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$ORG' AND s.preview_of IS NULL"); do sel+=(-d "sel=$row"); done
    [ ${#sel[@]} -gt 0 ] && code_of j -X POST "$BASE/o/$ORG/compute/actions" -d csrf=$csrf -d action=delete -d confirm=delete "${sel[@]}" >/dev/null
    for _ in $(seq 1 90); do
      n=$(R "sudo docker ps -aq --filter label=platform.org=$OID | wc -l" | tr -d '\r ')
      [ "$n" = 0 ] && break; sleep 3
    done
  fi
  owner_sql "DELETE FROM orgs WHERE slug = '$ORG'; DELETE FROM users WHERE email = 'owner@$ORG.test'; DELETE FROM plans WHERE id = 'loadtest' AND NOT EXISTS (SELECT 1 FROM orgs WHERE plan_id = 'loadtest'); SELECT pg_notify('deployments', '')" >/dev/null
  [ -n "$GEN_SSH" ] && G "rm -rf $GDIR" 2>/dev/null
  rm -rf "$TMP"
  log "clean-up done"
}
trap cleanup EXIT
trap 'log "interrupted"; exit 130' INT TERM

# ---------- host sampler: one SSH session, a line every 2 s ----------
sampler() {
  echo "t,cpu_pct,mem_pct,load1,caddy_cpu_pct,caddy_mem_mb,containers,rx_mbps,tx_mbps" > "$OUT/host.csv"
  R "sudo bash -s" > >(tr -d '\r' >> "$OUT/host.csv") <<'REMOTE' &
cpu() { head -1 /proc/stat | awk '{print $2+$3+$4+$7+$8+$9, $5+$6}'; }
IF=$(ip route show default | awk '{print $5; exit}')
net() { awk -v i="$IF:" '$1==i{print $2, $10}' /proc/net/dev; }
CID=$(docker inspect -f '{{.Id}}' platform-caddy)
CG=$(ls -d /sys/fs/cgroup/system.slice/docker-$CID.scope 2>/dev/null || ls -d /sys/fs/cgroup/docker/$CID 2>/dev/null)
ccpu() { awk '/usage_usec/{print $2}' "$CG/cpu.stat" 2>/dev/null || echo 0; }
read pb pi < <(cpu); read prx ptx < <(net); pc=$(ccpu); pt=$(date +%s%N)
while sleep 2; do
  read cb ci < <(cpu); read rx tx < <(net); cc=$(ccpu); t=$(date +%s%N)
  dt=$(( (t-pt)/1000 )); db=$((cb-pb)); di=$((ci-pi))
  awk -v t=$t -v db=$db -v di=$di -v cc=$((cc-pc)) -v dt=$dt -v rx=$((rx-prx)) -v tx=$((tx-ptx)) \
      -v mem="$(free | awk '/Mem:/{printf "%.1f", 100*($2-$7)/$2}')" -v ld="$(cut -d' ' -f1 /proc/loadavg)" \
      -v n="$(docker ps -q | wc -l)" -v cores="$(nproc)" -v cm="$(cat "$CG/memory.current" 2>/dev/null || echo 0)" \
      'BEGIN{ printf "%.1f,%.1f,%s,%s,%.1f,%.0f,%d,%.1f,%.1f\n", t/1e9, (db+di>0?100*db/(db+di):0), mem, ld, 100*cc/dt/cores, cm/1048576, n, rx*8/dt, tx*8/dt }'
  pb=$cb; pi=$ci; prx=$rx; ptx=$tx; pc=$cc; pt=$t
  [ -f /tmp/loadtest-stop ] && break
done
REMOTE
  echo $! > "$TMP/sampler.pid"
}

# ---------- k6: one process per step (a process holds only that step's virtual users) ----------
G() { ssh -i "$GEN_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ServerAliveInterval=15 "$GEN_SSH" "$@"; }
GDIR=/tmp/loadtest-$RUN
if [ -n "$GEN_SSH" ]; then
  G "mkdir -p $GDIR/out && chmod 777 $GDIR/out && cat > $GDIR/load.js" < "$D/load.js" || { echo "cannot reach the load generator $GEN_SSH"; exit 2; }
  G "$GEN_DOCKER pull -q $K6_IMAGE" >/dev/null
fi
k6step() { # phase profile value hold [env...]: runs one step, records it in steps.idx
  local ph=$1 profile=$2 v=$3 hold=$4 name t0 args; shift 4
  name=$ph-$(printf %06d "$v")
  t0=$(R date +%s | tr -d '\r')   # the server's clock, like host.csv
  if [ -z "$GEN_SSH" ]; then
    MSYS_NO_PATHCONV=1 docker run --rm -v "$(winpath "$D"):/scripts:ro" -v "$(winpath "$OUT"):/out" \
      -e PROFILE="$profile" -e NAME="$name" -e STEPS="$v" -e HOLD="$hold" "$@" "$K6_IMAGE" run -q /scripts/load.js 2>&1 |
      grep -v -E '^\s*$|level=(warn|error)' | tail -2 >> "$OUT/run.log"
  else
    # same container on the generator, host network, memory capped so it cannot crowd out
    # what else runs there (k6 is killed instead, and the step counts as failed)
    args=$(printf '%q ' -e PROFILE="$profile" -e NAME="$name" -e STEPS="$v" -e HOLD="$hold" "$@")
    G "$GEN_DOCKER run --rm --network host --memory $GEN_MEMORY -v $GDIR:/scripts:ro -v $GDIR/out:/out $args $K6_IMAGE run -q /scripts/load.js; echo k6 exit code \$?" 2>&1 |
      tr -d '\r' | grep -v -E '^\s*$|level=(warn|error)' | tail -2 >> "$OUT/run.log"
    G "cat $GDIR/out/$name.summary.json 2>/dev/null" > "$OUT/$name.summary.json"
  fi
  echo "$ph,$v,$t0,$(R date +%s | tr -d '\r'),$hold" >> "$OUT/steps.idx"
}
verdict() { python3 "$D/report.py" --verdict "$@" 2>/dev/null || python "$D/report.py" --verdict "$@"; }
# phase <name> <profile> <steps> <hold> [env...]: steps in order until one breaks
phase() {
  local ph=$1 profile=$2 steps=$3 hold=$4 v res; shift 4
  for v in ${steps//,/ }; do
    k6step "$ph" "$profile" "$v" "$hold" "$@"
    res=$(verdict "$OUT/$ph-$(printf %06d "$v").summary.json" "$ph")
    log "  $ph $v: $res"
    [[ $res == broken* ]] && break
  done
}

# ---------- setup ----------
log "run $RUN: organization $ORG on $CONSOLE, phases: $PHASES"
command -v docker >/dev/null && docker info >/dev/null 2>&1 || { echo "Docker must be running"; exit 2; }
owner_sql "INSERT INTO plans SELECT (jsonb_populate_record(null::plans, to_jsonb(p) || jsonb_build_object(
  'id','loadtest','name','Load test (temporary)','max_services',1000,'max_instances',10,'max_cpu_millis',1000000,'max_memory_mb',1000000,
  'price_monthly_cents',0,'price_vcpu_hour_cents',0,'price_gb_hour_cents',0,'price_gb_out_cents',0))).*
  FROM plans p WHERE p.id = 'pro' ON CONFLICT (id) DO NOTHING" >/dev/null
link=$(admin create-org --slug "$ORG" --name Loadtest --owner-email "owner@$ORG.test" | tail -1)
pw="load-$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')"   # throwaway, never stored
code_of j -X POST "$BASE/invite/${link##*/invite/}" --data-urlencode "name=Load test" --data-urlencode "password=$pw" --data-urlencode "password2=$pw" >/dev/null
page=$(c j "$BASE/totp/setup")
secret=$(grep -o 'class="select">[A-Z2-7]\{20,\}<' <<<"$page" | head -1 | sed 's/.*">//;s/<$//')
c j -o /dev/null -X POST "$BASE/totp/setup" -d "csrf=$(csrf_of <<<"$page")" -d "code=$(admin totp-code --secret "$secret")"
admin set-plan --slug "$ORG" --plan loadtest >/dev/null
CSRF=$(c j "$BASE/o/$ORG" | csrf_of)
[ -n "$CSRF" ] || { log "could not sign in to the test organization"; exit 1; }
code_of j -X POST "$BASE/o/$ORG/projects" -d csrf=$CSRF -d slug=load -d name=load >/dev/null
TOK=$(c j -X POST "$BASE/o/$ORG/registry-tokens" -d "csrf=$CSRF" -d name=loadtest -d push=1 | grep -o 'plr_[A-Za-z0-9_-]*' | head -1)
IMG=$REG/$ORG/loadtest:1
mkdir -p "$TMP/docker"
docker build -q -t "$IMG" "$(winpath "$D/app")" >/dev/null || { log "image build failed"; exit 1; }
echo "$TOK" | DOCKER_CONFIG="$(winpath "$TMP/docker")" docker login "$REG" -u "$ORG" --password-stdin >/dev/null 2>&1
DOCKER_CONFIG="$(winpath "$TMP/docker")" docker push -q "$IMG" >/dev/null || { log "image push failed"; exit 1; }
log "image pushed: $IMG"

# read-only use of the session cookie: services are created in parallel (density phase)
cr() { curl -sS -b "$TMP/j" -o /dev/null -w '%{redirect_url}' "$@"; }
urlerr() { python3 -c 'import sys,urllib.parse as u;print(u.parse_qs(u.urlparse(sys.argv[1]).query).get("err",[""])[0])' "$1" 2>/dev/null ||
  python -c 'import sys,urllib.parse as u;print(u.parse_qs(u.urlparse(sys.argv[1]).query).get("err",[""])[0])' "$1"; }
# service <project> <slug> <cpu_millis> <memory_mb> <instances> [autoscale_max] [env]:
# create and deploy (no wait); a refused save is logged with the console's reason
service() {
  local pr=$1 slug=$2 cpu=$3 mem=$4 n=$5 amax=${6:-0} env=${7:-} back
  cr -X POST "$BASE/o/$ORG/p/$pr/services" -d csrf=$CSRF -d slug="$slug" -d image="$IMG" >/dev/null
  local as=(); [ "$amax" -gt 0 ] && as=(-d autoscale=1 -d autoscale_min=1 -d autoscale_max="$amax" -d autoscale_cpu=50)
  back=$(cr -X POST "$BASE/o/$ORG/p/$pr/s/$slug/settings" -d csrf=$CSRF -d image="$IMG" -d port=8080 -d health_path=%2F \
    -d cpu_millis="$cpu" -d memory_mb="$mem" -d instances="$n" -d volume_path= --data-urlencode "env=$env" "${as[@]}" -d deploy=1)
  [[ $back == *err=* ]] && log "settings of $pr/$slug refused: $(urlerr "$back")"
  return 0
}
status_in() { curl -sS -b "$TMP/j" "$BASE/o/$ORG/p/$1/s/$2/status.json" | grep -o '"Status":"[a-z]*"' | head -1 | cut -d'"' -f4; }
wait_in() { # project slug [tries]
  local s
  for _ in $(seq 1 "${3:-150}"); do
    s=$(status_in "$1" "$2"); { [ "$s" = live ] || [ "$s" = failed ]; } && { echo "$s"; return; }; sleep 2
  done
  echo "timeout (${s:-no deployment})"
}
drop() { # project slug...: delete services and wait until they are gone
  local pr=$1 sel=() s; shift
  for s in "$@"; do sel+=(-d "sel=$pr/$s"); done
  code_of j -X POST "$BASE/o/$ORG/compute/actions" -d csrf=$CSRF -d action=delete -d confirm=delete "${sel[@]}" >/dev/null
  for _ in $(seq 1 40); do
    [ "$(owner_sql "SELECT count(*) FROM services sv JOIN projects p ON p.id = sv.project_id JOIN orgs o ON o.id = sv.org_id WHERE o.slug = '$ORG' AND p.slug = '$pr'")" = 0 ] && break; sleep 3
  done
  sleep 10
}
host_of() { echo "$2-$1-$ORG.$APPS"; }   # project slug
ready() { # project slug: answers over HTTPS (the first request issues its certificate)
  for _ in $(seq 1 30); do [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "https://$(host_of "$1" "$2")/")" = 200 ] && return 0; sleep 2; done; return 1
}
project() { code_of j -X POST "$BASE/o/$ORG/projects" -d csrf=$CSRF -d slug="$1" -d name="$1" >/dev/null; }

rm -f "$TMP/stop"; R "sudo rm -f /tmp/loadtest-stop"
sampler
GEN_LABEL="this workstation"; [ -n "$GEN_SSH" ] && GEN_LABEL="server ${GEN_SSH#*@}"
printf 'generator=%s\nrun=%s\norg=%s\nconsole=%s\nphases=%s\nlight_instances=%s\ncpu_millis=%s\ncpu_max=%s\n' \
  "$GEN_LABEL" "$RUN" "$ORG" "$CONSOLE" "$PHASES" "$LIGHT_INSTANCES" "$CPU_MILLIS" "$CPU_MAX" > "$OUT/meta.txt"
R "nproc; free -m | awk '/Mem:/{print \$2}'; uname -r; sudo docker ps -q | wc -l" | tr -d '\r' | paste -sd, | sed 's/^/host=/' >> "$OUT/meta.txt"

want() { [[ " $PHASES " == *" $1 "* ]]; }

if want light || want conns || want bw; then
  log "deploying load/light ($LIGHT_INSTANCES instances x 1000m / 256 MB)"
  service load light 1000 256 "$LIGHT_INSTANCES"
  log "light: $(wait_in load light)"; ready load light || log "light does not answer over HTTPS"
  if want light; then
    log "phase light: / at $LIGHT_STEPS req/s, ${LIGHT_HOLD}s each"
    phase light break "$LIGHT_STEPS" "$LIGHT_HOLD" -e URL="https://$(host_of load light)/"
  fi
  if want conns; then
    log "phase conns: /sleep?ms=1000 with $CONN_STEPS open connections, ${CONN_HOLD}s each"
    phase conns conns "$CONN_STEPS" "$CONN_HOLD" -e URL="https://$(host_of load light)/sleep?ms=1000" -e TIMEOUT=15s
  fi
  if want bw; then
    log "phase bw: /bytes?kb=1024 at $BW_STEPS req/s, ${BW_HOLD}s each"
    phase bw break "$BW_STEPS" "$BW_HOLD" -e URL="https://$(host_of load light)/bytes?kb=1024" -e TIMEOUT=30s
  fi
  drop load light
fi

if want cpu; then
  log "deploying load/cpu (autoscaling 1..$CPU_MAX x ${CPU_MILLIS}m / 256 MB, target 50%)"
  service load cpu "$CPU_MILLIS" 256 1 "$CPU_MAX" "ROUNDS=4000"
  log "cpu: $(wait_in load cpu)"; ready load cpu || log "cpu does not answer over HTTPS"
  # instance count during the phase, for the report (server clock)
  ( while [ ! -f "$TMP/stop" ] && [ ! -f "$TMP/cpu-done" ]; do
      R "date +%s; sudo docker exec -i $PG psql -qtA -U platform_owner platform -c \"SELECT coalesce(max(jsonb_array_length(d.instances)),0) FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$ORG' AND s.slug = 'cpu' AND d.status = 'live'\"" |
        tr -d '\r' | paste -sd, >> "$OUT/cpu-instances.csv"
      sleep 5
    done ) &
  log "phase cpu: /work at $CPU_STEPS req/s, ${CPU_HOLD}s each"
  phase cpu break "$CPU_STEPS" "$CPU_HOLD" -e URL="https://$(host_of load cpu)/work"
  touch "$TMP/cpu-done"
  drop load cpu
fi

if want density; then
  # Services spread over projects of DENSITY_PER_PROJECT: each project has one network (a
  # /24, about 250 containers), so this measures the server, not one project's addresses.
  echo "services,deploy_s,live,serving,mem_pct,containers" > "$OUT/density.csv"
  log "phase density: $DENSITY_BATCH services at a time (100m / 64 MB each), $DENSITY_PER_PROJECT per project, up to $DENSITY_MAX"
  n=0; stop=
  while [ $n -lt "$DENSITY_MAX" ] && [ -z "$stop" ]; do
    t0=$(date +%s); batch=(); pids=()
    for _ in $(seq 1 "$DENSITY_BATCH"); do
      pr=dens$(( n / DENSITY_PER_PROJECT + 1 ))
      [ $(( n % DENSITY_PER_PROJECT )) = 0 ] && project "$pr"
      n=$((n+1)); s=d$(printf %04d $n); batch+=("$pr/$s")
      service "$pr" "$s" 100 64 1 & pids+=($!)
    done
    wait "${pids[@]}"
    failed=0
    for x in "${batch[@]}"; do st=$(wait_in "${x%/*}" "${x#*/}" 300); [ "$st" = live ] || { failed=$((failed+1)); log "  $x: $st"; }; done
    dt=$(( $(date +%s) - t0 ))
    live=$(owner_sql "SELECT count(*) FROM deployments d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$ORG' AND d.status = 'live'")
    # every live service answers through the proxy's network (no public certificate needed)
    serving=$(owner_sql "SELECT coalesce(nullif(d.upstream, ''), (d.instances->0->>'name') || ':8080') FROM deployments d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$ORG' AND d.status = 'live'" |
      R "sudo docker exec -i platform-caddy sh -c 'n=0; while read u; do wget -q -T 5 -O /dev/null \"http://\$u/\" && n=\$((n+1)); done; echo \$n'" | tr -d '\r')
    read -r mem ctr < <(R "free | awk '/Mem:/{printf \"%.1f \", 100*(\$2-\$7)/\$2}'; sudo docker ps -q | wc -l" | tr -d '\r' | paste -sd' ')
    echo "$n,$dt,$live,$serving,$mem,$ctr" >> "$OUT/density.csv"
    log "density: $n services, batch ${dt}s, live $live, serving $serving, memory $mem%"
    [ "$failed" -gt 0 ] && stop="a deployment failed or timed out ($failed in the last batch)"
    awk -v m="$mem" -v l="$DENSITY_MEM_PCT" 'BEGIN{exit !(m>=l)}' && stop="memory reached $mem%"
    [ "$dt" -gt 900 ] && stop="a batch took over 15 minutes"
  done
  log "density stopped: ${stop:-reached DENSITY_MAX}"
  echo "${stop:-reached DENSITY_MAX ($DENSITY_MAX)}" > "$OUT/density-stop.txt"
fi

R "sudo touch /tmp/loadtest-stop"; sleep 3; kill "$(cat "$TMP/sampler.pid")" 2>/dev/null; R "sudo rm -f /tmp/loadtest-stop"
python3 "$D/report.py" "$OUT" 2>/dev/null || python "$D/report.py" "$OUT"
log "report: $OUT/report.md"
