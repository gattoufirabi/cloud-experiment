#!/usr/bin/env bash
# End-to-end test against the local dev stack (deploy/compose.dev.yml must be running).
# Creates two tenants, deploys an app for each, then tries to cross between them.
# Usage: bash scripts/e2e-dev.sh
set -uo pipefail
cd "$(dirname "$0")/../deploy"

BASE=http://console.localhost:8088
PORT=8088
PASS=0; FAIL=0
ok()   { echo "  PASS  $*"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL  $*"; FAIL=$((FAIL+1)); }
check() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (expected $3, got $2)"; fi; }
# until_is <expected> <seconds> <command...>: runs the command every second until it prints
# <expected> (or the time is up) and prints its last output; replaces fixed sleeps
until_is() { local want=$1 n=$2 out=; shift 2; for _ in $(seq 1 "$n"); do out=$("$@"); [ "$out" = "$want" ] && break; sleep 1; done; echo "$out"; }
# section "name": prints how long the previous section took (to find the slow parts)
T0=$(date +%s); SEC_T=$T0; SEC_N=setup
section() { local now; now=$(date +%s); echo "   ($SEC_N: $((now - SEC_T)) s)"; SEC_N=$1; SEC_T=$now; echo "== $1"; }
# Container names, resolved once: "docker exec" is about twice as fast as "docker compose exec"
PG_C=$(docker compose -f compose.dev.yml ps --format '{{.Name}}' postgres); API_C=$(docker compose -f compose.dev.yml ps --format '{{.Name}}' api)
NTFY_C=$(docker compose -f compose.dev.yml ps --format '{{.Name}}' ntfy)
# MSYS_NO_PATHCONV: Git Bash on Windows would otherwise rewrite /platform into a Windows path
admin() { MSYS_NO_PATHCONV=1 docker exec -i "$API_C" /platform admin "$@"; }
live_cid() { docker exec -i "$PG_C" psql -qtA -U platform_owner platform -c "SELECT d.container_id FROM deployments d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$1' AND d.status = 'live'" | tr -d '\r'; }
psql_api() { docker exec -i "$PG_C" psql -qtA "postgres://platform_api:dev-api-password@localhost/platform" -c "$1"; }

RUN=$(date +%s | tail -c 5)
TMP=$(mktemp -d)

# curl with a per-tenant cookie jar; *.localhost forced to loopback
c() { local jar=$1; shift; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -b "$TMP/$jar" -c "$TMP/$jar" "$@"; }
csrf_of() { grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"$//'; }
code_of() { c "$1" -o /dev/null -w '%{http_code}' "${@:2}"; }

# onboard <jar> <org-slug> <email>: create org, accept invite, enrol TOTP; prints the TOTP secret
onboard() {
  local jar=$1 org=$2 email=$3
  local link token page secret csrf code
  link=$(admin create-org --slug "$org" --name "${org^}" --owner-email "$email" | tail -1 | tr -d '\r')
  token=${link##*/invite/}
  code=$(code_of "$jar" -X POST "$BASE/invite/$token" --data-urlencode "name=Owner of $org" \
        --data-urlencode "password=correct-horse-battery-$org" --data-urlencode "password2=correct-horse-battery-$org")
  check "[$org] invite accepted" "$code" 303
  page=$(c "$jar" "$BASE/totp/setup")
  if echo "$page" | grep -q '<svg class="qr"'; then ok "[$org] two-step setup shows a QR code"; else bad "[$org] no QR code on the setup page"; fi
  secret=$(echo "$page" | grep -o 'class="select">[A-Z2-7]\{20,\}<' | head -1 | sed 's/.*">//;s/<$//')
  csrf=$(echo "$page" | csrf_of)
  page=$(c "$jar" -X POST "$BASE/totp/setup" -d "csrf=$csrf" -d "code=$(admin totp-code --secret "$secret" | tr -d '\r')")
  if echo "$page" | grep -q "Save your recovery codes"; then ok "[$org] two-step login enrolled, recovery codes shown"; else bad "[$org] two-step enrolment failed"; fi
  echo "$page" | grep -o 'class="select">[a-z2-7]\{5\}-[a-z2-7]\{5\}<' | sed 's/.*">//;s/<$//' > "$TMP/rc-$org"
  echo "$secret" > "$TMP/totp-$org"
}

wait_status() { # wait_status <jar> <service path> -> prints final status
  local s
  for _ in $(seq 1 60); do
    s=$(c "$1" "$BASE$2/status.json" | grep -o '"Status":"[a-z]*"' | head -1 | cut -d'"' -f4)
    if [ "$s" = live ] || [ "$s" = failed ]; then echo "$s"; return; fi
    sleep 2
  done
  echo "timeout"
}

A=a$RUN; B=b$RUN
section "Tenant A ($A) and tenant B ($B)"
onboard ja "$A" "owner@$A.test"
onboard jb "$B" "owner@$B.test"

section "Unauthenticated access"
check "console redirects anonymous users to login" "$(curl -sS --resolve console.localhost:$PORT:127.0.0.1 -o /dev/null -w '%{http_code}' $BASE/o/$A)" 303

section "Tenant A deploys nginx with a secret"
CSRF_A=$(c ja "$BASE/o/$A" | csrf_of)
check "[A] project created" "$(code_of ja -X POST "$BASE/o/$A/projects" -d csrf=$CSRF_A -d slug=shop -d name=Shop)" 303
check "[A] service created" "$(code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=web -d image=nginx:1.27-alpine)" 303
SVC_A=/o/$A/p/shop/s/web
check "[A] secret saved" "$(code_of ja -X POST "$BASE$SVC_A/secrets" -d csrf=$CSRF_A -d name=API_KEY -d value=tenant-a-secret)" 303
check "[A] settings saved + deploy" "$(code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine \
  -d port=80 -d health_path=%2F -d cpu_millis=250 -d memory_mb=128 -d volume_path= --data-urlencode 'env=GREETING=hello' -d deploy=1)" 303
check "[A] deployment reaches live" "$(wait_status ja $SVC_A)" live
HOST_A=web-shop-$A.apps.localhost
BODY=$(curl -sS --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/")
if echo "$BODY" | grep -q "Welcome to nginx"; then ok "[A] app answers at http://$HOST_A:$PORT"; else bad "[A] app not reachable"; fi

CID_A=$(live_cid "$A")
check "[A] secret injected into container" "$(docker exec "$CID_A" printenv API_KEY | tr -d '\r')" "tenant-a-secret"
check "[A] container: all capabilities dropped first" "$(docker inspect -f '{{json .HostConfig.CapDrop}}' "$CID_A")" '["ALL"]'
check "[A] container: not privileged" "$(docker inspect -f '{{.HostConfig.Privileged}}' "$CID_A")" false
check "[A] container: memory limit 128 MB" "$(docker inspect -f '{{.HostConfig.Memory}}' "$CID_A")" 134217728
check "[A] container: CPU limit 0.25 core" "$(docker inspect -f '{{.HostConfig.NanoCpus}}' "$CID_A")" 250000000
check "[A] container: PID limit" "$(docker inspect -f '{{.HostConfig.PidsLimit}}' "$CID_A")" 256
check "[A] container: no-new-privileges" "$(docker inspect -f '{{json .HostConfig.SecurityOpt}}' "$CID_A")" '["no-new-privileges:true"]'

section "Tenant B deploys its own app"
CSRF_B=$(c jb "$BASE/o/$B" | csrf_of)
code_of jb -X POST "$BASE/o/$B/projects" -d csrf=$CSRF_B -d slug=shop -d name=Shop >/dev/null
code_of jb -X POST "$BASE/o/$B/p/shop/services" -d csrf=$CSRF_B -d slug=web -d image=nginx:1.27-alpine >/dev/null
SVC_B=/o/$B/p/shop/s/web
code_of jb -X POST "$BASE$SVC_B/settings" -d csrf=$CSRF_B -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[B] deployment reaches live" "$(wait_status jb $SVC_B)" live
CID_B=$(live_cid "$B")

section "Isolation: B tries to reach A"
check "[B->A] organization page hidden (404)" "$(code_of jb "$BASE/o/$A")" 404
check "[B->A] service page hidden (404)" "$(code_of jb "$BASE$SVC_A")" 404
check "[B->A] logs API hidden (404)" "$(code_of jb "$BASE$SVC_A/logs.json")" 404
check "[B->A] deploy with own valid CSRF refused (404)" "$(code_of jb -X POST "$BASE$SVC_A/deploy" -d csrf=$CSRF_B)" 404
check "[B] request without CSRF token refused (403)" "$(code_of jb -X POST "$BASE$SVC_B/deploy")" 403
IP_A=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$CID_A" | awk '{print $1}')
if [ -z "$IP_A" ] || [ -z "$CID_B" ]; then
  bad "[B->A] network test could not run (missing container or IP)"
elif docker exec "$CID_B" wget -q -T 3 -O /dev/null "http://$IP_A:80/" 2>/dev/null; then
  bad "[B->A] B's container reached A's container directly at $IP_A"
else
  ok "[B->A] B's container cannot reach A's container ($IP_A) over the network"
fi
check "[DB] API role sees no tenant rows without a tenant context (RLS)" "$(psql_api 'SELECT count(*) FROM services' | tr -d '\r')" 0
check "[DB] API role cannot read secrets across tenants" "$(psql_api 'SELECT count(*) FROM secrets' | tr -d '\r')" 0

section "Failed deploy keeps the old version serving (A)"
code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=9999 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[A] broken deployment marked failed" "$(wait_status ja $SVC_A)" failed
BODY=$(curl -sS --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/")
if echo "$BODY" | grep -q "Welcome to nginx"; then ok "[A] previous version still serving"; else bad "[A] app down after failed deploy"; fi

section "Logs and audit"
for _ in 1 2 3; do curl -sS --resolve "$HOST_A:$PORT:127.0.0.1" -o /dev/null "http://$HOST_A:$PORT/?probe=$RUN"; done
for _ in $(seq 1 20); do grep -q "probe=$RUN" <<<"$(c ja "$BASE$SVC_A/logs.json?q=probe=$RUN")" && break; sleep 1; done
if c ja "$BASE$SVC_A/logs.json?q=probe=$RUN" | grep -q "probe=$RUN"; then ok "[A] request logs searchable in the console"; else bad "[A] logs not found"; fi
if c ja "$BASE/o/$A/audit" | grep -q "deployment.live"; then ok "[A] audit log records deployments"; else bad "[A] audit log missing events"; fi

section "Private registry and vulnerability scanning"
REG=localhost:8088
mkdir -p "$TMP/docker"
DCFG=$(cygpath -w "$TMP/docker" 2>/dev/null || echo "$TMP/docker")
dk() { DOCKER_CONFIG="$DCFG" docker "$@" >/dev/null 2>&1 && echo yes || echo no; } # isolated docker login state
owner_sql() { docker exec -i "$PG_C" psql -qtA -U platform_owner platform -c "$1" | tr -d '\r'; }
new_token() { c "$1" -X POST "$BASE/o/$2/registry-tokens" -d "csrf=$3" -d name=ci -d push=1 | grep -o 'plr_[A-Za-z0-9_-]*' | head -1; }
TOK_A=$(new_token ja "$A" "$CSRF_A")
TOK_B=$(new_token jb "$B" "$CSRF_B")
if [ -n "$TOK_A" ] && [ -n "$TOK_B" ]; then ok "registry tokens created in the console"; else bad "registry token not shown"; fi
check "[A] docker login refused with a wrong token" "$(echo plr_wrong | dk login $REG -u "$A" --password-stdin)" no
check "[A] docker login with its token" "$(echo "$TOK_A" | dk login $REG -u "$A" --password-stdin)" yes
docker tag nginx:1.27-alpine "$REG/$A/web:v1"
docker tag nginx:1.27-alpine "$REG/$B/intrude:v1"
check "[A] push to its own namespace" "$(dk push "$REG/$A/web:v1")" yes
check "[A->B] push into B's namespace refused" "$(dk push "$REG/$B/intrude:v1")" no
check "[B] docker login with its token" "$(echo "$TOK_B" | dk login $REG -u "$B" --password-stdin)" yes
check "[B->A] pull of A's private image refused" "$(dk pull "$REG/$A/web:v1")" no
LOC=$(c jb -o /dev/null -D - -X POST "$BASE$SVC_B/settings" -d csrf=$CSRF_B -d "image=$REG/$A/web:v1" -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 | tr -d '\r' | grep -i '^location')
if echo "$LOC" | grep -q "must+be+under"; then ok "[B->A] deploying A's registry image refused by the console"; else bad "[B->A] console accepted A's image ($LOC)"; fi

code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d "image=$REG/$A/web:v1" -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[A] deploy from its private registry reaches live" "$(wait_status ja $SVC_A)" live
if c ja "$BASE$SVC_A" | grep -q "Vulnerability scan"; then ok "[A] scan report shown on the service page"; else bad "[A] no scan report on the service page"; fi
SCAN_ST=""
for _ in $(seq 1 60); do
  SCAN_ST=$(owner_sql "SELECT s.status FROM image_scans s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.source = 'push' ORDER BY s.created_at DESC LIMIT 1")
  [ "$SCAN_ST" = done ] || [ "$SCAN_ST" = error ] && break
  sleep 3
done
check "[A] pushed image scanned automatically" "$SCAN_ST" done
CRIT=$(owner_sql "SELECT critical FROM image_scans s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.status = 'done' ORDER BY s.scanned_at DESC LIMIT 1")
echo "  INFO  nginx:1.27-alpine has $CRIT critical findings in the current vulnerability database"

admin set-scan-policy --slug "$A" --policy block-critical >/dev/null
code_of ja -X POST "$BASE$SVC_A/deploy" -d csrf=$CSRF_A >/dev/null
if [ "${CRIT:-0}" -gt 0 ]; then
  check "[A] block-critical policy refuses the image" "$(wait_status ja $SVC_A)" failed
  if c ja "$BASE$SVC_A/status.json" | grep -q "blocked by security policy"; then ok "[A] client sees why it was blocked"; else bad "[A] block reason missing"; fi
else
  check "[A] block-critical policy lets a clean image through" "$(wait_status ja $SVC_A)" live
fi
admin set-scan-policy --slug "$A" --policy warn >/dev/null
docker rmi "$REG/$A/web:v1" "$REG/$B/intrude:v1" >/dev/null 2>&1

section "Per-client backups"
dx() { MSYS_NO_PATHCONV=1 docker "$@"; }
wait_backup() {
  local s
  for _ in $(seq 1 90); do
    s=$(owner_sql "SELECT j.status FROM backup_jobs j JOIN services s ON s.id = j.service_id JOIN orgs o ON o.id = j.org_id
                   WHERE o.slug = '$A' AND s.slug = 'files' ORDER BY j.created_at DESC LIMIT 1")
    if [ "$s" = done ] || [ "$s" = error ]; then echo "$s"; return; fi
    sleep 2
  done
  echo timeout
}
code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=files -d image=nginx:1.27-alpine >/dev/null
SVC_F=/o/$A/p/shop/s/files
code_of ja -X POST "$BASE$SVC_F/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path=%2Fdata -d env= -d deploy=1 >/dev/null
check "[A] service with a persistent folder reaches live" "$(wait_status ja $SVC_F)" live
CID_F=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id
                   WHERE o.slug = '$A' AND s.slug = 'files' AND d.status = 'live'")
MARK=backup-marker-$RUN
dx exec "$CID_F" sh -c "echo $MARK-v1 > /data/state.txt"
check "[A] backup requested from the console" "$(code_of ja -X POST "$BASE$SVC_F/backups" -d csrf=$CSRF_A)" 303
check "[A] backup completes" "$(wait_backup)" done
SNAP=$(owner_sql "SELECT b.snapshot_id FROM backup_snapshots b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = b.org_id
                  WHERE o.slug = '$A' AND s.slug = 'files' ORDER BY b.taken_at DESC LIMIT 1")
if [ -n "$SNAP" ] && c ja "$BASE$SVC_F" | grep -q "${SNAP:0:8}"; then ok "[A] snapshot listed in the console"; else bad "[A] snapshot not listed"; fi
ORG_A_ID=$(owner_sql "SELECT id FROM orgs WHERE slug = '$A'")
if dx run --rm -v platform-backups:/r:ro alpine:3.20 test -d "/r/$ORG_A_ID"; then ok "[A] stored in its own repository"; else bad "[A] repository not found"; fi
if dx run --rm -v platform-backups:/r:ro alpine:3.20 grep -rqs "$MARK" /r; then bad "plaintext data found in the backup repository"; else ok "backup repository content is encrypted"; fi

dx exec "$CID_F" sh -c "echo $MARK-v2 > /data/state.txt"
check "[B->A] restore of A's snapshot refused (404)" "$(code_of jb -X POST "$BASE$SVC_F/backups/restore/$SNAP" -d csrf=$CSRF_B -d confirm=files)" 404
check "[B->A] backup of A's service refused (404)" "$(code_of jb -X POST "$BASE$SVC_F/backups" -d csrf=$CSRF_B)" 404
check "[A] restore without typed confirmation refused" "$(c ja -o /dev/null -D - -X POST "$BASE$SVC_F/backups/restore/$SNAP" -d csrf=$CSRF_A -d confirm=nope | tr -d '\r' | grep -ci 'location:.*err=')" 1
check "[A] restore requested" "$(code_of ja -X POST "$BASE$SVC_F/backups/restore/$SNAP" -d csrf=$CSRF_A -d confirm=files)" 303
check "[A] restore completes" "$(wait_backup)" done
check "[A] data is back to the snapshot" "$(dx exec "$CID_F" cat /data/state.txt | tr -d '\r')" "$MARK-v1"
check "[A] application running again after restore" "$(docker inspect -f '{{.State.Running}}' "$CID_F")" true
check "[A] safety snapshot kept before the restore" "$(owner_sql "SELECT count(*) > 0 FROM backup_snapshots b JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND b.tags = 'pre-restore'")" t

section "Managed PostgreSQL"
db_sql() { dx exec -e PGPASSWORD=x "$DB_CID" psql -U app -d app -h 127.0.0.1 -tA -c "$1" 2>&1 | tr -d '\r'; }
wait_dbjob() {
  local s
  for _ in $(seq 1 90); do
    s=$(owner_sql "SELECT j.status FROM backup_jobs j JOIN databases d ON d.id = j.database_id JOIN orgs o ON o.id = j.org_id
                   WHERE o.slug = '$A' AND d.slug = 'main' ORDER BY j.created_at DESC LIMIT 1")
    if [ "$s" = done ] || [ "$s" = error ]; then echo "$s"; return; fi
    sleep 2
  done
  echo timeout
}
check "[A] database created" "$(code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=main -d version=17)" 303
DB_ST=""; for _ in $(seq 1 60); do
  DB_ST=$(owner_sql "SELECT d.status FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'main'")
  [ "$DB_ST" = running ] || [ "$DB_ST" = failed ] && break; sleep 2; done
check "[A] database running" "$DB_ST" running
DB_CID=$(owner_sql "SELECT d.container_id FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'main'")
DB_HOST=$(owner_sql "SELECT 'pd-' || left(replace(d.id::text, '-', ''), 12) FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'main'")
check "[A] database runs with its memory limit" "$(docker inspect -f '{{.HostConfig.Memory}}' "$DB_CID")" 268435456
if dx exec "$CID_F" sh -c "echo | nc -w 3 $DB_HOST 5432" >/dev/null 2>&1; then ok "[A] service in the same project reaches the database"; else bad "[A] service cannot reach its database"; fi
if dx exec "$CID_B" sh -c "echo | nc -w 3 $DB_HOST 5432" >/dev/null 2>&1; then bad "[B->A] B's service reached A's database"; else ok "[B->A] B's service cannot reach A's database"; fi
check "[A] connection URL added to a service as a secret" "$(code_of ja -X POST "$BASE/o/$A/p/shop/d/main/attach" -d csrf=$CSRF_A -d service=files -d name=DATABASE_URL)" 303
check "[A] DATABASE_URL secret stored" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND v.slug = 'files' AND s.name = 'DATABASE_URL'")" 1
check "[B->A] B cannot open A's database page (404)" "$(code_of jb "$BASE/o/$A/p/shop/d/main")" 404
LOC=$(c ja -o /dev/null -D - -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=second | tr -d '\r' | grep -i '^location')
if echo "$LOC" | grep -q "allows+1+managed"; then ok "[A] plan limit on databases enforced"; else bad "[A] second database not refused ($LOC)"; fi

db_sql "CREATE TABLE notes (t text); INSERT INTO notes VALUES ('$MARK-db1')" >/dev/null
check "[A] database backup requested" "$(code_of ja -X POST "$BASE/o/$A/p/shop/d/main/backups" -d csrf=$CSRF_A)" 303
check "[A] database backup completes" "$(wait_dbjob)" done
DB_SNAP=$(owner_sql "SELECT b.snapshot_id FROM backup_snapshots b JOIN databases d ON d.id = b.database_id JOIN orgs o ON o.id = b.org_id
                     WHERE o.slug = '$A' AND d.slug = 'main' ORDER BY b.taken_at DESC LIMIT 1")
db_sql "DELETE FROM notes; INSERT INTO notes VALUES ('$MARK-db2')" >/dev/null
check "[B->A] B cannot restore A's database (404)" "$(code_of jb -X POST "$BASE/o/$A/p/shop/d/main/backups/restore/$DB_SNAP" -d csrf=$CSRF_B -d confirm=main)" 404
check "[A] database restore requested" "$(code_of ja -X POST "$BASE/o/$A/p/shop/d/main/backups/restore/$DB_SNAP" -d csrf=$CSRF_A -d confirm=main)" 303
check "[A] database page renders once backups exist" "$(code_of ja "$BASE/o/$A/p/shop/d/main")" 200
check "[A] database restore completes" "$(wait_dbjob)" done
check "[A] database rows are back to the snapshot" "$(db_sql "SELECT t FROM notes")" "$MARK-db1"
check "[A] database deleted" "$(code_of ja -X POST "$BASE/o/$A/p/shop/d/main/delete" -d csrf=$CSRF_A -d confirm=main)" 303
GONE=no; for _ in $(seq 1 30); do docker inspect "$DB_CID" >/dev/null 2>&1 || { GONE=yes; break; }; sleep 2; done
check "[A] database container removed" "$GONE" yes

section "Managed Redis"
rd_cli() { dx exec "$RD_CID" sh -c "REDISCLI_AUTH=\"\$REDIS_PASSWORD\" redis-cli $1" 2>&1 | tr -d '\r'; }
check "[A] Redis created" "$(code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=cache -d engine=redis:7)" 303
RD_ST=""; for _ in $(seq 1 60); do
  RD_ST=$(owner_sql "SELECT d.status FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'cache'")
  [ "$RD_ST" = running ] || [ "$RD_ST" = failed ] && break; sleep 2; done
check "[A] Redis running" "$RD_ST" running
RD_CID=$(owner_sql "SELECT d.container_id FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'cache'")
RD_HOST=$(owner_sql "SELECT 'pd-' || left(replace(d.id::text, '-', ''), 12) FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'cache'")
check "[A] Redis requires its password" "$(dx exec "$RD_CID" redis-cli ping 2>&1 | tr -d '\r' | grep -c NOAUTH)" 1
if dx exec "$CID_F" sh -c "echo | nc -w 3 $RD_HOST 6379" >/dev/null 2>&1; then ok "[A] service in the same project reaches Redis"; else bad "[A] service cannot reach Redis"; fi
if dx exec "$CID_B" sh -c "echo | nc -w 3 $RD_HOST 6379" >/dev/null 2>&1; then bad "[B->A] B's service reached A's Redis"; else ok "[B->A] B's service cannot reach A's Redis"; fi
code_of ja -X POST "$BASE/o/$A/p/shop/d/cache/attach" -d csrf=$CSRF_A -d service=files >/dev/null
check "[A] REDIS_URL secret stored by default" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND v.slug = 'files' AND s.name = 'REDIS_URL'")" 1
rd_cli "SET marker $MARK-r1" >/dev/null
check "[A] Redis backup completes" "$(code_of ja -X POST "$BASE/o/$A/p/shop/d/cache/backups" -d csrf=$CSRF_A >/dev/null; s=; for _ in $(seq 1 60); do s=$(owner_sql "SELECT j.status FROM backup_jobs j JOIN databases d ON d.id = j.database_id JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND d.slug = 'cache' ORDER BY j.created_at DESC LIMIT 1"); [ "$s" = done ] || [ "$s" = error ] && break; sleep 2; done; echo "$s")" done
RD_SNAP=$(owner_sql "SELECT b.snapshot_id FROM backup_snapshots b JOIN databases d ON d.id = b.database_id JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND d.slug = 'cache' ORDER BY b.taken_at DESC LIMIT 1")
rd_cli "SET marker $MARK-r2" >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/d/cache/backups/restore/$RD_SNAP" -d csrf=$CSRF_A -d confirm=cache >/dev/null
RS=""; for _ in $(seq 1 60); do RS=$(owner_sql "SELECT j.status FROM backup_jobs j JOIN databases d ON d.id = j.database_id JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND d.slug = 'cache' AND j.kind = 'restore' ORDER BY j.created_at DESC LIMIT 1"); [ "$RS" = done ] || [ "$RS" = error ] && break; sleep 2; done
check "[A] Redis restore completes" "$RS" done
check "[A] Redis key is back to the snapshot" "$(rd_cli "GET marker")" "$MARK-r1"
code_of ja -X POST "$BASE/o/$A/p/shop/d/cache/delete" -d csrf=$CSRF_A -d confirm=cache >/dev/null

section "Managed MySQL and MariaDB"
my_db() { owner_sql "SELECT d.$2 FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = '$1'"; }
my_job() { owner_sql "SELECT j.status FROM backup_jobs j JOIN databases d ON d.id = j.database_id JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND d.slug = '$1' AND j.kind = '$2' ORDER BY j.created_at DESC LIMIT 1"; }
my_wait_job() { local st=; for _ in $(seq 1 90); do st=$(my_job "$1" "$2"); [ "$st" = done ] || [ "$st" = error ] && break; sleep 2; done; echo "$st"; }
mysql_engine() { # mysql_engine <slug> <engine:version> <client> <label>
  local slug=$1 choice=$2 cli=$3 label=$4 st= cid host
  check "[A] $label created" "$(code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=$slug -d engine=$choice)" 303
  for _ in $(seq 1 120); do st=$(my_db $slug status); [ "$st" = running ] || [ "$st" = failed ] && break; sleep 2; done
  check "[A] $label running" "$st" running
  cid=$(my_db $slug container_id)
  host=$(owner_sql "SELECT 'pd-' || left(replace(d.id::text, '-', ''), 12) FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = '$slug'")
  my_sql() { dx exec "$cid" sh -c "MYSQL_PWD=\"\${MYSQL_PASSWORD:-\$MARIADB_PASSWORD}\" $cli -h 127.0.0.1 -u app app -N -e \"$1\"" 2>&1 | tr -d '\r'; }
  check "[A] $label refuses a wrong password" "$(dx exec "$cid" sh -c "MYSQL_PWD=wrong $cli -h 127.0.0.1 -u app app -e 'SELECT 1'" >/dev/null 2>&1 && echo yes || echo no)" no
  check "[A] $label root cannot log in with the app password" "$(dx exec "$cid" sh -c "MYSQL_PWD=\"\${MYSQL_PASSWORD:-\$MARIADB_PASSWORD}\" $cli -h 127.0.0.1 -u root -e 'SELECT 1'" >/dev/null 2>&1 && echo yes || echo no)" no
  if dx exec "$CID_F" sh -c "echo | nc -w 3 $host 3306" >/dev/null 2>&1; then ok "[A] service in the same project reaches $label"; else bad "[A] service cannot reach $label"; fi
  if dx exec "$CID_B" sh -c "echo | nc -w 3 $host 3306" >/dev/null 2>&1; then bad "[B->A] B's service reached A's $label"; else ok "[B->A] B's service cannot reach A's $label"; fi
  code_of ja -X POST "$BASE/o/$A/p/shop/d/$slug/attach" -d csrf=$CSRF_A -d service=files -d name=MY_${slug^^}_URL >/dev/null
  check "[A] $label connection URL stored as a secret" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND v.slug = 'files' AND s.name = 'MY_${slug^^}_URL'")" 1
  if grep -q "mysql://app:" <<<"$(c ja "$BASE/o/$A/p/shop/d/$slug")"; then ok "[A] $label page shows a mysql:// URL"; else bad "$label page lacks the connection URL"; fi
  my_sql "CREATE TABLE notes (t varchar(100)); INSERT INTO notes VALUES ('$MARK-m1')" >/dev/null
  check "[A] $label stores rows" "$(my_sql "SELECT t FROM notes")" "$MARK-m1"
  code_of ja -X POST "$BASE/o/$A/p/shop/d/$slug/backups" -d csrf=$CSRF_A >/dev/null
  check "[A] $label backup completes" "$(my_wait_job $slug backup)" done
  local snap; snap=$(owner_sql "SELECT b.snapshot_id FROM backup_snapshots b JOIN databases d ON d.id = b.database_id JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND d.slug = '$slug' ORDER BY b.taken_at DESC LIMIT 1")
  my_sql "UPDATE notes SET t = '$MARK-m2'" >/dev/null
  code_of ja -X POST "$BASE/o/$A/p/shop/d/$slug/backups/restore/$snap" -d csrf=$CSRF_A -d confirm=$slug >/dev/null
  check "[A] $label restore completes" "$(my_wait_job $slug restore)" done
  check "[A] $label rows are back to the snapshot" "$(my_sql "SELECT t FROM notes")" "$MARK-m1"
  code_of ja -X POST "$BASE/o/$A/p/shop/d/$slug/settings" -d csrf=$CSRF_A -d cpu_millis=250 -d memory_mb=384 -d backup_hour=3 -d backup_enabled=1 >/dev/null
  st=; for _ in $(seq 1 120); do [ "$(my_db $slug container_id)" != "$cid" ] && [ "$(my_db $slug status)" = running ] && { st=yes; break; }; sleep 2; done
  check "[A] $label recreated with more memory" "${st:-no}" yes
  cid=$(my_db $slug container_id)
  check "[A] $label data kept across the settings change" "$(my_sql "SELECT t FROM notes")" "$MARK-m1"
  code_of ja -X POST "$BASE/o/$A/p/shop/d/$slug/delete" -d csrf=$CSRF_A -d confirm=$slug >/dev/null
  code_of ja -X POST "$BASE/o/$A/p/shop/s/files/secrets/MY_${slug^^}_URL/delete" -d csrf=$CSRF_A >/dev/null
}
admin set-plan --slug "$A" --plan pro >/dev/null   # two databases at once
# MySQL initialises while the MariaDB checks run (its own create check then finds it existing)
code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=mysql -d engine=mysql:8.4 >/dev/null
mysql_engine maria mariadb:11.4 "mariadb --skip-ssl" "MariaDB"
mysql_engine mysql mysql:8.4 mysql "MySQL"
check "[A] unknown engine falls back to PostgreSQL" "$(code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=oracle -d engine=oracle:23 >/dev/null; my_db oracle engine)" postgres
code_of ja -X POST "$BASE/o/$A/p/shop/d/oracle/delete" -d csrf=$CSRF_A -d confirm=oracle >/dev/null
section "Managed MongoDB"
check "[A] MongoDB created" "$(code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=mongo -d engine=mongodb:8.0)" 303
st=; for _ in $(seq 1 150); do st=$(my_db mongo status); [ "$st" = running ] || [ "$st" = failed ] && break; sleep 2; done
check "[A] MongoDB running" "$st" running
MCID=$(my_db mongo container_id)
MHOST=$(owner_sql "SELECT 'pd-' || left(replace(d.id::text, '-', ''), 12) FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'mongo'")
mongo_js() { dx exec "$MCID" sh -c "mongosh --quiet --host 127.0.0.1 --username=\"\$MONGO_INITDB_ROOT_USERNAME\" --password=\"\$MONGO_INITDB_ROOT_PASSWORD\" --authenticationDatabase admin app --eval '$1'" 2>&1 | tr -d '\r'; }
check "[A] MongoDB refuses a wrong password" "$(dx exec "$MCID" sh -c "mongosh --quiet --host 127.0.0.1 -u app -p wrong --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok'" >/dev/null 2>&1 && echo yes || echo no)" no
if dx exec "$CID_F" sh -c "echo | nc -w 3 $MHOST 27017" >/dev/null 2>&1; then ok "[A] service in the same project reaches MongoDB"; else bad "[A] service cannot reach MongoDB"; fi
if dx exec "$CID_B" sh -c "echo | nc -w 3 $MHOST 27017" >/dev/null 2>&1; then bad "[B->A] B's service reached A's MongoDB"; else ok "[B->A] B's service cannot reach A's MongoDB"; fi
code_of ja -X POST "$BASE/o/$A/p/shop/d/mongo/attach" -d csrf=$CSRF_A -d service=files -d name=MONGODB_URI >/dev/null
check "[A] MONGODB_URI stored as a secret" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND v.slug = 'files' AND s.name = 'MONGODB_URI'")" 1
if grep -q "mongodb://app:.*authSource=admin" <<<"$(c ja "$BASE/o/$A/p/shop/d/mongo")"; then ok "[A] MongoDB page shows a mongodb:// URL"; else bad "MongoDB page lacks the connection URL"; fi
mongo_js "db.notes.insertOne({t: \"$MARK-g1\"})" >/dev/null
check "[A] MongoDB stores documents" "$(mongo_js 'db.notes.findOne().t')" "$MARK-g1"
code_of ja -X POST "$BASE/o/$A/p/shop/d/mongo/backups" -d csrf=$CSRF_A >/dev/null
check "[A] MongoDB backup completes (mongodump)" "$(my_wait_job mongo backup)" done
MSNAP=$(owner_sql "SELECT b.snapshot_id FROM backup_snapshots b JOIN databases d ON d.id = b.database_id JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND d.slug = 'mongo' ORDER BY b.taken_at DESC LIMIT 1")
mongo_js "db.notes.updateOne({}, {\$set: {t: \"$MARK-g2\"}})" >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/d/mongo/backups/restore/$MSNAP" -d csrf=$CSRF_A -d confirm=mongo >/dev/null
check "[A] MongoDB restore completes (mongorestore)" "$(my_wait_job mongo restore)" done
check "[A] MongoDB documents are back to the snapshot" "$(mongo_js 'db.notes.findOne().t')" "$MARK-g1"
code_of ja -X POST "$BASE/o/$A/p/shop/d/mongo/delete" -d csrf=$CSRF_A -d confirm=mongo >/dev/null

section "Managed Kafka (streaming)"
check "[A] Kafka created" "$(code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=events -d engine=kafka:3.9)" 303
st=; for _ in $(seq 1 120); do st=$(my_db events status); [ "$st" = running ] || [ "$st" = failed ] && break; sleep 2; done
check "[A] Kafka broker running" "$st" running
KCID=$(my_db events container_id)
KHOST=$(owner_sql "SELECT 'pd-' || left(replace(d.id::text, '-', ''), 12) FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'events'")
KPAGE=$(c ja "$BASE/o/$A/p/shop/d/events")
KURL=$(grep -o 'kafka://[^<]*' <<<"$KPAGE" | head -1 | sed 's/&amp;/\&/g')
check "[A] the page shows the bootstrap server and a kafka:// URL with SASL settings" "$(grep -c "$KHOST:9092" <<<"$KPAGE" | sed 's/^[1-9][0-9]*$/yes/'):$(grep -c 'sasl.mechanism=PLAIN' <<<"$KURL")" "yes:1"
KUSER=$(sed -E 's|kafka://([^:]+):.*|\1|' <<<"$KURL"); KPASS=$(sed -E 's|kafka://[^:]+:([^@]+)@.*|\1|' <<<"$KURL")
if dx exec "$CID_F" sh -c "echo | nc -w 3 $KHOST 9092" >/dev/null 2>&1; then ok "[A] service in the same project reaches Kafka"; else bad "[A] service cannot reach Kafka"; fi
if dx exec "$CID_B" sh -c "echo | nc -w 3 $KHOST 9092" >/dev/null 2>&1; then bad "[B->A] B's service reached A's Kafka"; else ok "[B->A] B's service cannot reach A's Kafka"; fi
KNET=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$KCID")
KIP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$KCID")
kclient() { # kclient <password> <shell commands>: a Kafka client container on the project network
  MSYS_NO_PATHCONV=1 docker run --rm --network "$KNET" --add-host "$KHOST:$KIP" -e "KU=$KUSER" -e "KP=$1" --entrypoint bash apache/kafka:3.9.1 -c \
    'printf "%s\n" security.protocol=SASL_PLAINTEXT sasl.mechanism=PLAIN "sasl.jaas.config=org.apache.kafka.common.security.plain.PlainLoginModule required username=\"$KU\" password=\"$KP\";" > /tmp/c.properties; '"$2" 2>&1 | tr -d '\r'; }
check "[A] a message is produced and consumed with the database's credentials" "$(kclient "$KPASS" "echo order-$RUN | /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server $KHOST:9092 --producer.config /tmp/c.properties --topic orders >/dev/null 2>&1; /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server $KHOST:9092 --consumer.config /tmp/c.properties --topic orders --from-beginning --max-messages 1 --timeout-ms 40000 2>/dev/null" | grep -c "order-$RUN")" 1
check "[A] a wrong password is refused" "$(kclient wrong "timeout 40 /opt/kafka/bin/kafka-topics.sh --bootstrap-server $KHOST:9092 --command-config /tmp/c.properties --list" | grep -ci 'authentication failed' | sed 's/^[1-9][0-9]*$/yes/')" yes
check "[A] Kafka has no backups (refused, and not scheduled)" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/shop/d/events/backups" -d csrf=$CSRF_A | grep -c 'err='):$(owner_sql "SELECT backup_enabled FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'events'")" "1:f"
code_of ja -X POST "$BASE/o/$A/p/shop/d/events/attach" -d csrf=$CSRF_A -d service=files -d name=KAFKA_URL >/dev/null
check "[A] KAFKA_URL stored as a secret of a service" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND v.slug = 'files' AND s.name = 'KAFKA_URL'")" 1
# Project map: the connection secret above draws files -> events
pmap() { c "$1" -H 'HX-Request: true' "$BASE/o/$2/p/$3/map"; }
PMAP=$(pmap ja "$A" shop)
check "project map: services and the Kafka stream are drawn, with the link from the secret" "$(grep -c 'class="pmap-svg"' <<<"$PMAP"):$(grep -c '>files</text>' <<<"$PMAP"):$(grep -c '>events</text>' <<<"$PMAP"):$(grep -cF 'files → events' <<<"$PMAP")" "1:1:1:1"
check "project map: the project page embeds it, live (htmx polling)" "$(c ja "$BASE/o/$A/p/shop" | grep -c 'id="project-map" hx-get="/o/'"$A"'/p/shop/map" hx-trigger="every')" 1
check "project map: a plain request is sent to the project page" "$(code_of ja "$BASE/o/$A/p/shop/map")" 303
check "[B->A] B cannot read A's project map (404)" "$(c jb -o /dev/null -w '%{http_code}' -H 'HX-Request: true' "$BASE/o/$A/p/shop/map")" 404
check "project map: no secret value in it" "$(grep -c "$KPASS" <<<"$PMAP")" 0
code_of ja -X POST "$BASE/o/$A/p/shop/d/events/delete" -d csrf=$CSRF_A -d confirm=events >/dev/null
check "[A] Kafka deleted with its container" "$(for _ in $(seq 1 30); do docker inspect "$KCID" >/dev/null 2>&1 || { echo gone; break; }; sleep 2; done)" gone
code_of ja -X POST "$BASE/o/$A/p/shop/s/files/secrets/MONGODB_URI/delete" -d csrf=$CSRF_A >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "API tokens and platformctl"
api_code() { local t=$1; shift; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $t" "$@"; }
api_get() { local t=$1; shift; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $t" "$@"; }
new_api_token() { c "$1" -X POST "$BASE/o/$2/api-tokens" -d "csrf=$3" -d "name=$4" -d "role=$5" | grep -o 'pla_[A-Za-z0-9_-]*' | head -1; }
TOK_DEV=$(new_api_token ja "$A" "$CSRF_A" ci developer)
TOK_VIEW=$(new_api_token ja "$A" "$CSRF_A" dashboard viewer)
TOK_B=$(new_api_token jb "$B" "$CSRF_B" ci developer)
if [ -n "$TOK_DEV" ] && [ -n "$TOK_VIEW" ] && [ -n "$TOK_B" ]; then ok "API tokens created in the console"; else bad "API token not shown"; fi
check "[API] request without a token refused (401)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "$BASE/api/v1/projects")" 401
check "[API] whoami names the organization" "$(api_get "$TOK_DEV" "$BASE/api/v1/me" | grep -o "\"organization\":\"$A\"")" "\"organization\":\"$A\""
check "[B->A API] B's token cannot see A's service (404)" "$(api_code "$TOK_B" "$BASE/api/v1/projects/shop/services/files")" 404
check "[API] viewer token cannot deploy (403)" "$(api_code "$TOK_VIEW" -X POST "$BASE/api/v1/projects/shop/services/web/deploy")" 403
check "[API] viewer token can read" "$(api_code "$TOK_VIEW" "$BASE/api/v1/projects/shop/services/web")" 200
(cd .. && go build -o "$TMP/platformctl.exe" ./cmd/platformctl) || bad "platformctl did not build"
pctl() { PLATFORM_URL="$BASE" PLATFORM_TOKEN="$TOK_DEV" "$TMP/platformctl.exe" "$@" 2>&1; }
if pctl deploy shop/web --image nginx:1.27-alpine --wait | grep -q "is live"; then ok "[CLI] platformctl deploy --wait reaches live"; else bad "[CLI] platformctl deploy failed"; fi
if pctl status shop/web | grep -q "live"; then ok "[CLI] platformctl status shows the live revision"; else bad "[CLI] status missing"; fi
if echo "cli-secret-$RUN" | pctl secret set shop/web CLI_TEST | grep -q "saved"; then ok "[CLI] platformctl secret set"; else bad "[CLI] secret set failed"; fi
if c ja "$BASE/o/$A/audit" | grep -q "via_api_token"; then ok "[API] audit log names the token"; else bad "[API] token missing from the audit log"; fi
section "Object storage (S3)"
S3=http://s3.localhost:$PORT
s3c() { local k=$1 s=$2; shift 2; curl -sS --resolve "s3.localhost:$PORT:127.0.0.1" --aws-sigv4 "aws:amz:us-east-1:s3" --user "$k:$s" "$@"; }
s3code() { local k=$1 s=$2; shift 2; s3c "$k" "$s" -o /dev/null -w '%{http_code}' "$@"; }
storage_poke() { owner_sql "SELECT pg_notify('deployments', 'storage')" >/dev/null; }
new_s3_key() { # new_s3_key <jar> <org> <csrf> <name> <perm args...>: prints "KEYID SECRET"
  local jar=$1 org=$2 csrf=$3 name=$4; shift 4
  local page; page=$(c "$jar" -X POST "$BASE/o/$org/storage/keys" -d csrf=$csrf -d "name=$name" "$@")
  echo "$(grep -o 'GK[0-9a-f]\{24\}' <<<"$page" | head -1) $(sed -n 's/.*select wrap">\([0-9a-f]\{64\}\)<.*/\1/p' <<<"$page" | head -1)"
}
check "[A] storage page opens" "$(code_of ja "$BASE/o/$A/storage")" 200
check "[A] bucket uploads created (5 MB)" "$(code_of ja -X POST "$BASE/o/$A/storage/buckets" -d csrf=$CSRF_A -d name=uploads -d quota_mb=5)" 303
check "[A] bucket row and Garage bucket recorded" "$(owner_sql "SELECT full_name || ':' || (garage_id <> '') FROM buckets b JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND b.name = 'uploads'")" "$A-uploads:true"
if grep -q "err=Bucket+name" <<<"$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/storage/buckets" -d csrf=$CSRF_A -d name=Bad_Name)"; then ok "invalid bucket name refused"; else bad "invalid bucket name accepted"; fi
if grep -q "err=Quota" <<<"$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/storage/buckets" -d csrf=$CSRF_A -d name=huge -d quota_mb=99999999)"; then ok "bucket quota above the plan refused"; else bad "bucket quota above the plan accepted"; fi
check "[B->A] B cannot open A's bucket (404)" "$(code_of jb "$BASE/o/$A/storage/b/uploads")" 404
read -r KW SW <<<"$(new_s3_key ja "$A" "$CSRF_A" e2e-rw -d perm_uploads=write)"
read -r KR SR <<<"$(new_s3_key ja "$A" "$CSRF_A" e2e-ro -d perm_uploads=read)"
if [ -n "$KW" ] && [ ${#SW} = 64 ] && [ -n "$KR" ]; then ok "[A] access keys created, secret shown once"; else bad "access key secret not shown ($KW)"; fi
check "secret not shown again on the storage page" "$(c ja "$BASE/o/$A/storage" | grep -c "$SW")" 0
echo "hello-s3-$RUN" > "$TMP/s3obj.txt"
check "[S3] read-write key uploads an object" "$(s3code "$KW" "$SW" -T "$TMP/s3obj.txt" "$S3/$A-uploads/docs/hello.txt")" 200
check "[S3] the object reads back" "$(s3c "$KW" "$SW" "$S3/$A-uploads/docs/hello.txt")" "hello-s3-$RUN"
if grep -q "<Key>docs/hello.txt</Key>" <<<"$(s3c "$KR" "$SR" "$S3/$A-uploads?list-type=2")"; then ok "[S3] read-only key lists the bucket"; else bad "[S3] read-only key cannot list"; fi
check "[S3] read-only key cannot write (403)" "$(s3code "$KR" "$SR" -T "$TMP/s3obj.txt" "$S3/$A-uploads/denied.txt")" 403
check "[S3] request without credentials refused (403)" "$(curl -sS --resolve "s3.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "$S3/$A-uploads/docs/hello.txt")" 403
check "[S3] wrong secret refused (403)" "$(s3code "$KW" "0000000000000000000000000000000000000000000000000000000000000000" "$S3/$A-uploads/docs/hello.txt")" 403
code_of jb -X POST "$BASE/o/$B/storage/buckets" -d csrf=$CSRF_B -d name=media -d quota_mb=5 >/dev/null
read -r KB SB <<<"$(new_s3_key jb "$B" "$CSRF_B" b-key -d perm_media=write)"
check "[B->A S3] B's key cannot read A's bucket (403)" "$(s3code "$KB" "$SB" "$S3/$A-uploads/docs/hello.txt")" 403
check "[B->A S3] B's key cannot write A's bucket (403)" "$(s3code "$KB" "$SB" -T "$TMP/s3obj.txt" "$S3/$A-uploads/x.txt")" 403
if grep -q "perm_uploads" <<<"$(c jb "$BASE/o/$B/storage")"; then bad "B's key form offers A's bucket"; else ok "B's key form lists only B's buckets"; fi
head -c 6291456 /dev/zero > "$TMP/s3big.bin"
check "[S3] upload beyond the bucket quota refused" "$(s3code "$KW" "$SW" -T "$TMP/s3big.bin" "$S3/$A-uploads/big.bin")" 403
echo "console-upload-$RUN" > "$TMP/console.txt"
check "[A] console upload" "$(code_of ja -X POST "$BASE/o/$A/storage/b/uploads/objects/upload" -F csrf=$CSRF_A -F prefix=docs/ -F "file=@$TMP/console.txt")" 303
PAGE=$(c ja "$BASE/o/$A/storage/b/uploads?prefix=docs/")
if grep -q "console.txt" <<<"$PAGE" && grep -q "hello.txt" <<<"$PAGE"; then ok "[A] object browser lists the folder"; else bad "object browser does not list the objects"; fi
LINK=$(c ja -o /dev/null -w '%{redirect_url}' "$BASE/o/$A/storage/b/uploads/objects/download?key=docs/console.txt")
check "[A] download link is a presigned URL that works" "$(curl -sS --resolve "s3.localhost:$PORT:127.0.0.1" "$LINK")" "console-upload-$RUN"
check "[A] presigned URL refused once tampered (403)" "$(curl -sS --resolve "s3.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "${LINK/console.txt/hello.txt}")" 403
check "[A] console delete of an object" "$(code_of ja -X POST "$BASE/o/$A/storage/b/uploads/objects/delete" -d csrf=$CSRF_A -d key=docs/console.txt)" 303
check "deleted object is gone (404)" "$(s3code "$KW" "$SW" "$S3/$A-uploads/docs/console.txt")" 404
storage_poke
check "usage refreshed by the agent" "$(for _ in $(seq 1 30); do [ "$(owner_sql "SELECT objects FROM buckets b JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND b.name = 'uploads'")" = 1 ] && { echo 1; break; }; sleep 2; done)" 1
check "storage usage metered for invoices" "$(owner_sql "SELECT count(*) > 0 FROM usage_hourly u JOIN orgs o ON o.id = u.org_id WHERE o.slug = '$A' AND u.kind = 'bucket' AND u.storage_bytes > 0")" t

echo "<h1>site-$RUN</h1>" > "$TMP/index.html"
WEB_HOST=$A-uploads.web.apps.localhost
web_get() { curl -sS --resolve "$WEB_HOST:$PORT:127.0.0.1" -o "$TMP/web.out" -w '%{http_code}' "http://$WEB_HOST:$PORT/"; }
s3code "$KW" "$SW" -T "$TMP/index.html" -H "Content-Type: text/html" "$S3/$A-uploads/index.html" >/dev/null
check "website off: bucket not served (404)" "$(web_get)" 404
check "[A] website turned on" "$(code_of ja -X POST "$BASE/o/$A/storage/b/uploads/settings" -d csrf=$CSRF_A -d quota_mb=5 -d website=1 -d index_document=index.html)" 303
check "public website serves index.html" "$(web_get):$(cat "$TMP/web.out")" "200:<h1>site-$RUN</h1>"
tls_ask() { dx run --rm --network platform-core curlimages/curl:8.11.1 -s -o /dev/null -w '%{http_code}' "http://api:8081/internal/tls-ask?domain=$1"; }
check "website host approved for a certificate" "$(tls_ask "$WEB_HOST")" 200
check "unknown website host refused a certificate" "$(tls_ask nope-x.web.apps.localhost)" 404
code_of ja -X POST "$BASE/o/$A/storage/b/uploads/settings" -d csrf=$CSRF_A -d quota_mb=5 -d index_document=index.html >/dev/null
check "website off again (404)" "$(web_get)" 404

check "[A] bucket connected to service files" "$(code_of ja -X POST "$BASE/o/$A/storage/b/uploads/attach" -d csrf=$CSRF_A -d service=shop/files)" 303
check "service got the AWS secrets" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND v.slug = 'files' AND s.name IN ('AWS_ACCESS_KEY_ID','AWS_SECRET_ACCESS_KEY','AWS_ENDPOINT_URL','AWS_REGION','S3_BUCKET')")" 5
check "service key recorded with its service" "$(owner_sql "SELECT count(*) FROM storage_keys k JOIN services v ON v.id = k.service_id WHERE v.slug = 'files' AND k.org_id = (SELECT id FROM orgs WHERE slug = '$A')")" 1
code_of ja -X POST "$BASE/o/$A/p/shop/s/files/deploy" -d csrf=$CSRF_A >/dev/null
CID_S3=""; for _ in $(seq 1 60); do CID_S3=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'files' AND d.status = 'live' ORDER BY d.revision DESC LIMIT 1"); [ -n "$CID_S3" ] && [ "$CID_S3" != "$CID_F" ] && break; sleep 2; done
check "redeployed service has the S3 variables" "$(dx exec "$CID_S3" printenv S3_BUCKET 2>&1 | tr -d '\r')" "$A-uploads"
if grep -q "403" <<<"$(dx exec "$CID_S3" sh -c 'wget -qO- "$AWS_ENDPOINT_URL/$S3_BUCKET/docs/hello.txt" 2>&1')"; then ok "service reaches the S3 endpoint through the proxy (unsigned request refused)"; else bad "service cannot reach the S3 endpoint"; fi
CID_F=$CID_S3

owner_sql "UPDATE orgs SET suspended = true WHERE slug = '$A'" >/dev/null; storage_poke
check "suspended organization: its keys lose access" "$(for _ in $(seq 1 30); do [ "$(s3code "$KW" "$SW" "$S3/$A-uploads/docs/hello.txt")" = 403 ] && { echo 403; break; }; sleep 2; done)" 403
owner_sql "UPDATE orgs SET suspended = false WHERE slug = '$A'" >/dev/null; storage_poke
check "reactivated organization: access restored" "$(for _ in $(seq 1 30); do [ "$(s3code "$KW" "$SW" "$S3/$A-uploads/docs/hello.txt")" = 200 ] && { echo 200; break; }; sleep 2; done)" 200

KR_ID=$(owner_sql "SELECT k.id FROM storage_keys k JOIN orgs o ON o.id = k.org_id WHERE o.slug = '$A' AND k.name = 'e2e-ro'")
check "[A] access key deleted" "$(code_of ja -X POST "$BASE/o/$A/storage/keys/$KR_ID/delete" -d csrf=$CSRF_A)" 303
check "deleted key refused (403)" "$(for _ in $(seq 1 30); do [ "$(s3code "$KR" "$SR" "$S3/$A-uploads?list-type=2")" = 403 ] && { echo 403; break; }; sleep 2; done)" 403

check "[API] bucket created with an API token" "$(api_code "$TOK_DEV" -X POST -H 'Content-Type: application/json' -d '{"name":"api-bucket","quota_mb":2}' "$BASE/api/v1/buckets")" 201
check "[API] viewer token cannot create a bucket (403)" "$(api_code "$TOK_VIEW" -X POST -H 'Content-Type: application/json' -d '{"name":"nope"}' "$BASE/api/v1/buckets")" 403
if pctl bucket list | grep -q "$A-api-bucket"; then ok "[CLI] platformctl bucket list"; else bad "[CLI] bucket list misses the bucket"; fi
OUT=$(pctl key create cli-key api-bucket=write)
KC=$(sed -n 's/^AWS_ACCESS_KEY_ID=//p' <<<"$OUT"); SC=$(sed -n 's/^AWS_SECRET_ACCESS_KEY=//p' <<<"$OUT")
check "[CLI] key created by platformctl writes to its bucket" "$(s3code "$KC" "$SC" -T "$TMP/s3obj.txt" "$S3/$A-api-bucket/f.txt")" 200
check "[CLI] that key cannot reach other buckets (403)" "$(s3code "$KC" "$SC" "$S3/$A-uploads/docs/hello.txt")" 403
check "[B->A API] B's token cannot list A's buckets" "$(api_get "$TOK_B" "$BASE/api/v1/buckets" | grep -c "$A-api-bucket")" 0
if grep -q "HTTP 403" <<<"$(pctl bucket delete api-bucket)"; then ok "[CLI] developer token cannot delete a bucket (admin only)"; else bad "[CLI] developer token deleted a bucket"; fi
check "[A] bucket deleted by an admin in the console" "$(code_of ja -X POST "$BASE/o/$A/storage/b/api-bucket/delete" -d csrf=$CSRF_A -d confirm=api-bucket)" 303

check "[A] non-empty bucket deleted" "$(code_of ja -X POST "$BASE/o/$A/storage/b/uploads/delete" -d csrf=$CSRF_A -d confirm=uploads)" 303
storage_poke
check "deleted buckets emptied and removed from Garage" "$(for _ in $(seq 1 45); do [ "$(owner_sql "SELECT count(*) FROM storage_trash")" = 0 ] && { echo done; break; }; sleep 2; done)" done
check "objects of the deleted bucket are gone" "$(s3code "$KW" "$SW" "$S3/$A-uploads/docs/hello.txt")" 404
check "bucket deletion audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'bucket.deleted' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" 2

section "Public TCP/UDP ports (non-HTTP services)"
admin set-plan --slug "$A" --plan pro >/dev/null   # room for two more services
NET=/o/$A/p/shop/s
code_of ja -X POST "$BASE/o/$A/compute" -d csrf=$CSRF_A -d project=shop -d slug=tcpsrv -d image=nginx:1.27-alpine -d kind=worker -d cpu_millis=100 -d memory_mb=64 >/dev/null
check "[A] worker exposes 80/tcp and deploys" "$(code_of ja -X POST "$BASE$NET/tcpsrv/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d kind=worker -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d port=8080 -d health_path=%2F -d volume_path= -d env= -d ports=80/tcp -d deploy=1)" 303
TCP_PORT=$(owner_sql "SELECT (s.ports->0->>'public_port') FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'tcpsrv'")
check "a public port of the range was assigned" "$([ "${TCP_PORT:-0}" -ge 20000 ] && [ "$TCP_PORT" -le 20999 ] && echo yes)" yes
check "[A] TCP service live" "$(wait_status ja $NET/tcpsrv)" live
check "the TCP port answers from outside" "$(for _ in $(seq 1 20); do curl -s --max-time 3 "http://127.0.0.1:$TCP_PORT/" | grep -q 'Welcome to nginx' && { echo yes; break; }; sleep 1; done)" yes
if grep -q "tcpsrv-shop-$A.apps.localhost:$TCP_PORT" <<<"$(c ja "$BASE$NET/tcpsrv")"; then ok "service page shows the public endpoint"; else bad "public endpoint not shown"; fi
code_of ja -X POST "$BASE$NET/tcpsrv/deploy" -d csrf=$CSRF_A >/dev/null
check "redeploy replaces the container and keeps the port" "$(wait_status ja $NET/tcpsrv):$(owner_sql "SELECT (s.ports->0->>'public_port') FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'tcpsrv'")" "live:$TCP_PORT"
check "the port answers after the redeploy" "$(for _ in $(seq 1 20); do curl -s --max-time 3 "http://127.0.0.1:$TCP_PORT/" | grep -q 'Welcome to nginx' && { echo yes; break; }; sleep 1; done)" yes
check "public ports need a single instance" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE$NET/tcpsrv/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d kind=worker -d cpu_millis=100 -d memory_mb=64 -d instances=2 -d port=8080 -d health_path=%2F -d volume_path= -d env= -d ports=80/tcp | grep -c 'single+instance')" 1
code_of ja -X POST "$BASE/o/$A/compute" -d csrf=$CSRF_A -d project=shop -d slug=udpsrv -d image=nginx:1.27-alpine -d kind=worker -d cpu_millis=100 -d memory_mb=64 >/dev/null
UDP_SETTINGS=(-d csrf=$CSRF_A -d image=nginx:1.27-alpine -d kind=worker -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d port=8080 -d health_path=%2F -d volume_path= -d env=)
check "[A] UDP echo server exposes 9999/udp" "$(code_of ja -X POST "$BASE$NET/udpsrv/settings" "${UDP_SETTINGS[@]}" -d ports=9999/udp -d deploy=1)" 303
UDP_PORT=$(owner_sql "SELECT (s.ports->0->>'public_port') FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'udpsrv'")
check "[A] UDP service live" "$(wait_status ja $NET/udpsrv)" live
UDP_CID=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'udpsrv' AND d.status = 'live' ORDER BY d.revision DESC LIMIT 1")
check "the container publishes the UDP port on the public port" "$(docker inspect "$UDP_CID" --format '{{range $p, $b := .HostConfig.PortBindings}}{{$p}}={{(index $b 0).HostPort}} {{end}}' | grep -o "9999/udp=$UDP_PORT")" "9999/udp=$UDP_PORT"
admin set-plan --slug "$A" --plan starter >/dev/null   # Starter: 2 public ports
check "over the plan's public ports refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE$NET/udpsrv/settings" "${UDP_SETTINGS[@]}" -d $'ports=9999/udp\n9998/udp\n9997/udp\n9996/udp\n9995/udp' | grep -c 'public+TCP')" 1
admin set-plan --slug "$A" --plan pro >/dev/null
code_of ja -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/tcpsrv -d sel=shop/udpsrv >/dev/null
check "deleting the services frees their public ports" "$(owner_sql "SELECT count(*) FROM public_ports p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A'")" 0
admin set-plan --slug "$A" --plan starter >/dev/null

section "Console: services, favorites, project filter, search"
for page in services projects compute compute/jobs compute/previews databases domains monitoring iam registry; do
  check "[A] console page $page opens" "$(code_of ja "$BASE/o/$A/$page")" 200
done
check "[B->A] B cannot open A's compute list (404)" "$(code_of jb "$BASE/o/$A/compute")" 404
check "[B->A] B cannot search A (404)" "$(code_of jb "$BASE/o/$A/search?q=web")" 404
HOME_PAGE=$(c ja "$BASE/o/$A")
if grep -q 'class="favbar"' <<<"$HOME_PAGE" && grep -q ">Compute<" <<<"$HOME_PAGE"; then ok "favorites bar shows the default services"; else bad "favorites bar missing"; fi
check "[A] pin Domains" "$(code_of ja -X POST "$BASE/o/$A/pins" -d csrf=$CSRF_A -d product=domains -d back=/o/$A)" 303
if grep -q "/o/$A/domains\"" <<<"$(c ja "$BASE/o/$A/compute" | sed -n '/class="favbar"/,/<\/nav>/p')"; then ok "pinned service appears in the favorites bar"; else bad "pinned service missing from the favorites bar"; fi
code_of ja -X POST "$BASE/o/$A/pins" -d csrf=$CSRF_A -d product=domains >/dev/null
if grep -q "/o/$A/domains\"" <<<"$(c ja "$BASE/o/$A" | sed -n '/class="favbar"/,/<\/nav>/p')"; then bad "unpinned service still in the favorites bar"; else ok "unpinned service leaves the favorites bar"; fi
check "pins are per user (B's bar unchanged)" "$(c jb "$BASE/o/$B" | sed -n '/class="favbar"/,/<\/nav>/p' | grep -c "/domains\"")" 0
check "unknown product cannot be pinned (404)" "$(code_of ja -X POST "$BASE/o/$A/pins" -d csrf=$CSRF_A -d product=nope)" 404
COMPUTE=$(c ja "$BASE/o/$A/compute")
if grep -q "/o/$A/p/shop/s/web\"" <<<"$COMPUTE" && grep -q "/o/$A/p/shop/s/files\"" <<<"$COMPUTE"; then ok "compute lists the services of every project"; else bad "compute list incomplete"; fi
code_of ja "$BASE/o/$A/scope?project=nope-project&back=/o/$A/compute" >/dev/null
svc_table() { c ja "$BASE/o/$A/compute" | sed -n '/id="services-table"/,/<\/table>/p'; }
check "project filter narrows the compute list" "$(svc_table | grep -c "/o/$A/p/shop/s/web\"")" 0
code_of ja "$BASE/o/$A/scope?project=&back=/o/$A/compute" >/dev/null
check "clearing the project filter shows everything again" "$(svc_table | grep -c "/o/$A/p/shop/s/web\"")" 1
check "filter redirect stays inside the organization" "$(c ja -o /dev/null -w '%{redirect_url}' "$BASE/o/$A/scope?project=shop&back=https://evil.example/")" "$BASE/o/$A"
code_of ja "$BASE/o/$A/scope?project=&back=/o/$A" >/dev/null
if grep -q "/o/$A/p/shop/s/web\"" <<<"$(c ja "$BASE/o/$A/search?q=web")"; then ok "search finds a service"; else bad "search misses the service"; fi
if grep -q "/o/$A/storage\"" <<<"$(c ja "$BASE/o/$A/search?q=bucket")"; then ok "search finds products by description"; else bad "search misses products"; fi
check "B's resources never appear in A's search" "$(c ja "$BASE/o/$A/search?q=$B" | grep -c "/o/$B/")" 0
check "[A] database created from the Databases area" "$(code_of ja -X POST "$BASE/o/$A/databases" -d csrf=$CSRF_A -d project=shop -d slug=areadb -d engine=redis:7)" 303
check "area database recorded in the chosen project" "$(owner_sql "SELECT p.slug FROM databases d JOIN projects p ON p.id = d.project_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'areadb'")" shop
if grep -q "/o/$A/p/shop/d/areadb\"" <<<"$(c ja "$BASE/o/$A/databases")"; then ok "databases area lists it"; else bad "databases area misses it"; fi
code_of ja -X POST "$BASE/o/$A/p/shop/d/areadb/delete" -d csrf=$CSRF_A -d confirm=areadb >/dev/null
check "[B->A] B cannot create a database in A's project (404)" "$(code_of jb -X POST "$BASE/o/$A/databases" -d csrf=$CSRF_B -d project=shop -d slug=evil -d engine=redis:7)" 404
admin set-plan --slug "$A" --plan pro >/dev/null   # room for two more services
check "[A] service created from the Compute area" "$(code_of ja -X POST "$BASE/o/$A/compute" -d csrf=$CSRF_A -d project=shop -d slug=area-svc -d image=nginx:1.27-alpine)" 303
check "area service recorded in the chosen project" "$(owner_sql "SELECT count(*) FROM services s JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND p.slug = 'shop' AND s.slug = 'area-svc'")" 1
check "[A] full create form: worker with 2 instances and variables, deployed" "$(code_of ja -X POST "$BASE/o/$A/compute" -d csrf=$CSRF_A -d project=shop -d slug=area-wkr -d image=nginx:1.27-alpine -d kind=worker -d cpu_millis=100 -d memory_mb=64 -d instances=2 --data-urlencode $'env=MODE=batch
LEVEL=2' -d deploy=1)" 303
check "create form stored type, instances and variables" "$(owner_sql "SELECT s.worker || ':' || s.instances || ':' || (s.env->>'MODE') FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'area-wkr'")" "true:2:batch"
check "create form started the first deployment" "$(owner_sql "SELECT count(*) FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'area-wkr'")" 1
if grep -q "err=Instances\|err=.*instances\|err=" <<<"$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/compute" -d csrf=$CSRF_A -d project=shop -d slug=area-bad -d image=nginx:1.27-alpine -d instances=50)"; then ok "create form refuses too many instances"; else bad "create form accepted 50 instances"; fi
check "[A] bulk redeploy of two services" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=redeploy -d sel=shop/area-svc -d sel=shop/area-wkr | grep -c 'msg=Redeploying+2')" 1
check "bulk delete needs the typed confirmation" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d sel=shop/area-wkr | grep -c 'err=')" 1
check "[B->A] B's bulk action cannot touch A's services" "$(code_of jb -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_B -d action=delete -d confirm=delete -d sel=shop/area-wkr)" 404
check "[A] bulk delete of two services" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/area-svc -d sel=shop/area-wkr | grep -c 'msg=Deleted+2')" 1
check "bulk-deleted services are gone" "$(owner_sql "SELECT count(*) FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug IN ('area-svc', 'area-wkr')")" 0
admin set-plan --slug "$A" --plan starter >/dev/null
if grep -q 'aria-label="Breadcrumb"' <<<"$(c ja "$BASE/o/$A/p/shop/s/web")"; then ok "resource pages show a breadcrumb"; else bad "breadcrumb missing"; fi

section "Functions"
FN=$BASE/o/$A/p/shop/f
fn_api() { local m=$1 path=$2; shift 2; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" -H 'Content-Type: application/json' -X "$m" "$BASE/api/v1/projects/shop/functions/$path" "$@"; }
check "[A] function created in the console" "$(code_of ja -X POST "$BASE/o/$A/functions" -d csrf=$CSRF_A -d project=shop -d slug=hello -d runtime=nodejs22)" 303
check "invalid function name refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/functions" -d csrf=$CSRF_A -d project=shop -d slug=Bad_Name -d runtime=nodejs22 | grep -c 'err=')" 1
PAGE=$(c ja -X POST "$FN/hello/test" -d csrf=$CSRF_A --data-urlencode 'event={"rawPath":"/e2e"}')
if grep -q "st-succeeded" <<<"$PAGE" && grep -q "Hello from hello" <<<"$PAGE"; then ok "[A] console test runs the starter code"; else bad "console test failed: $(grep -o 'error-text">[^<]*' <<<"$PAGE" | head -1)"; fi
check "[B->A] B cannot open A's function (404)" "$(code_of jb "$FN/hello")" 404
check "[B->A] B cannot test A's function (404)" "$(code_of jb -X POST "$FN/hello/test" -d csrf=$CSRF_B -d 'event={}')" 404
check "[B->A API] B's token cannot invoke A's function (404)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_B" -X POST -d '{}' "$BASE/api/v1/projects/shop/functions/hello/invoke")" 404
CODE='export const handler = async (event, context) => {
  if (event.boom || event.queryStringParameters?.boom) throw new RangeError("e2e boom");
  if (event.sleep) await new Promise((r) => setTimeout(r, event.sleep));
  console.log("e2e log line", context.awsRequestId);
  return { echo: event.value ?? null, env: process.env.E2E_VAR ?? null, shared: process.env.SHARED_TOKEN ? "set" : null, uid: process.getuid(), method: event.requestContext?.http?.method ?? null, body: event.body ?? null };
};'
PUT_BODY=$(printf '%s' "$CODE" | python -c 'import json,sys; print(json.dumps({"code": sys.stdin.read(), "env": {"E2E_VAR": "from-env"}, "timeout_seconds": 3}))')
check "[API] function code deployed with PUT" "$(fn_api PUT hello -o /dev/null -w '%{http_code}' -d "$PUT_BODY")" 200
OUT=$(fn_api POST hello/invoke -d '{"value": 42}')
check "[API] synchronous invoke returns the result" "$(grep -o '"echo":42' <<<"$OUT")" '"echo":42'
check "function sees its environment variable" "$(grep -o '"env":"from-env"' <<<"$OUT")" '"env":"from-env"'
check "function runs as an unprivileged user" "$(grep -o '"uid":65534' <<<"$OUT")" '"uid":65534'
HDRS=$(fn_api POST hello/invoke -D - -o /dev/null -d '{"boom": true}')
check "handler error flagged like AWS Invoke" "$(grep -ci 'X-Platform-Function-Error: Unhandled' <<<"$HDRS")" 1
check "error type recorded" "$(owner_sql "SELECT error_type FROM function_invocations i JOIN functions f ON f.id = i.function_id JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$A' AND f.slug = 'hello' ORDER BY i.created_at DESC LIMIT 1")" RangeError
TO_OUT=$(fn_api POST hello/invoke -d '{"sleep": 6000}' 2>&1)
check "timeout stops the run" "$(grep -o 'Sandbox.Timedout' <<<"$TO_OUT" || echo "response: ${TO_OUT:0:300}")" Sandbox.Timedout
check "function works again after a timeout" "$(fn_api POST hello/invoke -d '{"value": "again"}' | grep -o '"echo":"again"')" '"echo":"again"'
check "[API] asynchronous invoke answers 202" "$(fn_api POST hello/invoke -o /dev/null -w '%{http_code}' -H 'X-Invocation-Type: Event' -d '{"value": "async"}')" 202
check "asynchronous invocation runs" "$(for _ in $(seq 1 30); do [ "$(owner_sql "SELECT count(*) FROM function_invocations i JOIN functions f ON f.id = i.function_id JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$A' AND f.slug = 'hello' AND i.async AND i.status = 'succeeded'")" -ge 1 ] && { echo yes; break; }; sleep 1; done)" yes
check "invocation logs captured" "$(fn_api GET hello/invocations | grep -c 'e2e log line')" 1
if grep -q '"echo":"cli"' <<<"$(pctl fn invoke shop/hello --event '{"value":"cli"}' 2>/dev/null)"; then ok "[CLI] platformctl fn invoke"; else bad "[CLI] fn invoke failed"; fi
if pctl fn invoke shop/hello --event '{"boom":true}' >/dev/null 2>&1; then bad "[CLI] failed invocation exited 0"; else ok "[CLI] failed invocation exits with an error"; fi
printf 'def lambda_handler(event, context):\n    return {"py": True, "name": context.function_name}\n' > "$TMP/handler.py"
if grep -q "deployed" <<<"$(pctl fn deploy shop/pyfn --file "$TMP/handler.py")"; then ok "[CLI] platformctl fn deploy creates a Python function"; else bad "[CLI] fn deploy failed"; fi
check "Python function answers" "$(fn_api POST pyfn/invoke -d '{}' | grep -o '"name": "pyfn"\|"name":"pyfn"' | head -1 | tr -d ' ')" '"name":"pyfn"'

FN_HOST=hello-shop-$A.fn.apps.localhost
fn_url() { curl -sS --resolve "$FN_HOST:$PORT:127.0.0.1" "$@"; }
check "function URL off by default (404)" "$(fn_url -o /dev/null -w '%{http_code}' "http://$FN_HOST:$PORT/")" 404
fn_api PUT hello -o /dev/null -d '{"url_enabled": true, "url_auth": "none"}'
check "function URL answers GET" "$(fn_url "http://$FN_HOST:$PORT/x?y=1" | grep -o '"method":"GET"')" '"method":"GET"'
check "function URL passes the request body" "$(fn_url -X POST -H 'Content-Type: text/plain' -d 'hello-body' "http://$FN_HOST:$PORT/" | grep -o '"body":"hello-body"')" '"body":"hello-body"'
check "handler error on the URL answers 502" "$(fn_url -o /dev/null -w '%{http_code}' "http://$FN_HOST:$PORT/?boom=1")" 502
check "[A] URL switched to token callers" "$(code_of ja -X POST "$FN/hello/settings" -d csrf=$CSRF_A -d memory_mb=128 -d timeout_seconds=3 -d concurrency=5 -d handler=index.handler -d url_enabled=1 -d url_auth=token --data-urlencode 'env=E2E_VAR=from-env')" 303
check "function URL without the token refused (403)" "$(fn_url -o /dev/null -w '%{http_code}' "http://$FN_HOST:$PORT/")" 403
FN_TOKEN=$(c ja -X POST "$FN/hello/token" -d csrf=$CSRF_A | grep -o 'fnt_[A-Za-z0-9_-]*' | head -1)
check "function URL with the token answers" "$(fn_url -o /dev/null -w '%{http_code}' -H "X-Function-Token: $FN_TOKEN" "http://$FN_HOST:$PORT/")" 200
check "function URL with a wrong token refused (403)" "$(fn_url -o /dev/null -w '%{http_code}' -H "X-Function-Token: fnt_wrong" "http://$FN_HOST:$PORT/")" 403
check "token not stored in clear" "$(owner_sql "SELECT count(*) FROM functions WHERE url_token_hash = convert_to('$FN_TOKEN', 'UTF8')")" 0
check "[B] B's own function URL cannot reach A's function" "$(curl -sS --resolve "hello-shop-$B.fn.apps.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "http://hello-shop-$B.fn.apps.localhost:$PORT/")" 404

fn_api PUT hello -o /dev/null -d '{"schedule": "* * * * *", "schedule_enabled": true}'
check "scheduled invocation runs within two minutes" "$(for _ in $(seq 1 75); do [ "$(owner_sql "SELECT count(*) FROM function_invocations i JOIN functions f ON f.id = i.function_id JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$A' AND f.slug = 'hello' AND i.trigger = 'schedule' AND i.status <> 'queued'")" -ge 1 ] && { echo yes; break; }; sleep 2; done)" yes
fn_api PUT hello -o /dev/null -d '{"schedule_enabled": false}'
check "function usage metered for invoices" "$(owner_sql "SELECT sum(fn_requests) > 0 AND sum(fn_mb_ms) > 0 FROM usage_hourly u JOIN orgs o ON o.id = u.org_id WHERE o.slug = '$A' AND u.kind = 'function'")" t
if grep -q "/o/$A/p/shop/f/hello\"" <<<"$(c ja "$BASE/o/$A/functions")"; then ok "functions area lists the function"; else bad "functions area misses it"; fi
if grep -q "Recent invocations" <<<"$(c ja "$FN/hello")"; then ok "function page shows invocations"; else bad "function page broken"; fi
check "[A] function deleted" "$(code_of ja -X POST "$FN/hello/delete" -d csrf=$CSRF_A -d confirm=hello)" 303
check "deleted function's URL gone (404)" "$(fn_url -o /dev/null -w '%{http_code}' -H "X-Function-Token: $FN_TOKEN" "http://$FN_HOST:$PORT/")" 404
pctl fn delete shop/pyfn >/dev/null 2>&1
# Lambda web apps: an ordinary HTTP server in a function
check "[A] web app created in the console" "$(code_of ja -X POST "$BASE/o/$A/functions" -d csrf=$CSRF_A -d project=shop -d slug=webapp -d runtime=nodejs22 -d kind=web)" 303
check "it starts with a server and its start command" "$(owner_sql "SELECT web_command || ':' || web_port FROM functions f JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$A' AND f.slug = 'webapp'")" "node server.mjs:8080"
WA_HOST=webapp-shop-$A.fn.apps.localhost
wa_url() { curl -sS --resolve "$WA_HOST:$PORT:127.0.0.1" "$@"; }
fn_api PUT webapp -o /dev/null -d '{"url_enabled": true, "url_auth": "none"}'
check "the starter web app answers on its URL (cold start)" "$(wa_url "http://$WA_HOST:$PORT/hello?x=1" | grep -o 'Hello from a web app</h1><p>Path: /hello?x=1')" "Hello from a web app</h1><p>Path: /hello?x=1"
WA_CODE='import http from "node:http";
http.createServer((q, s) => {
  let b = "";
  q.on("data", (c) => (b += c));
  q.on("end", () => {
    if (q.url === "/events") {
      s.writeHead(200, { "content-type": "application/json" });
      return s.end(JSON.stringify({ ev: JSON.parse(b || "null"), port: process.env.PORT }));
    }
    s.setHeader("set-cookie", "sid=abc; Path=/");
    s.writeHead(q.url.startsWith("/missing") ? 404 : 201, { "content-type": "text/plain", "x-app": "yes" });
    s.end([q.method, q.url, b, q.headers["x-test"] || "", q.headers.cookie || "", q.headers["x-forwarded-for"] ? "xff" : ""].join("|"));
  });
}).listen(process.env.PORT);'
WA_BODY=$(printf '%s' "$WA_CODE" | python -c 'import json,sys; print(json.dumps({"code": sys.stdin.read()}))')
check "[API] web app code replaced with PUT" "$(fn_api PUT webapp -o /dev/null -w '%{http_code}' -d "$WA_BODY")" 200
WA_HDRS=$(wa_url -D - -o "$TMP/wa.out" -X POST -H 'Content-Type: text/plain' -H 'X-Test: t1' -H 'Cookie: a=1' -d 'payload' "http://$WA_HOST:$PORT/items?q=2")
check "method, path, query, body, headers and cookies reach the server" "$(cat "$TMP/wa.out")" "POST|/items?q=2|payload|t1|a=1|xff"
check "the server's status code and headers come back" "$(grep -o '^HTTP/1.1 201' <<<"$WA_HDRS"):$(grep -ci '^x-app: yes' <<<"$WA_HDRS"):$(grep -ci '^set-cookie: sid=abc' <<<"$WA_HDRS")" "HTTP/1.1 201:1:1"
check "a 404 from the app is passed through" "$(wa_url -o /dev/null -w '%{http_code}' "http://$WA_HOST:$PORT/missing")" 404
check "[API] other events are POSTed to /events" "$(fn_api POST webapp/invoke -d '{"order": 7}' | grep -o '"ev":{"order":7},"port":"8080"')" '"ev":{"order":7},"port":"8080"'
check "web app invocations recorded" "$(owner_sql "SELECT count(*) >= 4 FROM function_invocations i JOIN functions f ON f.id = i.function_id JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$A' AND f.slug = 'webapp' AND i.status = 'succeeded'")" t
fn_api PUT webapp -o /dev/null -d '{"web_command": "node nothere.mjs"}'
check "a server that does not start reports why (502)" "$(wa_url -o /dev/null -w '%{http_code}' --max-time 90 "http://$WA_HOST:$PORT/")" 502
check "the failure names the start problem" "$(owner_sql "SELECT error_type FROM function_invocations i JOIN functions f ON f.id = i.function_id JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$A' AND f.slug = 'webapp' ORDER BY i.created_at DESC LIMIT 1")" Platform.StartFailed
check "[API] an invalid web command is refused (422)" "$(fn_api PUT webapp -o /dev/null -w '%{http_code}' -d '{"web_port": 80}')" 422
fn_api DELETE webapp >/dev/null
ORG_A=$(owner_sql "SELECT id FROM orgs WHERE slug = '$A'")
# Function triggers: bucket events and webhooks
TRIG_CODE='export const handler = async (event) => {
  const r = event.Records?.[0];
  if (r) console.log("bucket-event", r.eventName, r.s3.bucket.name, r.s3.object.key);
  else console.log("webhook-event", event.requestContext?.http?.method, event.body);
  return { ok: true };
};'
check "[API] trigger function deployed" "$(fn_api PUT ontrigger -o /dev/null -w '%{http_code}' -d "$(printf '%s' "$TRIG_CODE" | python -c 'import json,sys; print(json.dumps({"runtime": "nodejs22", "code": sys.stdin.read()}))')")" 201
code_of ja -X POST "$BASE/o/$A/storage/buckets" -d csrf=$CSRF_A -d name=media -d quota_mb=5 >/dev/null
read -r KM SM <<<"$(new_s3_key ja "$A" "$CSRF_A" trigger-writer -d perm_media=write)"
check "[A] bucket trigger added (uploads of *.txt under in/)" "$(code_of ja -X POST "$BASE/o/$A/p/shop/f/ontrigger/triggers" -d csrf=$CSRF_A -d bucket=media -d events=created -d prefix=in/ -d suffix=.txt)" 303
sleep 21   # the agent reloads triggers every 20 seconds
echo "trigger-$RUN" > "$TMP/trig.txt"
for k in "in/doc-$RUN.txt" "in/photo-$RUN.jpg" "out/doc-$RUN.txt"; do s3code "$KM" "$SM" -T "$TMP/trig.txt" "$S3/$A-media/$k" >/dev/null; done
inv_log() { owner_sql "SELECT count(*) FROM function_invocations i JOIN functions f ON f.id = i.function_id WHERE f.org_id = '$ORG_A' AND f.slug = 'ontrigger' AND i.trigger = '$1' AND i.log LIKE '%$2%'"; }
check "an upload matching the trigger runs the function with an S3 event" "$(for _ in $(seq 1 30); do [ "$(inv_log bucket "ObjectCreated:Put $A-media in/doc-$RUN.txt")" = 1 ] && { echo yes; break; }; sleep 2; done)" yes
check "uploads outside the prefix or suffix do not run it" "$(inv_log bucket "photo-$RUN"):$(inv_log bucket "out/doc-$RUN")" "0:0"
s3code "$KM" "$SM" -X DELETE "$S3/$A-media/in/doc-$RUN.txt" >/dev/null; sleep 4
check "a delete does not run an uploads-only trigger" "$(inv_log bucket "ObjectRemoved")" 0
WPAGE=$(c ja -X POST "$BASE/o/$A/p/shop/f/ontrigger/webhook" -d csrf=$CSRF_A)
WURL=$(grep -o "$BASE/hooks/fn/whk_[A-Za-z0-9_-]*" <<<"$WPAGE" | head -1)
check "[A] webhook URL created (shown once)" "$([ -n "$WURL" ] && echo yes)" yes
check "the webhook answers 202 at once" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d "{\"hook\":\"h-$RUN\"}" "$WURL")" 202
check "and runs the function with the request" "$(for _ in $(seq 1 30); do [ "$(inv_log webhook "webhook-event POST")" -ge 1 ] && [ "$(inv_log webhook "h-$RUN")" -ge 1 ] && { echo yes; break; }; sleep 2; done)" yes
check "a wrong webhook token answers 404" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -d x "$BASE/hooks/fn/whk_wrong")" 404
code_of ja -X POST "$BASE/o/$A/p/shop/f/ontrigger/webhook" -d csrf=$CSRF_A -d remove=1 >/dev/null
check "a removed webhook stops working" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -d x "$WURL")" 404
fn_api DELETE ontrigger >/dev/null
s3code "$KM" "$SM" -X DELETE "$S3/$A-media/in/photo-$RUN.jpg" >/dev/null; s3code "$KM" "$SM" -X DELETE "$S3/$A-media/out/doc-$RUN.txt" >/dev/null
code_of ja -X POST "$BASE/o/$A/storage/b/media/delete" -d csrf=$CSRF_A -d confirm=media >/dev/null
ORG_A_ID=$(owner_sql "SELECT id FROM orgs WHERE slug = '$A'")
check "function containers removed after deletion" "$(for _ in $(seq 1 45); do [ "$(docker ps -aq --filter label=platform.function --filter "label=platform.org=$ORG_A_ID" | wc -l)" = 0 ] && { echo yes; break; }; sleep 2; done)" yes

section "Queues"
qapi() { local t=$1 m=$2 path=$3; shift 3; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -X "$m" "$BASE/api/v1/queues$path" "$@"; }
qcode() { local t=$1 m=$2 path=$3; shift 3; qapi "$t" "$m" "$path" -o /dev/null -w '%{http_code}' "$@"; }
check "[A] queue created in the console" "$(code_of ja -X POST "$BASE/o/$A/queues" -d csrf=$CSRF_A -d name=orders)" 303
check "[API] dead-letter queue created" "$(qcode "$TOK_DEV" POST "" -d '{"name": "orders-dlq"}')" 201
check "[API] message sent" "$(qcode "$TOK_DEV" POST /orders/messages -d '{"body": "order-1"}')" 201
R=$(qapi "$TOK_DEV" POST "/orders/receive?max=10")
RECEIPT=$(grep -o '"receipt":"[^"]*"' <<<"$R" | head -1 | cut -d'"' -f4)
check "[API] receive returns the message with a receipt" "$(grep -o '"body":"order-1"' <<<"$R"):$([ -n "$RECEIPT" ] && echo receipt)" '"body":"order-1":receipt'
check "a received message is hidden from other consumers" "$(qapi "$TOK_DEV" POST "/orders/receive?max=10" | grep -c order-1)" 0
check "[API] message deleted with its receipt" "$(qcode "$TOK_DEV" DELETE "/orders/messages/$RECEIPT")" 204
check "deleting twice answers 404" "$(qcode "$TOK_DEV" DELETE "/orders/messages/$RECEIPT")" 404
check "[B->A API] B's token cannot reach A's queue (404)" "$(qcode "$TOK_B" POST /orders/messages -d '{"body": "evil"}')" 404
check "[A] visibility 2 s, 2 receives, then orders-dlq" "$(code_of ja -X POST "$BASE/o/$A/queues/orders/settings" -d csrf=$CSRF_A -d visibility=2 -d retention_hours=24 -d max_receives=2 -d dlq=orders-dlq -d batch_size=10)" 303
qcode "$TOK_DEV" POST /orders/messages -d '{"body": "poison"}' >/dev/null
qapi "$TOK_DEV" POST "/orders/receive" >/dev/null; sleep 3
check "a message not deleted comes back after the visibility timeout" "$(qapi "$TOK_DEV" POST "/orders/receive" | grep -o '"receive_count":2')" '"receive_count":2'
sleep 3
check "after max receives it moves to the dead-letter queue" "$(qapi "$TOK_DEV" POST "/orders/receive" | grep -c poison):$(qapi "$TOK_DEV" POST "/orders-dlq/receive" | grep -c poison)" "0:1"
T0=$(date +%s); (sleep 2; qcode "$TOK_DEV" POST /orders/messages -d '{"body": "late"}' >/dev/null) &
R=$(qapi "$TOK_DEV" POST "/orders/receive?wait=10"); T1=$(date +%s); wait
check "long polling waits for a message and returns it as soon as it arrives" "$(grep -c '"body":"late"' <<<"$R"):$([ $((T1 - T0)) -ge 1 ] && [ $((T1 - T0)) -le 8 ] && echo fast)" "1:fast"
check "[A] queue connected to service files" "$(code_of ja -X POST "$BASE/o/$A/queues/orders/attach" -d csrf=$CSRF_A -d service=shop/files)" 303
QT_COUNT=$(owner_sql "SELECT count(*) FROM api_tokens t JOIN queues q ON q.id = t.queue_id JOIN orgs o ON o.id = t.org_id WHERE o.slug = '$A' AND q.name = 'orders'")
check "the service got a token limited to the queue" "$QT_COUNT:$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND v.slug = 'files' AND s.name IN ('QUEUE_URL', 'QUEUE_TOKEN')")" "1:2"
# The scoped token itself is only stored encrypted; create one directly to test its limits.
SCOPED=pla_e2e$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
owner_sql "INSERT INTO api_tokens (org_id, name, token_hash, role, queue_id) SELECT o.id, 'scoped-probe', sha256(convert_to('$SCOPED', 'UTF8')), 'developer', q.id FROM orgs o JOIN queues q ON q.org_id = o.id WHERE o.slug = '$A' AND q.name = 'orders'" >/dev/null
check "a queue token sends and receives on its queue" "$(qcode "$SCOPED" POST /orders/messages -d '{"body": "scoped"}'):$(qapi "$SCOPED" POST "/orders/receive?max=10" | grep -c scoped)" "201:1"
check "a queue token cannot reach another queue (404)" "$(qcode "$SCOPED" POST /orders-dlq/messages -d '{"body": "x"}')" 404
check "a queue token cannot list queues or deploy (403)" "$(qcode "$SCOPED" GET "" ):$(api_code "$SCOPED" -X POST "$BASE/api/v1/projects/shop/services/web/deploy")" "403:403"
qcode "$TOK_DEV" POST /orders/purge >/dev/null
check "[API] purge empties the queue" "$(owner_sql "SELECT count(*) FROM queue_messages m JOIN queues q ON q.id = m.queue_id JOIN orgs o ON o.id = q.org_id WHERE o.slug = '$A' AND q.name = 'orders'")" 0
# Function trigger: the function retries only the message whose body is "fail".
CONSUMER='export const handler = async (event) => ({ batchItemFailures: event.Records.filter((r) => r.body === "fail").map((r) => ({ itemIdentifier: r.messageId })) });'
PUT_BODY=$(printf '%s' "$CONSUMER" | python -c 'import json,sys; print(json.dumps({"runtime": "nodejs22", "code": sys.stdin.read()}))')
check "[API] consumer function deployed" "$(fn_api PUT consumer -o /dev/null -w '%{http_code}' -d "$PUT_BODY")" 201
check "[A] function trigger set on the queue" "$(code_of ja -X POST "$BASE/o/$A/queues/orders/settings" -d csrf=$CSRF_A -d visibility=2 -d retention_hours=24 -d max_receives=2 -d dlq=orders-dlq -d function=shop/consumer -d batch_size=10 -d trigger_enabled=1)" 303
qcode "$TOK_DEV" POST /orders/messages -d '{"messages": [{"body": "ok-1"}, {"body": "ok-2"}, {"body": "fail"}]}' >/dev/null
check "the function consumed the batch; only the failed item stayed" "$(for _ in $(seq 1 45); do r=$(owner_sql "SELECT string_agg(m.body, ',' ORDER BY m.body) FROM queue_messages m JOIN queues q ON q.id = m.queue_id JOIN orgs o ON o.id = q.org_id WHERE o.slug = '$A' AND q.name IN ('orders', 'orders-dlq') AND m.body IN ('ok-1', 'ok-2', 'fail')"); [ "$r" = fail ] && { echo yes; break; }; sleep 2; done)" yes
check "the failed item ends in the dead-letter queue" "$(for _ in $(seq 1 30); do [ "$(owner_sql "SELECT q.name FROM queue_messages m JOIN queues q ON q.id = m.queue_id JOIN orgs o ON o.id = q.org_id WHERE o.slug = '$A' AND m.body = 'fail'")" = orders-dlq ] && { echo yes; break; }; sleep 2; done)" yes
check "queue invocations recorded with the SQS event source" "$(owner_sql "SELECT count(*) > 0 FROM function_invocations i JOIN functions f ON f.id = i.function_id JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$A' AND f.slug = 'consumer' AND i.trigger = 'queue' AND i.status = 'succeeded'")" t
check "[CLI] platformctl queue send and receive" "$(pctl queue send orders-dlq cli-msg >/dev/null; pctl queue receive orders-dlq --max 10 | grep -c cli-msg)" 1
check "queue requests metered for invoices" "$(owner_sql "SELECT sum(queue_requests) > 0 FROM usage_hourly u JOIN orgs o ON o.id = u.org_id WHERE o.slug = '$A' AND u.kind = 'queue'")" t
if grep -q "/o/$A/queues/orders\"" <<<"$(c ja "$BASE/o/$A/queues")"; then ok "queues area lists the queue"; else bad "queues area misses it"; fi
fn_api DELETE consumer >/dev/null
code_of ja -X POST "$BASE/o/$A/queues/orders/delete" -d csrf=$CSRF_A -d confirm=orders >/dev/null
check "[API] queue deletion needs an admin token (403)" "$(qcode "$TOK_DEV" DELETE /orders-dlq)" 403
code_of ja -X POST "$BASE/o/$A/queues/orders-dlq/delete" -d csrf=$CSRF_A -d confirm=orders-dlq >/dev/null

section "Secrets manager"
vapi() { local t=$1 m=$2 path=$3; shift 3; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -X "$m" "$BASE/api/v1/secrets$path" "$@"; }
vcode() { local t=$1 m=$2 path=$3; shift 3; vapi "$t" "$m" "$path" -o /dev/null -w '%{http_code}' "$@"; }
admin set-plan --slug "$A" --plan pro >/dev/null   # room for one more service
check "[A] secret created in the console" "$(code_of ja -X POST "$BASE/o/$A/secrets" -d csrf=$CSRF_A -d name=stripe-key -d description=payments -d "value=sk-test-v1-$RUN")" 303
V_PAGE=$(c ja "$BASE/o/$A/secrets")
check "the list shows it, never its value" "$(grep -c 'stripe-key' <<<"$V_PAGE" | sed 's/^[1-9][0-9]*$/listed/'):$(grep -c "sk-test-v1" <<<"$V_PAGE")" "listed:0"
check "the value is stored encrypted" "$(owner_sql "SELECT count(*) FROM org_secret_versions WHERE position(convert_to('sk-test-v1', 'UTF8') in value_enc) > 0")" 0
check "[API] secret created" "$(vcode "$TOK_DEV" POST "" -d '{"name": "db-pass", "value": "pw-1"}')" 201
check "[API] the same name twice is refused (409)" "$(vcode "$TOK_DEV" POST "" -d '{"name": "db-pass", "value": "pw-2"}')" 409
check "[B->A API] B cannot read A's secret (404)" "$(vcode "$TOK_B" GET /stripe-key)" 404
check "[CLI] platformctl vault list" "$(pctl vault list | grep -c -e '^stripe-key' -e '^db-pass')" 2
code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=vaultsvc -d image=nginx:1.27-alpine >/dev/null
V_SET=(-d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path=)
check "[A] service variable references the secret" "$(code_of ja -X POST "$BASE/o/$A/p/shop/s/vaultsvc/settings" "${V_SET[@]}" --data-urlencode 'env=STRIPE_KEY=${secret:stripe-key}' -d deploy=1)" 303
check "[A] service live" "$(wait_status ja /o/$A/p/shop/s/vaultsvc)" live
V_CID() { owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'vaultsvc' AND d.status = 'live' ORDER BY d.revision DESC LIMIT 1"; }
V1=$(V_CID)
check "the container receives the secret's value" "$(dx exec "$V1" printenv STRIPE_KEY 2>&1 | tr -d '\r')" "sk-test-v1-$RUN"
check "the secret's page lists the service using it" "$(c ja "$BASE/o/$A/secrets/stripe-key" | grep -c 'shop/vaultsvc' | sed 's/^[1-9][0-9]*$/yes/')" yes
check "[API] new version redeploys the service" "$(vapi "$TOK_DEV" PUT /stripe-key -d "{\"value\": \"sk-test-v2-$RUN\"}" | grep -o '"services_redeployed":1')" '"services_redeployed":1'
V2=; for _ in $(seq 1 60); do V2=$(V_CID); [ -n "$V2" ] && [ "$V2" != "$V1" ] && break; sleep 2; done
check "the redeployed container has the new version" "$(dx exec "$V2" printenv STRIPE_KEY 2>&1 | tr -d '\r')" "sk-test-v2-$RUN"
check "[API] read the current value and an older version" "$(vapi "$TOK_DEV" GET /stripe-key | grep -o "sk-test-v2-$RUN"):$(vapi "$TOK_DEV" GET '/stripe-key?version=1' | grep -o "sk-test-v1-$RUN")" "sk-test-v2-$RUN:sk-test-v1-$RUN"
check "[A] reveal in the console shows the value" "$(c ja -X POST "$BASE/o/$A/secrets/stripe-key/reveal" -d csrf=$CSRF_A -d version=2 | grep -c "sk-test-v2-$RUN" | sed 's/^[1-9][0-9]*$/yes/')" yes
check "reads, creation and rotation are audited" "$(owner_sql "SELECT count(DISTINCT action) FROM audit_events WHERE target = 'secret:stripe-key' AND action IN ('secret.created', 'secret.rotated', 'secret.read') AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" 3
check "a viewer of the API cannot read values (403)" "$(vcode "$TOK_VIEW" GET /stripe-key)" 403
check "[API] a secret in use cannot be deleted (409)" "$(vcode "$TOK_DEV" DELETE /stripe-key)" 409
code_of ja -X POST "$BASE/o/$A/p/shop/s/vaultsvc/settings" "${V_SET[@]}" --data-urlencode 'env=STRIPE_KEY=${secret:no-such-secret}' -d deploy=1 >/dev/null
st=; for _ in $(seq 1 30); do st=$(owner_sql "SELECT d.status || ':' || (d.message LIKE '%no-such-secret%does not exist%') FROM deployments d JOIN services s ON s.id = d.service_id WHERE s.slug = 'vaultsvc' AND d.org_id = (SELECT id FROM orgs WHERE slug = '$A') ORDER BY d.revision DESC LIMIT 1"); [ "${st%%:*}" = failed ] && break; sleep 2; done
check "a missing secret fails the deployment with a clear message" "$st" "failed:true"
code_of ja -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/vaultsvc >/dev/null
check "[A] unused secret deleted in the console" "$(code_of ja -X POST "$BASE/o/$A/secrets/stripe-key/delete" -d csrf=$CSRF_A -d confirm=stripe-key)" 303
check "[API] secret deleted" "$(vcode "$TOK_DEV" DELETE /db-pass)" 204
admin set-plan --slug "$A" --plan starter >/dev/null

section "Outgoing email (verified domains, DKIM)"
eapi() { local t=$1 m=$2 path=$3; shift 3; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -X "$m" "$BASE/api/v1/email$path" "$@"; }
ecode() { local t=$1 m=$2 path=$3; shift 3; eapi "$t" "$m" "$path" -o /dev/null -w '%{http_code}' "$@"; }
mkdir -p "$TMP/oidc"; (cd .. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$TMP/oidc/mockoidc" ./scripts/mockoidc) || bad "mock DNS server did not build"
docker rm -f e2e-dns >/dev/null 2>&1
MSYS_NO_PATHCONV=1 docker run -d --name e2e-dns --network platform-core -p 8098:8098 -v "$(cygpath -w "$TMP/oidc"):/m:ro" alpine:3.20 /m/mockoidc -addr :8098 -dns :53 -smtp :1025 >/dev/null
dnsset() { curl -sS -X POST -G "http://127.0.0.1:8098/dns/set" --data-urlencode "name=$1" --data-urlencode "txt=$2"; }
dnsrec() { curl -sS -X POST -G "http://127.0.0.1:8098/dns/set" --data-urlencode "name=$1" --data-urlencode "$2=$3"; }
sleep 1
# Direct delivery: every domain's mail server is Mailpit, except bounce.e2e.test (the mock refuses nobody@)
ipof() { docker inspect -f '{{(index .NetworkSettings.Networks "platform-core").IPAddress}}' "$1"; }
dnsrec '*' mx mx.e2e.test; dnsrec mx.e2e.test a "$(ipof platform-dev-mailpit-1)"
dnsrec bounce.e2e.test mx smtp.bounce.e2e.test; dnsrec smtp.bounce.e2e.test a "$(ipof e2e-dns)"
ED=$A-mail.example.test
ESEND() { printf '{"from": "Shop <hello@%s>", "to": ["%s"], "subject": "%s", "text": "Thanks for your order.", "html": "<p>Thanks!</p>"}' "$ED" "$1" "$2"; }
check "[A] invalid domain refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/email/domains" -d csrf=$CSRF_A -d domain=not_a_domain | grep -c 'err=')" 1
check "[A] sending domain added in the console" "$(code_of ja -X POST "$BASE/o/$A/email/domains" -d csrf=$CSRF_A -d "domain=$ED")" 303
check "its page lists the ownership and DKIM records" "$(c ja "$BASE/o/$A/email/d/$ED" | grep -o -e "_platform-verify.$ED" -e "_domainkey.$ED" | sort -u | wc -l | tr -d ' ')" 2
check "the DKIM private key is stored encrypted" "$(owner_sql "SELECT count(*) FROM email_domains WHERE domain = '$ED' AND position(convert_to('PRIVATE KEY', 'UTF8') in dkim_key_enc) = 0")" 1
check "[API] sending before verification is refused (422)" "$(ecode "$TOK_DEV" POST /send -d "$(ESEND buyer-$RUN@example.org early)")" 422
IFS='|' read -r ETOK ESEL ETXT <<<"$(owner_sql "SELECT verify_token || '|' || dkim_selector || '|' || dkim_txt FROM email_domains WHERE domain = '$ED'")"
dnsset "_platform-verify.$ED" "platform-verify=$ETOK"; dnsset "$ESEL._domainkey.$ED" "$ETXT"
check "[A] check now" "$(code_of ja -X POST "$BASE/o/$A/email/d/$ED/check" -d csrf=$CSRF_A)" 303
st=; for _ in $(seq 1 30); do st=$(owner_sql "SELECT status FROM email_domains WHERE domain = '$ED'"); [ "$st" = verified ] && break; sleep 2; done
check "domain verified through DNS" "$st" verified
EID=$(eapi "$TOK_DEV" POST /send -d "$(ESEND buyer-$RUN@example.org "Order $RUN")" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
check "[API] email queued (202 with an id)" "$([ -n "$EID" ] && echo yes)" yes
EM_ID=; for _ in $(seq 1 20); do EM_ID=$(curl -s -G "http://localhost:8025/api/v1/search" --data-urlencode "query=to:buyer-$RUN@example.org" | grep -o '"ID":"[^"]*"' | head -1 | cut -d'"' -f4); [ -n "$EM_ID" ] && break; sleep 1; done
EM=$(curl -s "http://localhost:8025/api/v1/message/$EM_ID/raw")
check "delivered with a DKIM signature of the domain" "$(grep -o "d=$ED; s=$ESEL" <<<"$EM" | head -1)" "d=$ED; s=$ESEL"
check "text and HTML parts" "$(grep -c -e 'Thanks for your order.' -e '<p>Thanks!</p>' <<<"$EM" | sed 's/^[1-9][0-9]*$/yes/')" yes
st=; for _ in $(seq 1 10); do st=$(eapi "$TOK_DEV" GET "/messages/$EID" | grep -o '"status":"[a-z]*","subject"' | cut -d'"' -f4); [ "$st" = sent ] && break; sleep 1; done
check "[API] message status is sent" "$st" sent
check "the body is deleted once sent" "$(owner_sql "SELECT body_text = '' AND body_html = '' FROM email_messages WHERE id = '$EID'")" t
check "the sending log shows it" "$(c ja "$BASE/o/$A/email" | grep -c "Order $RUN" | sed 's/^[1-9][0-9]*$/yes/')" yes
check "[B->A API] B cannot send from A's domain (422)" "$(ecode "$TOK_B" POST /send -d "$(ESEND victim@example.org spoof)")" 422
check "[API] a header injection in the subject is refused (422)" "$(ecode "$TOK_DEV" POST /send -d "$(ESEND x@example.org 'a\r\nBcc: evil@example.org')")" 422
ESCOPED="pla_e2e_email_only_$RUN"
owner_sql "INSERT INTO api_tokens (org_id, name, token_hash, role, email_only) SELECT id, 'email-probe', sha256(convert_to('$ESCOPED', 'UTF8')), 'developer', true FROM orgs WHERE slug = '$A'" >/dev/null
check "an email-only token sends (202)" "$(ecode "$ESCOPED" POST /send -d "$(ESEND buyer2-$RUN@example.org scoped)")" 202
check "an email-only token cannot do anything else (403)" "$(ecode "$ESCOPED" GET /domains):$(curl -sS -o /dev/null -w '%{http_code}' --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $ESCOPED" "$BASE/api/v1/queues")" "403:403"
check "[A] connect email to a service" "$(code_of ja -X POST "$BASE/o/$A/email/attach" -d csrf=$CSRF_A -d service=shop/files)" 303
check "the service got EMAIL_API_URL and EMAIL_TOKEN" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id WHERE v.slug = 'files' AND s.org_id = (SELECT id FROM orgs WHERE slug = '$A') AND s.name IN ('EMAIL_API_URL', 'EMAIL_TOKEN')")" 2
check "[API] each recipient's result is reported (delivered)" "$(eapi "$TOK_DEV" GET "/messages/$EID" | grep -o '"status":"delivered"' | head -1)" '"status":"delivered"'
check "the SPF record to publish is shown" "$(c ja "$BASE/o/$A/email/d/$ED" | grep -c 'v=spf1 include:spf.example.test ~all')" 1
# Delivery webhook first, so the results below are reported
HOOK=$(eapi "$TOK_DEV" PUT /webhook -d '{"url": "http://e2e-dns:8098/hook"}')
HSEC=$(grep -o '"secret":"whsec_[^"]*"' <<<"$HOOK" | cut -d'"' -f4)
check "[API] delivery webhook saved, its signing secret given once" "$([ -n "$HSEC" ] && echo yes):$(eapi "$TOK_DEV" GET /webhook | grep -c whsec_)" "yes:0"
check "[API] the webhook secret is stored encrypted" "$(owner_sql "SELECT count(*) FROM email_webhooks WHERE position(convert_to('$HSEC', 'UTF8') in secret_enc) > 0")" 0
# Hard bounce: the receiving server refuses one recipient for good
BID=$(eapi "$TOK_DEV" POST /send -d "$(printf '{"from": "hello@%s", "to": ["nobody-%s@bounce.e2e.test", "ok-%s@bounce.e2e.test"], "subject": "Bounce %s", "text": "hi"}' "$ED" "$RUN" "$RUN" "$RUN")" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
st=; for _ in $(seq 1 30); do st=$(eapi "$TOK_DEV" GET "/messages/$BID" | grep -o '"status":"[a-z]*","subject"' | cut -d'"' -f4); [ "$st" = partial ] && break; sleep 1; done
check "one recipient bounced, the other delivered: the message is partly sent" "$st" partial
check "the bounce keeps the receiving server's answer" "$(eapi "$TOK_DEV" GET "/messages/$BID" | grep -c '550 5.1.1')" 1
check "a hard bounce puts the address on the suppression list" "$(eapi "$TOK_DEV" GET /suppressions | grep -c "\"nobody-$RUN@bounce.e2e.test\",\"reason\":\"bounce\"")" 1
SID=$(eapi "$TOK_DEV" POST /send -d "$(printf '{"from": "hello@%s", "to": ["nobody-%s@bounce.e2e.test"], "subject": "Again %s", "text": "hi"}' "$ED" "$RUN" "$RUN")" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
st=; for _ in $(seq 1 20); do st=$(eapi "$TOK_DEV" GET "/messages/$SID" | grep -o '"status":"[a-z]*","subject"' | cut -d'"' -f4); [ "$st" = failed ] && break; sleep 1; done
check "a suppressed address is not sent to again" "$st:$(eapi "$TOK_DEV" GET "/messages/$SID" | grep -c '"status":"suppressed"')" "failed:1"
check "the webhook received the results, signed with the secret" "$(for _ in $(seq 1 30); do L=$(curl -s http://127.0.0.1:8098/hook/log); grep -q 'email.bounced' <<<"$L" && grep -q 'email.delivered' <<<"$L" && grep -q 'email.suppressed' <<<"$L" && { echo yes; break; }; sleep 2; done)" yes
check "the webhook signature verifies (HMAC-SHA256 of the body)" "$(curl -s http://127.0.0.1:8098/hook/log | grep 'email.bounced' | head -1 | python -c 'import sys,hmac,hashlib; sig,body=sys.stdin.read().strip().split(" ",1); print(hmac.compare_digest(sig, "sha256="+hmac.new(sys.argv[1].encode(), body.encode(), hashlib.sha256).hexdigest()))' "$HSEC" | tr -d '\r')" True
check "[B->A] B does not see A's suppression list" "$(eapi "$TOK_B" GET /suppressions | grep -c "nobody-$RUN")" 0
check "[A] the console lists the suppressed address" "$(c ja "$BASE/o/$A/email" | grep -q "<code>nobody-$RUN@bounce.e2e.test</code>" && echo yes)" yes
check "[API] suppression removed" "$(ecode "$TOK_DEV" DELETE "/suppressions/nobody-$RUN@bounce.e2e.test"):$(eapi "$TOK_DEV" GET /suppressions | grep -c "nobody-$RUN")" "204:0"
# Attachments
ATT=$(printf '%%PDF-1.4 invoice %s' "$RUN" | base64 | tr -d '\n')
AID=$(eapi "$TOK_DEV" POST /send -d "$(printf '{"from": "hello@%s", "to": ["att-%s@example.org"], "subject": "Invoice %s", "text": "Attached.", "attachments": [{"filename": "invoice-%s.pdf", "content_type": "application/pdf", "content": "%s"}]}' "$ED" "$RUN" "$RUN" "$RUN" "$ATT")" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
AM=; for _ in $(seq 1 20); do AM_ID=$(curl -s -G "http://localhost:8025/api/v1/search" --data-urlencode "query=to:att-$RUN@example.org" | grep -o '"ID":"[^"]*"' | head -1 | cut -d'"' -f4); [ -n "$AM_ID" ] && { AM=$(curl -s "http://localhost:8025/api/v1/message/$AM_ID"); break; }; sleep 1; done
check "[API] an attachment arrives with the message" "$(grep -o "\"FileName\":\"invoice-$RUN.pdf\"" <<<"$AM" | head -1)" "\"FileName\":\"invoice-$RUN.pdf\""
check "[API] executable attachments are refused (422)" "$(ecode "$TOK_DEV" POST /send -d "$(printf '{"from": "hello@%s", "to": ["x@example.org"], "subject": "x", "text": "x", "attachments": [{"filename": "run.exe", "content": "TVo="}]}' "$ED")")" 422
# SMTP submission (port 465 through the proxy; plain SMTP in development)
printf 'From: Shop <hello@%s>\r\nTo: smtp-%s@example.org\r\nBcc: hidden-%s@example.org\r\nSubject: SMTP %s\r\n\r\nSent over SMTP.\r\n' "$ED" "$RUN" "$RUN" "$RUN" > "$TMP/smtp.eml"
smtp_send() { curl -sS --max-time 20 "smtp://127.0.0.1:1465" --mail-from "hello@$ED" "$@" --upload-file "$(cygpath -w "$TMP/smtp.eml")" 2>&1; }
check "[SMTP] a message is accepted with the email token" "$(smtp_send --user "token:$ESCOPED" --mail-rcpt "smtp-$RUN@example.org" --mail-rcpt "hidden-$RUN@example.org" >/dev/null && echo yes)" yes
SM=; for _ in $(seq 1 20); do SM_ID=$(curl -s -G "http://localhost:8025/api/v1/search" --data-urlencode "query=to:smtp-$RUN@example.org" | grep -o '"ID":"[^"]*"' | head -1 | cut -d'"' -f4); [ -n "$SM_ID" ] && { SM=$(curl -s "http://localhost:8025/api/v1/message/$SM_ID/raw"); break; }; sleep 1; done
check "[SMTP] delivered, signed with the domain's DKIM key; the Bcc recipient got it too" "$(grep -c "d=$ED; s=$ESEL" <<<"$SM" | sed 's/^[1-9][0-9]*$/yes/'):$(curl -s -G "http://localhost:8025/api/v1/search" --data-urlencode "query=hidden-$RUN" | grep -c '"ID"' | sed 's/^[1-9][0-9]*$/yes/')" "yes:yes"
check "[SMTP] a wrong token is refused (curl: login denied)" "$(smtp_send --user "token:pla_wrong" --mail-rcpt "x-$RUN@example.org" >/dev/null; echo $?)" 67
printf 'From: someone@unverified-%s.example\r\nTo: x@example.org\r\nSubject: spoof\r\n\r\nx\r\n' "$RUN" > "$TMP/smtp.eml"
check "[SMTP] a From address outside the verified domains is refused" "$(smtp_send --user "token:$ESCOPED" --mail-rcpt "x-$RUN@example.org" >/dev/null && echo accepted || echo refused)" refused
check "[A] connecting a service also gives SMTP settings" "$(owner_sql "SELECT count(*) FROM secrets s JOIN services v ON v.id = s.service_id WHERE v.slug = 'files' AND s.org_id = (SELECT id FROM orgs WHERE slug = '$A') AND s.name IN ('SMTP_HOST', 'SMTP_PORT', 'SMTP_USERNAME', 'SMTP_PASSWORD')")" 4
check "[API] delivery webhook removed" "$(ecode "$TOK_DEV" DELETE /webhook)" 204
EMAX=$(owner_sql "SELECT p.max_emails_day FROM plans p JOIN orgs o ON o.plan_id = p.id WHERE o.slug = '$A'")
owner_sql "UPDATE plans SET max_emails_day = 2 WHERE id = (SELECT plan_id FROM orgs WHERE slug = '$A')" >/dev/null
check "[API] the daily limit answers 429" "$(ecode "$TOK_DEV" POST /send -d "$(ESEND buyer3-$RUN@example.org over)")" 429
owner_sql "UPDATE plans SET max_emails_day = $EMAX WHERE id = (SELECT plan_id FROM orgs WHERE slug = '$A')" >/dev/null
check "sent emails are metered" "$(for _ in $(seq 1 10); do n=$(owner_sql "SELECT coalesce(sum(emails), 0) FROM usage_hourly WHERE kind = 'email' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')"); [ "${n:-0}" -ge 2 ] && { echo yes; break; }; sleep 1; done)" yes
check "[A] domain deleted" "$(code_of ja -X POST "$BASE/o/$A/email/d/$ED/delete" -d csrf=$CSRF_A -d "confirm=$ED")" 303
owner_sql "DELETE FROM api_tokens WHERE name = 'email-probe'" >/dev/null
docker rm -f e2e-dns >/dev/null 2>&1

section "Observability: Prometheus API, custom metrics, Grafana, analytics"
ORG_A=$(owner_sql "SELECT id FROM orgs WHERE slug = '$A'"); ORG_B=$(owner_sql "SELECT id FROM orgs WHERE slug = '$B'")
prom() { curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $1" -G "$BASE/api/v1/prometheus/api/v1/$2" "${@:3}"; }
P=$(prom "$TOK_DEV" query --data-urlencode 'query=count by (org_id) (platform_container_memory_bytes)')
check "[Prometheus API] answers with A's series only" "$(grep -o '"org_id":"[^"]*"' <<<"$P" | sort -u | tr -d '\n')" "\"org_id\":\"$ORG_A\""
P=$(prom "$TOK_DEV" query --data-urlencode 'query=count by (org_id) (platform_container_memory_bytes)' --data-urlencode "extra_label=org_id=$ORG_B")
check "[Prometheus API] a forged extra_label cannot reach B" "$(grep -c "$ORG_B" <<<"$P")" 0
check "[Prometheus API] series and labels endpoints work" "$(prom "$TOK_DEV" labels | grep -c '"service_id"'):$(prom "$TOK_DEV" label/service_id/values | grep -c '"status":"success"')" "1:1"
check "[Prometheus API] other paths refused (404)" "$(curl -sS -o /dev/null -w '%{http_code}' --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/prometheus/api/v1/admin/tsdb/delete_series")" 404
check "[Prometheus API] needs a token (401)" "$(curl -sS -o /dev/null -w '%{http_code}' --resolve "console.localhost:$PORT:127.0.0.1" "$BASE/api/v1/prometheus/api/v1/query?query=up")" 401

alarm_redir() { c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/monitoring/alarms" -d csrf=$CSRF_A "$@"; }
check "[alarms] invalid PromQL refused" "$(alarm_redir -d name=bad --data-urlencode 'query=max(' -d op=%3E -d threshold=1 -d for_minutes=0 | grep -c 'err=')" 1
check "[alarms] a range query is refused (needs a number or instant vector)" "$(alarm_redir -d name=range --data-urlencode 'query=platform_container_memory_bytes[5m]' -d op=%3E -d threshold=1 -d for_minutes=0 | grep -c 'instant')" 1
check "[A] alarm created" "$(code_of ja -X POST "$BASE/o/$A/monitoring/alarms" -d csrf=$CSRF_A -d "name=Memory above 1 byte" --data-urlencode 'query=max(platform_container_memory_bytes)' -d op=%3E -d threshold=1 -d for_minutes=0)" 303
owner_sql "SELECT pg_notify('deployments', 'alarms')" >/dev/null
AL=; for _ in $(seq 1 45); do AL=$(owner_sql "SELECT r.state || ':' || (SELECT count(*) FROM alert_incidents i WHERE i.alarm_id = r.id AND i.status = 'firing' AND i.kind = 'metric_alarm') FROM alarm_rules r WHERE r.org_id = '$ORG_A' AND r.name = 'Memory above 1 byte'"); [ "$AL" = "firing:1" ] && break; sleep 2; done
check "[alarms] the breach fires an incident" "$AL" "firing:1"
check "[alarms] the incident and the current value show on the Monitoring page" "$(c ja "$BASE/o/$A/monitoring" | grep -c -e 'Memory above 1 byte:' -e '<span class="error-text">firing</span>' | sed 's/^[1-9][0-9]*$/yes/')" yes
check "[B->A] B's API does not list A's alarms" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_B" "$BASE/api/v1/alarms" | grep -c 'Memory above 1 byte')" 0
ALID=$(owner_sql "SELECT id FROM alarm_rules WHERE org_id = '$ORG_A' AND name = 'Memory above 1 byte'")
code_of ja -X POST "$BASE/o/$A/monitoring/alarms/$ALID" -d csrf=$CSRF_A -d action=pause >/dev/null
check "[alarms] pausing resolves its incident" "$(owner_sql "SELECT count(*) FROM alert_incidents WHERE alarm_id = '$ALID' AND status = 'firing'"):$(owner_sql "SELECT enabled FROM alarm_rules WHERE id = '$ALID'")" "0:f"
check "[API] alarm created and listed" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_DEV" -H 'Content-Type: application/json' -d '{"name":"api-alarm","query":"sum(rate(platform_container_cpu_seconds_total[5m]))","op":">","threshold":100,"for_minutes":5}' "$BASE/api/v1/alarms"):$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/alarms" | grep -c '"Name":"api-alarm"')" "201:1"
check "[API] alarm deleted" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/alarms/api-alarm")" 204
code_of ja -X POST "$BASE/o/$A/monitoring/alarms/$ALID" -d csrf=$CSRF_A -d action=delete >/dev/null
admin set-plan --slug "$A" --plan pro >/dev/null
mkdir -p "$TMP/metricsapp"; printf '%s\n' 'FROM nginx:1.27-alpine' 'COPY metrics /usr/share/nginx/html/metrics' > "$TMP/metricsapp/Dockerfile"
printf '%s\n' '# TYPE app_orders_total counter' "app_orders_total{org_id=\"$ORG_B\",shop=\"main\"} 42" 'app_queue_depth 7' > "$TMP/metricsapp/metrics"
docker build -q -t "$REG/$A/metricsapp:1" "$(cygpath -w "$TMP/metricsapp")" >/dev/null
TOK_MX=$(new_token ja "$A" "$CSRF_A"); echo "$TOK_MX" | dk login $REG -u "$A" --password-stdin >/dev/null; dk push "$REG/$A/metricsapp:1" >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=metrics -d image=nginx:1.27-alpine >/dev/null
check "[A] service with a metrics path deployed" "$(code_of ja -X POST "$BASE/o/$A/p/shop/s/metrics/settings" -d csrf=$CSRF_A -d "image=$REG/$A/metricsapp:1" -d port=80 -d health_path=%2F -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env= -d metrics_path=%2Fmetrics -d deploy=1):$(wait_status ja /o/$A/p/shop/s/metrics)" "303:live"
MQ=; for _ in $(seq 1 40); do MQ=$(prom "$TOK_DEV" query --data-urlencode 'query=app_orders_total'); grep -q '"42"' <<<"$MQ" && break; sleep 3; done
check "the app's own metrics are scraped and queryable" "$(grep -o '"value":\[[0-9.]*,"42"\]' <<<"$MQ" | wc -l | tr -d ' ')" 1
check "scraped series carry the service and A's organization" "$(grep -o '"service":"metrics"' <<<"$MQ" | head -1):$(grep -o '"org_id":"[^"]*"' <<<"$MQ" | sort -u | tr -d '\n')" "\"service\":\"metrics\":\"org_id\":\"$ORG_A\""
check "an app cannot write series into B (org_id label dropped)" "$(prom "$TOK_B" query --data-urlencode 'query=app_orders_total' | grep -c '"42"')" 0
code_of ja -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/metrics >/dev/null

for i in 1 2 3 4 5; do curl -sS -o /dev/null --resolve "$HOST_A:$PORT:127.0.0.1" -A "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari/604.1" -e "https://news.example.org/x" "http://$HOST_A:$PORT/analytics-$RUN?secret=s3cr3t"; done
curl -sS -o /dev/null --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/missing-$RUN"
AN=; for _ in $(seq 1 30); do AN=$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/projects/shop/services/web/analytics?period=1h"); grep -q "/analytics-$RUN" <<<"$AN" && break; sleep 2; done
check "[analytics] each request is recorded at the proxy" "$(grep -o "\"Key\":\"/analytics-$RUN\",\"Count\":5" <<<"$AN")" "\"Key\":\"/analytics-$RUN\",\"Count\":5"
check "[analytics] referrer, browser and device are classified" "$(grep -c '"news.example.org"' <<<"$AN"):$(grep -c '"Key":"iOS"' <<<"$AN"):$(grep -c '"Key":"mobile"' <<<"$AN")" "1:1:1"
vlq() { dx run --rm --network platform-core curlimages/curl:8.11.1 -s "http://victorialogs:9428/select/logsql/query" --data-urlencode "query=$1"; }
check "[analytics] query strings and IP addresses are not stored" "$(vlq "an_kind:access s3cr3t" | wc -l | tr -d ' '):$(vlq "an_kind:access" | grep -c 'remote_ip\|client_ip')" "0:0"
check "[analytics] latency percentiles and a 404 counted" "$(grep -o '"P95":[0-9]*' <<<"$AN" | wc -l | tr -d ' '):$(grep -c '"Key":"404"' <<<"$AN")" "1:1"
check "[B->A] B cannot read A's analytics (404)" "$(code_of jb "$BASE/o/$A/analytics")" 404
PAGE=$(c ja "$BASE/o/$A/analytics?service=shop%2Fweb&period=1h")
check "[A] the Analytics page shows the traffic chart and top paths" "$(grep -c '<svg class="chart"' <<<"$PAGE"):$(grep -c "/analytics-$RUN" <<<"$PAGE" | sed 's/^[1-9][0-9]*$/yes/')" "1:yes"
check "page-view script served" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}:%{content_type}' "$BASE/a/s.js")" "200:text/javascript; charset=utf-8"
check "[A] page-view script turned on" "$(code_of ja -X POST "$BASE/o/$A/analytics/script" -d csrf=$CSRF_A -d service=shop/web -d on=1)" 303
AKEY=$(owner_sql "SELECT analytics_key FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'web' AND s.preview_of IS NULL")
beacon() { curl -sS -o /dev/null -w '%{http_code}' --resolve "console.localhost:$PORT:127.0.0.1" -A "Mozilla/5.0 (Windows NT 10.0) Chrome/130.0 Safari/537.36" -H 'Content-Type: text/plain' -d "$1" "$BASE/a/e"; }
check "events accepted (204)" "$(beacon "{\"k\":\"$AKEY\",\"n\":\"pageview\",\"u\":\"http://$HOST_A/pricing-$RUN?utm_source=newsletter\",\"r\":\"https://www.example.org/\",\"w\":1400,\"s\":\"sess$RUN\"}"):$(beacon "{\"k\":\"$AKEY\",\"n\":\"signup\",\"u\":\"http://$HOST_A/pricing-$RUN\",\"s\":\"sess$RUN\",\"p\":{\"plan\":\"pro\"}}")" "204:204"
beacon "{\"k\":\"$AKEY\",\"n\":\"pageview\",\"u\":\"https://evil.example/forged-$RUN\",\"s\":\"evil$RUN\"}" >/dev/null
AN=; for _ in $(seq 1 20); do AN=$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/projects/shop/services/web/analytics?period=1h"); grep -q "pricing-$RUN" <<<"$AN" && break; sleep 2; done
check "[behavior] page views, campaigns and custom events reported" "$(grep -c "\"Key\":\"/pricing-$RUN\"" <<<"$AN"):$(grep -c '"Key":"newsletter"' <<<"$AN"):$(grep -c '"Key":"signup"' <<<"$AN")" "1:1:1"
check "[behavior] events from another site are ignored" "$(grep -c "forged-$RUN" <<<"$AN")" 0
check "[behavior] sessions and bounce rate computed" "$(grep -o '"Sessions":[1-9][0-9]*' <<<"$AN" | wc -l | tr -d ' '):$(grep -o '"BounceRate":[0-9]*' <<<"$AN" | wc -l | tr -d ' ')" "1:1"
code_of ja -X POST "$BASE/o/$A/analytics/script" -d csrf=$CSRF_A -d service=shop/web -d on=0 >/dev/null

GPAGE=$(c ja -X POST "$BASE/o/$A/monitoring/grafana" -d csrf=$CSRF_A -d project=shop)
GPW=$(sed -n 's/.*<pre class="cmd">\([a-z0-9_-]*\)<\/pre>.*/\1/p' <<<"$GPAGE" | head -1)
check "[A] managed Grafana created (admin password shown once)" "$([ ${#GPW} -ge 12 ] && echo yes)" yes
check "Grafana has a read-only token, its password and a persistent folder" "$(owner_sql "SELECT count(*) FROM secrets x JOIN services s ON s.id = x.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'grafana' AND x.name IN ('GRAFANA_PROM_TOKEN', 'GF_SECURITY_ADMIN_PASSWORD')"):$(owner_sql "SELECT volume_path FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'grafana'"):$(owner_sql "SELECT role FROM api_tokens t JOIN orgs o ON o.id = t.org_id WHERE o.slug = '$A' AND t.name = 'grafana in shop'")" "2:/var/lib/grafana:viewer"
ACT=$(c ja "$BASE/o/$A/activity.json")
check "[activity] the Grafana build shows as in progress" "$(grep -o '"kind":"build","label":"shop/grafana","state":"[a-z]*"' <<<"$ACT" | sed 's/.*"state":"//;s/"//' | grep -c -e queued -e building)" 1
check "[activity] the header has the in-progress indicator" "$(c ja "$BASE/o/$A/compute" | grep -c 'id="activity" data-url="/o/'"$A"'/activity.json"')" 1
check "[B->A] B cannot read A's activity (404)" "$(code_of jb "$BASE/o/$A/activity.json")" 404
GH=grafana-shop-$A.apps.localhost
check "Grafana built and live" "$(for _ in $(seq 1 100); do st=$(owner_sql "SELECT d.status FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'grafana' ORDER BY d.revision DESC LIMIT 1"); [ "$st" = live ] || [ "$st" = failed ] && { echo "$st"; break; }; sleep 4; done)" live
check "Grafana signs in with the admin password" "$(curl -sS -o /dev/null -w '%{http_code}' --resolve "$GH:$PORT:127.0.0.1" -H 'Content-Type: application/json' -d "{\"user\":\"admin\",\"password\":\"$GPW\"}" "http://$GH:$PORT/login")" 200
check "the platform data source and dashboard are provisioned" "$(curl -sS --resolve "$GH:$PORT:127.0.0.1" -u "admin:$GPW" "http://$GH:$PORT/api/datasources/uid/platform" | grep -o '"type":"prometheus"'):$(curl -sS --resolve "$GH:$PORT:127.0.0.1" -u "admin:$GPW" "http://$GH:$PORT/api/dashboards/uid/platform-services" | grep -o '"title":"Services"' | head -1)" '"type":"prometheus":"title":"Services"'
check "[activity] nothing in progress once Grafana is live" "$(c ja "$BASE/o/$A/activity.json" | grep -c 'shop/grafana')" 0
code_of ja -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/grafana >/dev/null
owner_sql "DELETE FROM api_tokens WHERE name = 'grafana in shop' AND org_id = '$ORG_A'" >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "Single sign-on (OIDC)"
mkdir -p "$TMP/oidc"
(cd .. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$TMP/oidc/mockoidc" ./scripts/mockoidc) || bad "mock OIDC provider did not build"
docker rm -f e2e-oidc >/dev/null 2>&1
MSYS_NO_PATHCONV=1 docker run -d --name e2e-oidc -p 8097:8097 -v "$(cygpath -w "$TMP/oidc"):/m:ro" alpine:3.20 /m/mockoidc -addr :8097 -issuer http://host.docker.internal:8097 >/dev/null
# wait until the provider answers, from the host and from the platform's containers
for _ in $(seq 1 30); do curl -s -o /dev/null http://127.0.0.1:8097/.well-known/openid-configuration && MSYS_NO_PATHCONV=1 docker exec platform-dev-api-1 wget -qO /dev/null http://host.docker.internal:8097/.well-known/openid-configuration 2>/dev/null && break; sleep 1; done
IDP=http://host.docker.internal:8097
for _ in $(seq 1 20); do curl -s --resolve host.docker.internal:8097:127.0.0.1 -o /dev/null "$IDP/jwks" && break; sleep 0.5; done
SSO_DOM=sso-$A.test
sso_save() { code_of ja -X POST "$BASE/o/$A/sso" -d csrf=$CSRF_A --data-urlencode "issuer=$IDP" -d client_id=platform-e2e -d client_secret=e2e-client-secret \
  -d "domains=$SSO_DOM" -d default_role=viewer -d admin_groups=platform-admins -d enabled=1 "$@"; }
check "[A] single sign-on configured" "$(sso_save)" 303
check "SSO settings stored, secret encrypted" "$(owner_sql "SELECT enabled || ':' || (client_secret_enc IS NOT NULL) || ':' || (position(convert_to('e2e-client-secret', 'UTF8') in client_secret_enc) = 0) FROM org_sso c JOIN orgs o ON o.id = c.org_id WHERE o.slug = '$A'")" "true:true:true"
check "unreachable provider refused when saving" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/sso" -d csrf=$CSRF_A -d issuer=http://host.docker.internal:8096 -d client_id=x -d domains=other-$A.test -d default_role=viewer -d enabled=1 | grep -c 'err=')" 1
check "[B] cannot claim A's domain" "$(c jb -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$B/sso" -d csrf=$CSRF_B --data-urlencode "issuer=$IDP" -d client_id=platform-e2e -d "domains=$SSO_DOM" -d default_role=viewer | grep -c 'another+organization')" 1
check "[B->A] B cannot change A's SSO (404)" "$(code_of jb -X POST "$BASE/o/$A/sso" -d csrf=$CSRF_B --data-urlencode "issuer=$IDP" -d client_id=evil -d default_role=admin -d enabled=1)" 404
# sso_login <jar> <email> [extra mock params]: follows the whole flow, prints the final redirect
sso_login() {
  local jar=$1 email=$2 extra=${3:-} auth cb
  auth=$(c "$jar" -o /dev/null -w '%{redirect_url}' -X POST "$BASE/sso/start" --data-urlencode "who=$email")
  [ -n "$auth" ] || { echo "no-redirect"; return; }
  cb=$(curl -s --resolve host.docker.internal:8097:127.0.0.1 -o /dev/null -w '%{redirect_url}' "$auth$extra")
  echo "$cb" > "$TMP/sso-last-callback"
  c "$jar" -o /dev/null -w '%{http_code} %{redirect_url}' "$cb"
}
check "SSO sign-in of a new admin-group member lands in the console" "$(sso_login js1 "alice@$SSO_DOM" "&mock_groups=platform-admins&mock_name=Alice")" "303 $BASE/o/$A"
check "the SSO session opens the console without a two-step prompt" "$(code_of js1 "$BASE/o/$A")" 200
check "account created on first sign-in, role from the provider's group" "$(owner_sql "SELECT u.sso_only || ':' || m.role FROM users u JOIN memberships m ON m.user_id = u.id JOIN orgs o ON o.id = m.org_id WHERE u.email = 'alice@$SSO_DOM' AND o.slug = '$A'")" "true:admin"
check "a replayed sign-in link is refused" "$(c js9 -o /dev/null -w '%{http_code}' "$(cat "$TMP/sso-last-callback")")" 403
check "member without a mapped group gets the default role" "$(sso_login js2 "bob@$SSO_DOM" >/dev/null; owner_sql "SELECT m.role FROM users u JOIN memberships m ON m.user_id = u.id JOIN orgs o ON o.id = m.org_id WHERE u.email = 'bob@$SSO_DOM' AND o.slug = '$A'")" viewer
check "roles follow the provider's groups at the next sign-in" "$(sso_login js2 "bob@$SSO_DOM" "&mock_groups=platform-admins" >/dev/null; owner_sql "SELECT m.role FROM users u JOIN memberships m ON m.user_id = u.id JOIN orgs o ON o.id = m.org_id WHERE u.email = 'bob@$SSO_DOM' AND o.slug = '$A'")" admin
check "unknown email domain cannot start SSO" "$(c js3 -o /dev/null -w '%{http_code}' -X POST "$BASE/sso/start" -d "who=eve@nowhere-$A.test")" 404
AUTH=$(c js3 -o /dev/null -w '%{redirect_url}' -X POST "$BASE/sso/start" -d "who=$A")
CB=$(curl -s --resolve host.docker.internal:8097:127.0.0.1 -o /dev/null -w '%{redirect_url}' "$AUTH&mock_email=eve@evil-$A.test")
check "email outside the organization's domains refused" "$(c js3 -o /dev/null -w '%{http_code}' "$CB")" 403
CB=$(sso_login js4 "carol@$SSO_DOM" >/dev/null; cat "$TMP/sso-last-callback")
AUTH=$(c js4 -o /dev/null -w '%{redirect_url}' -X POST "$BASE/sso/start" -d "who=carol@$SSO_DOM")
CB=$(curl -s --resolve host.docker.internal:8097:127.0.0.1 -o /dev/null -w '%{redirect_url}' "$AUTH")
check "tampered authorization code refused" "$(c js5 -o /dev/null -w '%{http_code}' "${CB/code=/code=x}")" 403
check "SSO sign-in audited" "$(owner_sql "SELECT count(*) >= 3 FROM audit_events WHERE action = 'login.sso' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
# A member with a password in the SSO domain: refused once single sign-on is required.
INV=$(c ja -X POST "$BASE/o/$A/invites" -d csrf=$CSRF_A -d "email=pw@$SSO_DOM" -d role=developer | grep -o '/invite/[A-Za-z0-9_-]*' | head -1)
code_of jp -X POST "$BASE$INV" --data-urlencode "name=Pw User" --data-urlencode "password=correct-horse-battery-pw" --data-urlencode "password2=correct-horse-battery-pw" >/dev/null
check "password sign-in works while SSO is optional" "$(code_of jp2 -X POST "$BASE/login" --data-urlencode "email=pw@$SSO_DOM" --data-urlencode "password=correct-horse-battery-pw")" 303
check "[A] single sign-on required" "$(sso_save -d enforce=1)" 303
check "password sign-in refused when SSO is required" "$(code_of jp3 -X POST "$BASE/login" --data-urlencode "email=pw@$SSO_DOM" --data-urlencode "password=correct-horse-battery-pw")" 403
check "the owner keeps password sign-in" "$(code_of jo -X POST "$BASE/login" --data-urlencode "email=owner@$A.test" --data-urlencode "password=correct-horse-battery-$A")" 303
check "[A] single sign-on turned off" "$(code_of ja -X POST "$BASE/o/$A/sso" -d csrf=$CSRF_A --data-urlencode "issuer=$IDP" -d client_id=platform-e2e -d "domains=$SSO_DOM" -d default_role=viewer)" 303
check "SSO no longer starts once off" "$(c js6 -o /dev/null -w '%{http_code}' -X POST "$BASE/sso/start" -d "who=alice@$SSO_DOM")" 404

section "Logs: search, download, retention, export"
LOGMARK=logexport-$RUN
for _ in 1 2 3; do curl -sS --resolve "$HOST_A:$PORT:127.0.0.1" -o /dev/null "http://$HOST_A:$PORT/?mark=$LOGMARK"; done
# log shipping can lag under load: wait for the lines to be searchable
check "[A] logs page finds lines across services" "$(for _ in $(seq 1 30); do grep -q "mark=$LOGMARK" <<<"$(c ja "$BASE/o/$A/monitoring/logs?q=$LOGMARK")" && { echo yes; break; }; sleep 2; done)" yes
check "[A] logs download holds the lines" "$(for _ in $(seq 1 15); do [ "$(c ja "$BASE/o/$A/monitoring/logs.txt?q=$LOGMARK&range=1h" | grep -c "$LOGMARK")" -ge 3 ] && { echo yes; break; }; sleep 2; done)" yes
check "[B->A] B cannot read A's logs (404)" "$(code_of jb "$BASE/o/$A/monitoring/logs.txt?q=$LOGMARK")" 404
check "B's own logs never show A's lines" "$(c jb "$BASE/o/$B/monitoring/logs.txt?q=$LOGMARK&range=1h" | grep -c "$LOGMARK")" 0
check "ranges beyond the plan's retention are offered disabled" "$(c ja "$BASE/o/$A/monitoring/logs" | grep -c 'longer than your plan keeps')" 1
check "a target outside the organization is refused (404)" "$(code_of ja "$BASE/o/$A/monitoring/logs.txt?target=shop/nope")" 404
code_of ja -X POST "$BASE/o/$A/storage/buckets" -d csrf=$CSRF_A -d name=logs -d quota_mb=20 >/dev/null
check "[A] export to a bucket created" "$(code_of ja -X POST "$BASE/o/$A/monitoring/logs/exports" -d csrf=$CSRF_A -d name=archive -d kind=bucket -d bucket=logs -d prefix=app-logs/)" 303
check "[A] export to an HTTPS endpoint created" "$(code_of ja -X POST "$BASE/o/$A/monitoring/logs/exports" -d csrf=$CSRF_A -d name=siem -d kind=https --data-urlencode "url=$IDP/sink" -d secret=e2e-sink-secret)" 303
check "export endpoint secret stored encrypted" "$(owner_sql "SELECT count(*) FROM log_exports e JOIN orgs o ON o.id = e.org_id WHERE o.slug = '$A' AND e.secret_enc IS NOT NULL AND position(convert_to('e2e-sink-secret', 'UTF8') in e.secret_enc) = 0")" 1
check "[B] cannot export to A's bucket" "$(c jb -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$B/monitoring/logs/exports" -d csrf=$CSRF_B -d name=steal -d kind=bucket -d bucket=logs | grep -c 'err=')" 1
owner_sql "SELECT pg_notify('deployments', 'logexport')" >/dev/null
check "HTTPS export delivered the lines, signed" "$(for i in $(seq 1 60); do [ $((i % 10)) = 0 ] && owner_sql "SELECT pg_notify('deployments', 'logexport')" >/dev/null; r=$(curl -s --resolve host.docker.internal:8097:127.0.0.1 "$IDP/sink/count?q=$LOGMARK"); [ "${r%% *}" -ge 3 ] 2>/dev/null && { echo "ok ${r##* }"; break; }; sleep 2; done)" "ok 0"
read -r KL SL <<<"$(new_s3_key ja "$A" "$CSRF_A" log-reader -d perm_logs=read)"
check "bucket export wrote hourly gzipped NDJSON files" "$(for _ in $(seq 1 30); do k=$(s3c "$KL" "$SL" "$S3/$A-logs?list-type=2&prefix=app-logs/" | grep -o '<Key>[^<]*ndjson.gz</Key>' | head -1 | sed 's/<[^>]*>//g'); [ -n "$k" ] && { echo yes; break; }; sleep 2; done)" yes
EXPORTED=""; for k in $(s3c "$KL" "$SL" "$S3/$A-logs?list-type=2&prefix=app-logs/" | grep -o '<Key>[^<]*</Key>' | sed 's/<[^>]*>//g'); do EXPORTED+=$(s3c "$KL" "$SL" "$S3/$A-logs/$k" | gzip -dc 2>/dev/null); done
check "exported lines name the project and service" "$(grep "$LOGMARK" <<<"$EXPORTED" | head -1 | grep -c '"project":"shop","service":"web"')" 1
check "export status shown as ok" "$(c ja "$BASE/o/$A/monitoring/logs" | grep -c 'st st-live">ok')" 2
EXP_ID=$(owner_sql "SELECT e.id FROM log_exports e JOIN orgs o ON o.id = e.org_id WHERE o.slug = '$A' AND e.name = 'siem'")
code_of ja -X POST "$BASE/o/$A/monitoring/logs/exports/$EXP_ID" -d csrf=$CSRF_A -d action=pause >/dev/null
check "[A] export paused" "$(owner_sql "SELECT enabled FROM log_exports WHERE id = '$EXP_ID'")" f
check "[B->A] B cannot delete A's export" "$(code_of jb -X POST "$BASE/o/$A/monitoring/logs/exports/$EXP_ID" -d csrf=$CSRF_B -d action=delete)" 404
code_of ja -X POST "$BASE/o/$A/monitoring/logs/exports/$EXP_ID" -d csrf=$CSRF_A -d action=delete >/dev/null
check "[A] export deleted" "$(owner_sql "SELECT count(*) FROM log_exports WHERE id = '$EXP_ID'")" 0
code_of ja -X POST "$BASE/o/$A/storage/b/logs/delete" -d csrf=$CSRF_A -d confirm=logs >/dev/null
docker rm -f e2e-oidc >/dev/null 2>&1

TOK_ID=$(owner_sql "SELECT t.id FROM api_tokens t JOIN orgs o ON o.id = t.org_id WHERE o.slug = '$A' AND t.name = 'ci'")
code_of ja -X POST "$BASE/o/$A/api-tokens/$TOK_ID/delete" -d csrf=$CSRF_A >/dev/null
check "[API] revoked token refused (401)" "$(api_code "$TOK_DEV" "$BASE/api/v1/me")" 401

section "Public status pages"
pub_code() { curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "$BASE$1"; }
check "status page is off by default (404)" "$(pub_code /status/$A/shop)" 404
check "[B->A] B cannot turn on A's status page" "$(code_of jb -X POST "$BASE/o/$A/p/shop/status-page" -d csrf=$CSRF_B -d public=1)" 404
check "[A] status page turned on" "$(code_of ja -X POST "$BASE/o/$A/p/shop/status-page" -d csrf=$CSRF_A -d public=1)" 303
PAGE=$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" "$BASE/status/$A/shop")
if echo "$PAGE" | grep -q "<strong>web</strong>"; then ok "public status page lists A's services, no login needed"; else bad "status page missing services"; fi
if echo "$PAGE" | grep -q "tenant-a-secret\|GREETING\|nginx:"; then bad "status page leaks settings"; else ok "status page shows no secrets, env or images"; fi
check "B's project stays private (404)" "$(pub_code /status/$B/shop)" 404
check "uptime is being recorded" "$(psql_api "SELECT count(*) > 0 FROM status_page_services('$A','shop') WHERE total > 0" | tr -d '\r')" t
check "[A] status page turned off" "$(code_of ja -X POST "$BASE/o/$A/p/shop/status-page" -d csrf=$CSRF_A)" 303
check "status page gone after turning off (404)" "$(pub_code /status/$A/shop)" 404

section "Passkeys"
(cd .. && go build -o "$TMP/softauthn.exe" ./scripts/softauthn) || bad "softauthn did not build"
SID_A=$(awk -F'\t' '$6 == "sid" {print $7}' "$TMP/ja" | tr -d '\r')
if "$TMP/softauthn.exe" register -url "$BASE" -sid "$SID_A" -csrf "$CSRF_A" -key "$TMP/pk-a.json" -name "e2e key" | grep -q registered; then ok "[A] passkey registered"; else bad "[A] passkey registration failed"; fi
if c ja "$BASE/account" | grep -q "e2e key"; then ok "[A] passkey listed on the account page"; else bad "[A] passkey not listed"; fi
check "registration without the CSRF token refused (403)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -b "sid=$SID_A" -X POST -H 'Content-Type: application/json' -d '{}' "$BASE/account/passkeys/begin")" 403
NEW_SID=$("$TMP/softauthn.exe" login -url "$BASE" -key "$TMP/pk-a.json" 2>&1)
check "passkey sign-in opens a full session (no password, no code)" \
  "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -b "sid=$NEW_SID" "$BASE/o/$A")" 200
OUT=$("$TMP/softauthn.exe" login -url "$BASE" -key "$TMP/pk-a.json" -tamper 2>&1)
if echo "$OUT" | grep -q "HTTP 401"; then ok "forged passkey signature refused"; else bad "forged signature accepted"; fi
check "sign-in from another site refused (403)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -X POST -H 'Origin: https://evil.example' "$BASE/login/passkey/begin")" 403
check "passkey sign-in audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'login.passkey' AND user_id = (SELECT id FROM users WHERE email = 'owner@$A.test')" | tr -d '\r')" 1
PKID=$(c ja "$BASE/account" | grep -o '/account/passkeys/[A-Za-z0-9_-]*/delete' | head -1)
check "[B->A] B cannot remove A's passkey" "$(code_of jb -X POST "$BASE$PKID" -d csrf=$CSRF_B)" 303
check "A's passkey still there after B's attempt" "$(psql_api "SELECT count(*) FROM passkeys p JOIN users u ON u.id = p.user_id WHERE u.email = 'owner@$A.test'" | tr -d '\r')" 1
check "[A] passkey removed" "$(code_of ja -X POST "$BASE$PKID" -d csrf=$CSRF_A)" 303
OUT=$("$TMP/softauthn.exe" login -url "$BASE" -key "$TMP/pk-a.json" 2>&1)
if echo "$OUT" | grep -q "HTTP 401"; then ok "removed passkey can no longer sign in"; else bad "removed passkey still works"; fi

section "Traffic protection"
app_code() { curl -s -o /dev/null -w '%{http_code}' --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT$1"; }
wait_app_code() { # wait_app_code <path> <expected>: proxy changes apply within seconds
  local got
  for _ in $(seq 1 20); do got=$(app_code "$1"); [ "$got" = "$2" ] && break; sleep 1; done
  echo "$got"
}
XSS='/?q=%3Cscript%3Ealert(1)%3C%2Fscript%3E'
SQLI='/?id=1%27%20OR%20%271%27%3D%271'
check "WAF off: suspicious request passes" "$(app_code "$XSS")" 200
check "[B->A] B cannot change A's protection" "$(code_of jb -X POST "$BASE$SVC_A/protection" -d csrf=$CSRF_B -d waf=1)" 404
if c ja -L "$BASE$SVC_A/protection" -d csrf=$CSRF_A -d rpm=5 | grep -q "between 10 and 100000"; then ok "request limit out of range refused"; else bad "rpm=5 accepted"; fi
if c ja -L "$BASE$SVC_A/protection" -d csrf=$CSRF_A --data-urlencode "blocked=not-an-ip" | grep -q "Not an address"; then ok "invalid blocked address refused"; else bad "invalid address accepted"; fi
check "[A] WAF turned on" "$(code_of ja -X POST "$BASE$SVC_A/protection" -d csrf=$CSRF_A -d waf=1)" 303
check "WAF blocks cross-site scripting (403)" "$(wait_app_code "$XSS" 403)" 403
check "WAF blocks SQL injection (403)" "$(app_code "$SQLI")" 403
check "WAF lets normal requests through" "$(app_code /)" 200
check "B's app unaffected by A's WAF" "$(curl -s -o /dev/null -w '%{http_code}' --resolve "web-shop-$B.apps.localhost:$PORT:127.0.0.1" "http://web-shop-$B.apps.localhost:$PORT$XSS")" 200
check "[A] request limit 10/min set" "$(code_of ja -X POST "$BASE$SVC_A/protection" -d csrf=$CSRF_A -d waf=1 -d rpm=10)" 303
sleep 1
N429=0; for _ in $(seq 1 15); do [ "$(app_code /)" = 429 ] && N429=$((N429+1)); done
if [ "$N429" -ge 4 ]; then ok "visitors over the limit get 429 ($N429 of 15)"; else bad "rate limit not enforced ($N429 of 15 got 429)"; fi
check "[A] block every address" "$(code_of ja -X POST "$BASE$SVC_A/protection" -d csrf=$CSRF_A --data-urlencode $'blocked=0.0.0.0/0\n::/0')" 303
check "blocked visitors get 403" "$(wait_app_code / 403)" 403
check "protection saved in the audit log" "$(owner_sql "SELECT count(*) >= 3 FROM audit_events WHERE action = 'service.protection' AND target = 'service:web' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
check "[A] protection turned off" "$(code_of ja -X POST "$BASE$SVC_A/protection" -d csrf=$CSRF_A)" 303
check "app answers normally again" "$(wait_app_code "$XSS" 200)" 200

section "Usage metering and invoices"
check "usage is metered every minute" "$(owner_sql "SELECT count(*) > 0 FROM usage_hourly u JOIN orgs o ON o.id = u.org_id WHERE o.slug = '$A' AND u.cpu_milli_minutes > 0")" t
# Last month for A: existed all month, 1 vCPU-hour, 1 GB-hour, 1 GB out (fictitious usage).
LAST=$(date -u -d "$(date -u +%Y-%m-01) -1 day" +%Y-%m)
owner_sql "UPDATE orgs SET created_at = '$LAST-01'::date - interval '1 day' WHERE slug = '$A';
  INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, cpu_milli_minutes, mem_mb_minutes, egress_bytes)
  SELECT s.org_id, s.id, 'service', 'shop/web', '$LAST-02 10:00+00', 60000, 61440, 1000000000
  FROM services s JOIN orgs o ON o.id = s.org_id JOIN projects p ON p.id = s.project_id WHERE o.slug = '$A' AND p.slug = 'shop' AND s.slug = 'web'
  ON CONFLICT DO NOTHING" >/dev/null
admin generate-invoices --month "$LAST" >/dev/null
INV=$(owner_sql "SELECT number FROM invoices i JOIN orgs o ON o.id = i.org_id WHERE o.slug = '$A' AND i.period_start = '$LAST-01'")
# Starter: fee 500c + 1c CPU + 1c memory (0.5 rounded) + 2c out = 504c, +20% tax = 605c
check "[A] monthly invoice issued with the right total" "$(owner_sql "SELECT total_cents FROM invoices WHERE number = '$INV'")" 605
check "invoice has fee, CPU, memory and network lines" "$(owner_sql "SELECT count(*) FROM invoice_lines l JOIN invoices i ON i.id = l.invoice_id WHERE i.number = '$INV'")" 4
admin generate-invoices --month "$LAST" >/dev/null
check "running invoicing again issues nothing twice" "$(owner_sql "SELECT count(*) FROM invoices i JOIN orgs o ON o.id = i.org_id WHERE o.slug = '$A'")" 1
check "no invoice for B (nothing used last month)" "$(owner_sql "SELECT count(*) FROM invoices i JOIN orgs o ON o.id = i.org_id WHERE o.slug = '$B'")" 0
if c ja "$BASE/o/$A/billing" | grep -q "$INV"; then ok "[A] invoice listed on the Billing page"; else bad "[A] invoice not listed"; fi
PAGE=$(c ja "$BASE/o/$A/invoices/$INV")
if echo "$PAGE" | grep -q "6.05 EUR" && echo "$PAGE" | grep -q "Example Hosting"; then ok "[A] invoice page shows total and seller"; else bad "[A] invoice page incomplete"; fi
check "[B->A] B cannot open A's invoice (404)" "$(code_of jb "$BASE/o/$B/invoices/$INV")" 404
check "[B->A] B cannot open A's billing page (404)" "$(code_of jb "$BASE/o/$A/billing")" 404
check "[A] billing details saved" "$(code_of ja -X POST "$BASE/o/$A/billing/details" -d csrf=$CSRF_A -d "billing_name=Shop Owner Ltd (fictitious)" --data-urlencode $'billing_address=2 Test Road\nTestville')" 303
admin invoice-paid --number "$INV" >/dev/null
if c ja "$BASE/o/$A/invoices/$INV" | grep -q "PAID"; then ok "operator payment shows as paid"; else bad "invoice not marked paid"; fi
check "invoice issue and payment audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE target = 'invoice:$INV' AND action IN ('invoice.issued', 'invoice.paid')")" 2

section "Preview environments"
admin set-plan --slug "$A" --plan pro >/dev/null   # room for previews in the CPU/memory quota
TOK_DEV=$(new_api_token ja "$A" "$CSRF_A" previews developer)   # the first CI token was revoked above
PV1=pr-1.web-shop-$A.apps.localhost
pv_code() { curl -s -o /dev/null -w '%{http_code}' --resolve "$1:$PORT:127.0.0.1" "http://$1:$PORT/"; }
wait_pv_code() { local got; for _ in $(seq 1 45); do got=$(pv_code "$1"); [ "$got" = "$2" ] && break; sleep 2; done; echo "$got"; }
check "[A] preview created in the console" "$(code_of ja -X POST "$BASE$SVC_A/previews" -d csrf=$CSRF_A -d name=pr-1 -d image=nginx:1.27-alpine -d ttl_hours=24)" 303
check "preview answers at its own address" "$(wait_pv_code "$PV1" 200)" 200
PV_CID=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.preview_name = 'pr-1' AND d.status = 'live'")
check "preview gets the service's secrets" "$(docker exec "$PV_CID" printenv API_KEY | tr -d '\r')" "tenant-a-secret"
if api_get "$TOK_DEV" "$BASE/api/v1/projects/shop/services" | grep -q '"pv-'; then bad "previews leak into the service list"; else ok "previews are not listed as services"; fi
check "[API] invalid preview name refused (422)" "$(api_code "$TOK_DEV" -X POST -H 'Content-Type: application/json' -d '{"name":"Bad_Name"}' "$BASE/api/v1/projects/shop/services/web/previews")" 422
if pctl preview create shop/web pr-2 --image nginx:1.27-alpine --ttl 2 --wait | grep -q "is live"; then ok "[CLI] platformctl preview create --wait reaches live"; else bad "[CLI] preview create failed"; fi
check "[API] creating an existing preview updates it (200)" "$(api_code "$TOK_DEV" -X POST -H 'Content-Type: application/json' -d '{"name":"pr-2","image":"nginx:1.27-alpine"}' "$BASE/api/v1/projects/shop/services/web/previews")" 200
check "[CLI] platformctl preview list shows both" "$(pctl preview list shop/web | grep -c '^pr-')" 2
check "[B->A] B cannot delete A's preview (404)" "$(api_code "$TOK_B" -X DELETE "$BASE/api/v1/projects/shop/services/web/previews/pr-1")" 404
check "A's preview still up after B's attempt" "$(pv_code "$PV1")" 200
owner_sql "UPDATE services SET expires_at = now() - interval '1 minute' WHERE preview_name = 'pr-2' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')" >/dev/null
check "expired preview removed automatically" "$(wait_pv_code "pr-2.web-shop-$A.apps.localhost" 404)" 404
check "expiry audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'preview.expired' AND detail->>'preview' = 'pr-2' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" 1
if pctl preview delete shop/web pr-1 | grep -q deleted; then ok "[CLI] platformctl preview delete"; else bad "[CLI] preview delete failed"; fi
check "deleted preview no longer served" "$(wait_pv_code "$PV1" 404)" 404
check "parent service unaffected" "$(app_code /)" 200

section "Shared project variables"
VARS=/api/v1/projects/shop/variables
WEB_CID() { owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND p.slug = 'shop' AND s.slug = 'web' AND s.preview_name IS NULL AND d.status = 'live'"; }
check "[API] secret project variable saved" "$(api_code "$TOK_DEV" -X PUT -H 'Content-Type: application/json' -d '{"value":"shared-secret-xyz"}' "$BASE$VARS/SHARED_TOKEN")" 200
check "[API] project variable shadowed by the service's own secret" "$(api_code "$TOK_DEV" -X PUT -H 'Content-Type: application/json' -d '{"value":"from-project","secret":false}' "$BASE$VARS/API_KEY")" 200
check "[API] reserved name refused (422)" "$(api_code "$TOK_DEV" -X PUT -H 'Content-Type: application/json' -d '{"value":"1"}' "$BASE$VARS/PORT")" 422
check "[API] viewer token cannot set variables (403)" "$(api_code "$TOK_VIEW" -X PUT -H 'Content-Type: application/json' -d '{"value":"x"}' "$BASE$VARS/X")" 403
OLD_WEB=$(WEB_CID)
check "[A] plain variable saved in the console, services redeployed" "$(code_of ja -X POST "$BASE/o/$A/p/shop/variables" -d csrf=$CSRF_A -d name=SHARED_HOST -d value=shared.example.test -d redeploy=1)" 303
for _ in $(seq 1 45); do NEW_WEB=$(WEB_CID); [ -n "$NEW_WEB" ] && [ "$NEW_WEB" != "$OLD_WEB" ] && break; sleep 2; done
check "service redeployed with the project variables" "$([ -n "$NEW_WEB" ] && [ "$NEW_WEB" != "$OLD_WEB" ] && echo yes)" yes
check "service receives the secret project variable" "$(docker exec "$NEW_WEB" printenv SHARED_TOKEN | tr -d '\r')" shared-secret-xyz
check "service receives the plain project variable" "$(docker exec "$NEW_WEB" printenv SHARED_HOST | tr -d '\r')" shared.example.test
check "the service's own secret wins over the project variable" "$(docker exec "$NEW_WEB" printenv API_KEY | tr -d '\r')" tenant-a-secret
check "each name appears once in the environment" "$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$NEW_WEB" | grep -c '^API_KEY=')" 1
BVARS=$(api_get "$TOK_B" "$BASE$VARS")   # B's own project is also called "shop"
if ! grep -q "shared.example.test\|SHARED_HOST" <<<"$BVARS"; then ok "[B->A] B's token sees only B's project variables"; else bad "[B->A] B sees A's variables"; fi
A_ROW() { owner_sql "SELECT md5(v.value_enc::text) || v.updated_at FROM project_variables v JOIN orgs o ON o.id = v.org_id JOIN projects p ON p.id = v.project_id WHERE o.slug = '$A' AND p.slug = 'shop' AND v.name = 'SHARED_TOKEN'"; }
A_BEFORE=$(A_ROW)   # compared as-is: no clock involved (the VM clock can jump)
api_code "$TOK_B" -X PUT -H 'Content-Type: application/json' -d '{"value":"evil"}' "$BASE$VARS/SHARED_TOKEN" >/dev/null
check "[B->A] B's write lands in B's own project only" "$([ "$(A_ROW)" = "$A_BEFORE" ] && echo a-unchanged):$(owner_sql "SELECT count(*) FROM project_variables v JOIN orgs o ON o.id = v.org_id WHERE o.slug = '$B' AND v.name = 'SHARED_TOKEN'")" "a-unchanged:1"
PPAGE=$(c ja "$BASE/o/$A/p/shop")
if grep -q "shared.example.test" <<<"$PPAGE" && ! grep -q "shared-secret-xyz" <<<"$PPAGE"; then ok "project page shows plain values and hides secret ones"; else bad "project page shows the wrong values"; fi
if grep -q "<code>SHARED_TOKEN</code>" <<<"$(c ja "$BASE$SVC_A")"; then ok "service page lists the variables it receives from the project"; else bad "inherited variables not listed"; fi
VLIST=$(pctl var list shop)
if grep -q "SHARED_HOST *shared.example.test" <<<"$VLIST" && grep -q "SHARED_TOKEN *(secret)" <<<"$VLIST" && ! grep -q "shared-secret-xyz" <<<"$VLIST"; then ok "[CLI] platformctl var list hides secret values"; else bad "[CLI] var list wrong: $VLIST"; fi
check "secret values encrypted at rest" "$(owner_sql "SELECT count(*) FROM project_variables WHERE position(convert_to('shared-secret-xyz', 'UTF8') IN value_enc) > 0")" 0
if printf 'cli-value\n' | pctl var set shop CLI_VAR | grep -q "saved"; then ok "[CLI] platformctl var set (value from standard input)"; else bad "[CLI] var set failed"; fi
for _ in $(seq 1 30); do [ "$(owner_sql "SELECT count(*) FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND d.status IN ('pending', 'deploying')")" = 0 ] && break; sleep 2; done
if pctl redeploy shop | grep -q "redeploying [1-9]"; then ok "[CLI] platformctl redeploy restarts the project's services"; else bad "[CLI] redeploy did nothing"; fi
check "redeploy reaches live" "$(wait_status ja $SVC_A)" live
check "new variable applied after the redeploy" "$(docker exec "$(WEB_CID)" printenv CLI_VAR | tr -d '\r')" cli-value
if pctl var unset shop CLI_VAR | grep -q deleted; then ok "[CLI] platformctl var unset"; else bad "[CLI] var unset failed"; fi
check "[API] deleted variable no longer listed" "$(api_get "$TOK_DEV" "$BASE$VARS" | grep -c CLI_VAR)" 0
check "project variable changes audited" "$(owner_sql "SELECT count(*) >= 4 FROM audit_events WHERE action IN ('project_variable.set', 'project_variable.deleted') AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
owner_sql "DELETE FROM project_variables WHERE name = 'API_KEY' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')" >/dev/null

section "Workers and scheduled jobs"
SVC_Q=/o/$A/p/shop/s/queue
code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=queue -d image=nginx:1.27-alpine >/dev/null
check "[A] worker service saved and deployed" "$(code_of ja -X POST "$BASE$SVC_Q/settings" -d csrf=$CSRF_A -d kind=worker -d image=nginx:1.27-alpine \
  -d port=80 -d health_path=%2F -d cpu_millis=100 -d memory_mb=64 -d volume_path= -d env= -d deploy=1)" 303
check "worker reaches live without a health check URL" "$(wait_status ja $SVC_Q)" live
Q_CID=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'queue' AND d.status = 'live'")
check "worker gets no PORT variable" "$(docker exec "$Q_CID" sh -c 'echo "${PORT:-none}"' | tr -d '\r')" none
check "worker has no public address" "$(curl -s -o /dev/null -w '%{http_code}' --resolve "queue-shop-$A.apps.localhost:$PORT:127.0.0.1" "http://queue-shop-$A.apps.localhost:$PORT/")" 404
code_of ja -X POST "$BASE$SVC_Q/domains" -d csrf=$CSRF_A -d hostname=worker.example.test >/dev/null
check "a worker cannot get a domain" "$(owner_sql "SELECT count(*) FROM domains WHERE hostname = 'worker.example.test'")" 0
if grep -q "worker · no public address" <<<"$(c ja "$BASE$SVC_Q")"; then ok "service page marks the worker"; else bad "worker not shown as such"; fi
check "[A] worker deleted" "$(code_of ja -X POST "$BASE$SVC_Q/delete" -d csrf=$CSRF_A -d confirm=queue)" 303

JOBS=/api/v1/projects/shop/services/web/jobs
last_run() { owner_sql "SELECT r.$2 FROM job_runs r JOIN jobs j ON j.id = r.job_id JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND j.slug = '$1' ORDER BY r.started_at DESC LIMIT 1"; }
wait_run() { local st; for _ in $(seq 1 45); do st=$(last_run "$1" status); [ -n "$st" ] && [ "$st" != running ] && break; sleep 2; done; echo "$st"; }
check "[A] job created in the console" "$(code_of ja -X POST "$BASE$SVC_A/jobs" -d csrf=$CSRF_A -d slug=hello -d 'schedule=* * * * *' \
  --data-urlencode 'command=echo "job says $SHARED_HOST" && echo "run of $PLATFORM_JOB"' -d timeout_seconds=60)" 303
check "[API] invalid schedule refused (422)" "$(api_code "$TOK_DEV" -X PUT -H 'Content-Type: application/json' -d '{"schedule":"61 * * * *","command":"true"}' "$BASE$JOBS/bad")" 422
check "[API] job created with the API" "$(api_code "$TOK_DEV" -X PUT -H 'Content-Type: application/json' -d '{"schedule":"0 0 1 1 *","command":"echo about to fail; exit 3"}' "$BASE$JOBS/fail")" 200
check "[API] viewer token cannot run a job (403)" "$(api_code "$TOK_VIEW" -X POST "$BASE$JOBS/fail/run")" 403
check "[B->A] B cannot run A's job (404)" "$(api_code "$TOK_B" -X POST "$BASE$JOBS/fail/run")" 404
if ! api_get "$TOK_B" "$BASE$JOBS" | grep -q '"hello"'; then ok "[B->A] B's job list shows none of A's jobs"; else bad "[B->A] B sees A's jobs"; fi
JOUT=$(pctl job run shop/web hello --wait)
if grep -q "job says shared.example.test" <<<"$JOUT" && grep -q "run of hello" <<<"$JOUT" && grep -q "succeeded" <<<"$JOUT"; then ok "[CLI] platformctl job run --wait: output with project variables, success"; else bad "[CLI] job run: $JOUT"; fi
check "manual run recorded as succeeded" "$(owner_sql "SELECT r.status || ':' || r.exit_code FROM job_runs r JOIN jobs j ON j.id = r.job_id JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND j.slug = 'hello' AND r.trigger = 'manual' ORDER BY r.started_at DESC LIMIT 1")" succeeded:0
check "[API] job run requested (202)" "$(api_code "$TOK_DEV" -X POST "$BASE$JOBS/fail/run")" 202
check "failing job recorded with its exit code" "$(wait_run fail):$(last_run fail exit_code)" failed:3
check "failed job raises a job_failed alert event" "$(for _ in $(seq 1 10); do n=$(owner_sql "SELECT count(*) FROM alert_incidents i JOIN orgs o ON o.id = i.org_id WHERE o.slug = '$A' AND i.kind = 'job_failed' AND i.summary LIKE 'job fail %exit code 3'"); [ "$n" -ge 1 ] && { echo yes; break; }; sleep 1; done)" yes
if pctl job logs shop/web fail | grep -q "about to fail"; then ok "[CLI] platformctl job logs shows the last run"; else bad "[CLI] job logs missing output"; fi
api_code "$TOK_DEV" -X PUT -H 'Content-Type: application/json' -d '{"schedule":"0 0 1 1 *","command":"sleep 60","timeout_seconds":10}' "$BASE$JOBS/slow" >/dev/null
api_code "$TOK_DEV" -X POST "$BASE$JOBS/slow/run" >/dev/null
check "a job over its time limit is stopped" "$(wait_run slow)" timed_out
check "scheduled run happens on time (every minute)" "$(for _ in $(seq 1 45); do [ "$(owner_sql "SELECT count(*) FROM job_runs r JOIN jobs j ON j.id = r.job_id JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND j.slug = 'hello' AND r.trigger = 'schedule'")" -ge 1 ] && { echo yes; break; }; sleep 2; done)" yes
check "job containers removed after each run" "$(for _ in $(seq 1 15); do [ "$(docker ps -aq --filter label=platform.job | wc -l | tr -d ' ')" = 0 ] && { echo 0; break; }; sleep 2; done)" 0
RUN_ID=$(last_run fail id)
if grep -q "about to fail" <<<"$(c ja "$BASE$SVC_A/job-runs/$RUN_ID")"; then ok "[A] full run log opens in the console"; else bad "run log page missing"; fi
check "[B->A] B cannot open A's run log (404)" "$(code_of jb "$BASE$SVC_A/job-runs/$RUN_ID")" 404
if grep -q "<strong>hello</strong>" <<<"$(c ja "$BASE$SVC_A")"; then ok "[A] Jobs tab lists the jobs"; else bad "jobs not listed"; fi
code_of ja -X POST "$BASE$SVC_A/jobs/hello/pause" -d csrf=$CSRF_A >/dev/null
check "paused job has no next run" "$(owner_sql "SELECT enabled::text || ':' || (next_run_at IS NULL) FROM jobs j JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND j.slug = 'hello'")" "false:true"
for j in hello fail slow; do api_code "$TOK_DEV" -X DELETE "$BASE$JOBS/$j" >/dev/null; done
check "[API] jobs deleted" "$(api_get "$TOK_DEV" "$BASE$JOBS")" "[]"
check "job changes audited" "$(owner_sql "SELECT count(*) >= 6 FROM audit_events WHERE action LIKE 'job.%' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t

section "Several instances per service"
WEB_DEP() { owner_sql "SELECT d.$1 FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND p.slug = 'shop' AND s.slug = 'web' AND s.preview_name IS NULL AND d.status = 'live'"; }
if pctl scale shop/web 3 --wait | grep -q "is live"; then ok "[CLI] platformctl scale to 3 instances reaches live"; else bad "[CLI] scale failed"; fi
check "three instances recorded" "$(WEB_DEP 'instances' | grep -o '"id"' | wc -l | tr -d ' ')" 3
DEP3=$(WEB_DEP id)
check "three containers running" "$(docker ps -q --filter label=platform.deployment=$DEP3 | wc -l | tr -d ' ')" 3
for _ in $(seq 1 15); do app_code / >/dev/null; done
check "the proxy spreads requests over the instances" "$(n=0; for c in $(docker ps -q --filter label=platform.deployment=$DEP3); do docker logs "$c" 2>&1 | grep -q 'GET / ' && n=$((n+1)); done; [ "$n" -ge 2 ] && echo yes)" yes
check "the service page shows the instance count" "$(grep -c '3 instances' <<<"$(c ja "$BASE$SVC_A")" | tr -d ' ')" 1
LOST=$(docker ps -q --filter label=platform.deployment=$DEP3 | head -1)
docker rm -f "$LOST" >/dev/null
check "app keeps answering with an instance lost" "$(app_code /)" 200
check "a lost instance is replaced (new revision, 3 instances)" "$(for _ in $(seq 1 45); do d=$(WEB_DEP id); [ -n "$d" ] && [ "$d" != "$DEP3" ] && [ "$(docker ps -q --filter label=platform.deployment=$d | wc -l | tr -d ' ')" = 3 ] && { echo yes; break; }; sleep 2; done)" yes
check "replacement audited" "$(owner_sql "SELECT count(*) >= 1 FROM audit_events WHERE action = 'deployment.redeployed' AND detail->>'reason' LIKE '%instances were lost' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
check "the old instances are removed" "$(for _ in $(seq 1 20); do [ "$(docker ps -aq --filter label=platform.deployment=$DEP3 | wc -l | tr -d ' ')" = 0 ] && { echo yes; break; }; sleep 2; done)" yes
check "[API] more instances than the plan allows refused (422)" "$(api_code "$TOK_DEV" -X POST -H 'Content-Type: application/json' -d '{"instances":6}' "$BASE/api/v1/projects/shop/services/web/scale")" 422
check "[API] several instances with a persistent folder refused (422)" "$(api_code "$TOK_DEV" -X POST -H 'Content-Type: application/json' -d '{"instances":2}' "$BASE/api/v1/projects/shop/services/files/scale")" 422
api_code "$TOK_B" -X POST -H 'Content-Type: application/json' -d '{"instances":1}' "$BASE/api/v1/projects/shop/services/web/scale" >/dev/null   # B's own shop/web
check "[B->A] B's token scales only B's service" "$(owner_sql "SELECT instances FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'web' AND s.preview_name IS NULL")" 3
if pctl scale shop/web 1 --wait | grep -q "is live"; then ok "[CLI] scale back to 1 instance"; else bad "[CLI] scale back failed"; fi
check "one container after scaling down" "$(docker ps -q --filter label=platform.deployment=$(WEB_DEP id) | wc -l | tr -d ' ')" 1
check "app answers after scaling down" "$(app_code /)" 200

section "Autoscaling"
admin set-plan --slug "$A" --plan pro >/dev/null   # room for three instances
mkdir -p "$TMP/busy"; printf '%s\n' 'FROM nginx:1.27-alpine' 'CMD ["sh", "-c", "yes > /dev/null & exec nginx -g \"daemon off;\""]' > "$TMP/busy/Dockerfile"
docker build -q -t "$REG/$A/busy:1" "$(cygpath -w "$TMP/busy")" >/dev/null
TOK_AS=$(new_token ja "$A" "$CSRF_A")
echo "$TOK_AS" | dk login $REG -u "$A" --password-stdin >/dev/null; dk push "$REG/$A/busy:1" >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=busy -d image=nginx:1.27-alpine >/dev/null
AS_SET=(-d csrf=$CSRF_A -d "image=$REG/$A/busy:1" -d port=80 -d health_path=%2F -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env=)
check "autoscaling refused above the plan's instances" "$(admin set-plan --slug "$A" --plan starter >/dev/null; c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/shop/s/busy/settings" "${AS_SET[@]}" -d autoscale=1 -d autoscale_min=1 -d autoscale_max=9 -d autoscale_cpu=50 | grep -c 'err='; admin set-plan --slug "$A" --plan pro >/dev/null)" 1
check "autoscaling refused with a persistent folder" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/shop/s/busy/settings" "${AS_SET[@]/volume_path=/volume_path=%2Fdata}" -d autoscale=1 -d autoscale_min=1 -d autoscale_max=3 -d autoscale_cpu=50 | grep -c 'persistent+folder')" 1
check "[A] CPU-busy service with autoscaling 1-3 at 50%" "$(code_of ja -X POST "$BASE/o/$A/p/shop/s/busy/settings" "${AS_SET[@]}" -d autoscale=1 -d autoscale_min=1 -d autoscale_max=3 -d autoscale_cpu=50 -d deploy=1)" 303
check "[A] busy service live" "$(wait_status ja /o/$A/p/shop/s/busy)" live
check "it scales out when the CPU stays above the target" "$(for _ in $(seq 1 60); do n=$(owner_sql "SELECT instances FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'busy'"); [ "${n:-1}" -ge 2 ] && { echo yes; break; }; sleep 5; done)" yes
check "the new instances go live behind the proxy" "$(wait_status ja /o/$A/p/shop/s/busy):$(owner_sql "SELECT jsonb_array_length(d.instances) >= 2 FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'busy' AND d.status = 'live'")" "live:t"
check "scaling audited with the measured CPU" "$(owner_sql "SELECT count(*) > 0 FROM audit_events WHERE action = 'service.autoscaled' AND org_id = (SELECT id FROM orgs WHERE slug = '$A') AND detail->>'target' = '50'")" t
code_of ja -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/busy >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "Builds from git"
# A local git server (static "dumb HTTP" repository) reached from the build sandbox
# through host.docker.internal; BUILD_ALLOW_INSECURE_REPOS is on in the dev stack only.
GITD="$TMP/git"; mkdir -p "$GITD/src" "$GITD/srv"
cat > "$GITD/src/Dockerfile" <<'DOCKERFILE'
FROM nginx:1.27-alpine
RUN if [ -e /var/run/docker.sock ]; then echo DOCKER-SOCKET-VISIBLE; else echo SANDBOX-NO-DOCKER-SOCKET; fi
RUN if wget -q -T 3 -O /dev/null http://postgres:5432 2>/dev/null; then echo PLATFORM-REACHABLE; else echo SANDBOX-NO-PLATFORM-ACCESS; fi
COPY index.html /usr/share/nginx/html/index.html
DOCKERFILE
echo "built-from-git-$RUN" > "$GITD/src/index.html"
(cd "$GITD/src" && git init -q -b main && git config core.autocrlf false && git add . && git -c user.email=e2e@example.test -c user.name=e2e commit -qm "e2e app") >/dev/null
(cd "$GITD/src" && git checkout -q -b node && git rm -q Dockerfile index.html \
  && printf '%s\n' '{ "name": "e2e-node", "version": "1.0.0", "scripts": { "start": "node server.js" } }' > package.json \
  && printf '%s\n' "require('http').createServer((q, s) => s.end('built-with-railpack-$RUN')).listen(process.env.PORT || 3000);" > server.js \
  && git add . && git -c user.email=e2e@example.test -c user.name=e2e commit -qm "node app, no Dockerfile" && git checkout -q main) >/dev/null
git clone -q --bare "$GITD/src" "$GITD/srv/app.git" && (cd "$GITD/srv/app.git" && git update-server-info)
docker rm -f e2e-git >/dev/null 2>&1
MSYS_NO_PATHCONV=1 docker run -d --name e2e-git -p 8099:80 -v "$(cygpath -w "$GITD/srv"):/usr/share/nginx/html:ro" nginx:1.27-alpine >/dev/null
for _ in $(seq 1 30); do curl -s -o /dev/null -f http://127.0.0.1:8099/app.git/HEAD && break; sleep 1; done   # the build must not start before the server answers
REPO=http://host.docker.internal:8099/app.git
if c ja -L "$BASE$SVC_A/source" -d csrf=$CSRF_A --data-urlencode "repo=https://user:pw@example.com/a.git" -d branch=main | grep -q "Repository:"; then ok "repository address with credentials refused"; else bad "credentials in repository address accepted"; fi
check "[A] git source saved" "$(code_of ja -X POST "$BASE$SVC_A/source" -d csrf=$CSRF_A --data-urlencode "repo=$REPO" -d branch=main -d dir=)" 303
check "[A] build started from the console" "$(code_of ja -X POST "$BASE$SVC_A/builds" -d csrf=$CSRF_A -d deploy=1)" 303
OUT=$(pctl build shop/web --wait)
if echo "$OUT" | grep -q "done: .*/$A/web:git-"; then ok "[CLI] build finished; image pushed to A's registry space"; else bad "build failed: $(echo "$OUT" | tail -3)"; fi
if echo "$OUT" | grep -q SANDBOX-NO-DOCKER-SOCKET; then ok "build sandbox has no Docker socket"; else bad "Docker socket check missing"; fi
if echo "$OUT" | grep -q SANDBOX-NO-PLATFORM-ACCESS; then ok "build sandbox cannot reach platform services"; else bad "platform reachable from a build"; fi
check "built image deployed (app serves the repository's page)" "$(for _ in $(seq 1 45); do b=$(curl -s --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/"); echo "$b" | grep -q "built-from-git-$RUN" && { echo yes; break; }; sleep 2; done)" yes
check "[B->A] B cannot open A's build page (404)" "$(code_of jb "$BASE/o/$A/p/shop/s/web/builds/1")" 404
HOOK=$(c ja -L "$BASE$SVC_A/build-hook" -d csrf=$CSRF_A | grep -o "$BASE/hooks/build/bh_[A-Za-z0-9_-]*" | head -1)
if [ -n "$HOOK" ]; then ok "[A] webhook address created (shown once)"; else bad "webhook address missing"; fi
hook() { curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' "$@"; }
check "webhook: unknown token refused (404)" "$(hook -d '{}' "$BASE/hooks/build/bh_wrong")" 404
check "webhook: GitHub ping answered without a build" "$(hook -H 'X-GitHub-Event: ping' -d '{"zen":"hi"}' "$HOOK")" 200
check "webhook: push to another branch ignored (202)" "$(hook -d '{"ref":"refs/heads/feature"}' "$HOOK")" 202
check "webhook: push to main queues a build (202)" "$(hook -d '{"ref":"refs/heads/main"}' "$HOOK")" 202
LASTB=$(owner_sql "SELECT max(number) FROM builds b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'web' AND b.trigger = 'webhook'")
for _ in $(seq 1 90); do st=$(owner_sql "SELECT b.status FROM builds b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'web' AND b.number = ${LASTB:-0}"); [ "$st" = done ] || [ "$st" = failed ] && break; sleep 2; done
check "webhook build finished" "$st" done
code_of ja -X POST "$BASE$SVC_A/source" -d csrf=$CSRF_A --data-urlencode "repo=$REPO" -d branch=main -d dir=nope >/dev/null
OUT=$(pctl build shop/web --no-deploy --wait)
if echo "$OUT" | grep -q "folder nope not found"; then ok "missing folder reported clearly"; else bad "missing folder: $(echo "$OUT" | tail -2)"; fi
code_of ja -X POST "$BASE$SVC_A/source" -d csrf=$CSRF_A --data-urlencode "repo=$REPO" -d branch=node -d dir= >/dev/null
OUT=$(pctl build shop/web --wait)
if echo "$OUT" | grep -q "with Railpack (no Dockerfile)" && echo "$OUT" | grep -q "done: "; then ok "repository without a Dockerfile built with Railpack"; else bad "Railpack build failed: $(echo "$OUT" | tail -3)"; fi
check "the first Railpack build starts from the node's warm cache (no big download)" "$(grep -c "started from the node's warm cache" <<<"$OUT")" 1
check "Railpack-built app deployed and serving" "$(for _ in $(seq 1 45); do b=$(curl -s --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/"); echo "$b" | grep -q "built-with-railpack-$RUN" && { echo yes; break; }; sleep 2; done)" yes
# Zip deployment: the source uploaded as an archive, built and deployed
ZD="$TMP/zipapp"; mkdir -p "$ZD/site"
printf '%s\n' 'FROM nginx:1.27-alpine' 'COPY site/index.html /usr/share/nginx/html/index.html' > "$ZD/Dockerfile"
echo "built-from-zip-$RUN" > "$ZD/site/index.html"
OUT=$(pctl zip shop/web "$ZD" --wait)
if grep -q "build #[0-9]* done: .*/$A/web:zip-" <<<"$OUT"; then ok "[CLI] folder zipped, uploaded, built (image tagged zip-)"; else bad "zip deploy failed: $(tail -3 <<<"$OUT")"; fi
check "zip-built image deployed and serving" "$(for _ in $(seq 1 45); do b=$(curl -s --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/"); grep -q "built-from-zip-$RUN" <<<"$b" && { echo yes; break; }; sleep 2; done)" yes
check "the archive is dropped once built" "$(owner_sql "SELECT source_zip IS NULL AND source_name = 'zipapp.zip' AND length(source_sha256) = 64 FROM builds b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'web' AND b.trigger = 'zip' ORDER BY b.number DESC LIMIT 1")" t
# A repository's "Download ZIP": one top-level folder, no Dockerfile (Railpack)
python -c 'import sys, zipfile
z = zipfile.ZipFile(sys.argv[1], "w")
z.writestr("app-main/package.json", "{ \"name\": \"zipnode\", \"version\": \"1.0.0\", \"scripts\": { \"start\": \"node server.js\" } }")
z.writestr("app-main/server.js", "require(\"http\").createServer((q, s) => s.end(\"zip-railpack-" + sys.argv[2] + "\")).listen(process.env.PORT || 3000);")
z.close()' "$(cygpath -w "$TMP/app-main.zip")" "$RUN"
check "[A] zip uploaded in the console (folder archive, no Dockerfile)" "$(code_of ja -X POST "$BASE$SVC_A/zip" -F csrf=$CSRF_A -F deploy=1 -F "archive=@$(cygpath -w "$TMP/app-main.zip");type=application/zip")" 303
check "the folder archive is built with Railpack and served" "$(for _ in $(seq 1 150); do b=$(curl -s --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/"); grep -q "zip-railpack-$RUN" <<<"$b" && { echo yes; break; }; sleep 2; done)" yes
zapi() { curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/zip' --data-binary "@$2" "$BASE/api/v1/projects/shop/services/web/zip"; }
echo "not a zip" > "$TMP/bad.zip"
check "[API] an invalid archive is refused (422)" "$(zapi "$TOK_DEV" "$TMP/bad.zip")" 422
check "[B->A API] B cannot upload to a service only A has (404)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_B" -H 'Content-Type: application/zip' --data-binary "@$(cygpath -w "$TMP/app-main.zip")" "$BASE/api/v1/projects/shop/services/files/zip")" 404
check "zip builds audited" "$(owner_sql "SELECT count(*) >= 2 FROM audit_events WHERE action = 'build.queued' AND detail::text LIKE '%"zip"%' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
section "Preview per pull request"
(cd "$GITD/src" && git checkout -q -b pr-branch main && echo "pr-preview-$RUN" > index.html \
  && git -c user.email=e2e@example.test -c user.name=e2e commit -qam "pull request change" && git checkout -q main) >/dev/null
(cd "$GITD/srv/app.git" && git fetch -q "$GITD/src" pr-branch:pr-branch && git update-server-info) >/dev/null
pr_event() { # pr_event <action> <number> <head repo> [header]: GitHub-style pull request webhook
  printf '{"action":"%s","number":%s,"pull_request":{"head":{"ref":"pr-branch","sha":"0","repo":{"full_name":"%s"}},"base":{"repo":{"full_name":"acme/app"}}}}' "$1" "$2" "$3"; }
pr_builds() { owner_sql "SELECT count(*) FROM builds b JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND b.trigger = 'pull_request'"; }
PR_HOST=pr-7.web-shop-$A.apps.localhost
pr_page() { curl -s --resolve "$PR_HOST:$PORT:127.0.0.1" "http://$PR_HOST:$PORT/"; }
code_of ja -X POST "$BASE$SVC_A/source" -d csrf=$CSRF_A --data-urlencode "repo=$REPO" -d branch=main -d dir= >/dev/null
check "pull request ignored while previews are off (202, no build)" "$(hook -H 'X-GitHub-Event: pull_request' -d "$(pr_event opened 7 acme/app)" "$HOOK"):$(pr_builds)" 202:0
check "[A] pull request previews turned on" "$(code_of ja -X POST "$BASE$SVC_A/source" -d csrf=$CSRF_A --data-urlencode "repo=$REPO" -d branch=main -d dir= -d pr_previews=1)" 303
check "pull request from a fork ignored (no build, no preview)" "$(hook -H 'X-GitHub-Event: pull_request' -d "$(pr_event opened 7 someone-else/app)" "$HOOK"):$(pr_builds)" 202:0
check "opened pull request queues a preview build (202)" "$(hook -H 'X-GitHub-Event: pull_request' -d "$(pr_event opened 7 acme/app)" "$HOOK")" 202
check "preview pr-7 created for the pull request" "$(owner_sql "SELECT count(*) FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.preview_name = 'pr-7'")" 1
check "the pull request's branch is built and served at the preview address" "$(for _ in $(seq 1 90); do pr_page | grep -q "pr-preview-$RUN" && { echo yes; break; }; sleep 2; done)" yes
check "the service itself keeps its own version" "$(curl -s --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/" | grep -c "pr-preview-$RUN")" 0
if grep -q "pull request #7" <<<"$(c ja "$BASE$SVC_A")"; then ok "[A] Builds tab shows the pull request build"; else bad "pull request build not listed"; fi
check "a push to the pull request rebuilds its preview (Gitea event)" "$(hook -H 'X-Gitea-Event: pull_request' -d "$(pr_event synchronized 7 acme/app)" "$HOOK"):$(for _ in $(seq 1 90); do [ "$(pr_builds)" -ge 2 ] && [ "$(owner_sql "SELECT count(*) FROM builds b JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND b.trigger = 'pull_request' AND b.status IN ('queued', 'running')")" = 0 ] && { echo built; break; }; sleep 2; done)" 202:built
check "rebuilt preview still serves the branch" "$(for _ in $(seq 1 30); do pr_page | grep -q "pr-preview-$RUN" && { echo yes; break; }; sleep 2; done)" yes
check "closing the pull request deletes its preview (202)" "$(hook -H 'X-GitHub-Event: pull_request' -d "$(pr_event closed 7 acme/app)" "$HOOK")" 202
check "closed pull request's preview no longer served" "$(for _ in $(seq 1 30); do [ "$(curl -s -o /dev/null -w '%{http_code}' --resolve "$PR_HOST:$PORT:127.0.0.1" "http://$PR_HOST:$PORT/")" = 404 ] && { echo 404; break; }; sleep 2; done)" 404
check "preview deletion audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'preview.deleted' AND detail->>'preview' = 'pr-7' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" 1
code_of ja -X POST "$BASE$SVC_A/source" -d csrf=$CSRF_A -d repo= -d branch=main >/dev/null
check "builds audited" "$(owner_sql "SELECT count(*) >= 2 FROM audit_events WHERE action = 'build.done' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
docker rm -f e2e-git >/dev/null 2>&1

section "Self-signup and trials"
T=t$RUN; TEMAIL="owner@$T.test"; TPW=correct-horse-battery-trial
pow() { # pow <challenge>: nonce for SIGNUP_POW_BITS=8 (sha256 starting with two zero hex digits)
  python -c 'import hashlib,sys,itertools; c=sys.argv[1]; print(next(n for n in itertools.count() if hashlib.sha256(f"{c}:{n}".encode()).hexdigest()[:2] == "00"))' "$1" | tr -d '\r'; }
challenge() { c "$1" "$BASE/signup" | grep -o 'name="challenge" value="[^"]*"' | sed 's/.*value="//;s/"$//'; }
mail_body() { # mail_body <to> <subject text>: body of the newest matching message (Mailpit API)
  local id; for _ in $(seq 1 20); do
    id=$(curl -s -G "http://localhost:8025/api/v1/search" --data-urlencode "query=to:$1 subject:\"$2\"" | grep -o '"ID":"[^"]*"' | head -1 | cut -d'"' -f4)
    if [ -n "$id" ]; then curl -s "http://localhost:8025/api/v1/message/$id"; return; fi; sleep 1
  done; }
# Invitations by email: the person gets the join link and joins with it
INVM="invited-$RUN@example.org"
check "[A admin] invitation emailed" "$(c ja -X POST "$BASE/o/$A/invites" -d csrf=$CSRF_A -d "email=$INVM" -d role=viewer -d send_email=1 | grep -c "Invitation emailed to $INVM")" 1
IBODY=$(mail_body "$INVM" "invited you to")
ILINK=$(grep -o 'invite/[A-Za-z0-9_-]*' <<<"$IBODY" | head -1)
check "the email carries the join link and the role" "$([ -n "$ILINK" ] && echo yes):$(grep -c 'as viewer' <<<"$IBODY" | sed 's/^[1-9][0-9]*$/yes/')" "yes:yes"
check "the invited person joins with the emailed link" "$(code_of jinv -X POST "$BASE/$ILINK" --data-urlencode "name=Invited Viewer" --data-urlencode "password=correct-horse-battery-inv" --data-urlencode "password2=correct-horse-battery-inv"):$(owner_sql "SELECT m.role FROM memberships m JOIN users u ON u.id = m.user_id JOIN orgs o ON o.id = m.org_id WHERE o.slug = '$A' AND u.email = '$INVM'")" "303:viewer"
check "the emailed link works only once" "$(code_of jinv2 "$BASE/$ILINK")" 404
signup() { # signup <jar> <email> <slug> <challenge> <nonce> [extra curl args]: prints the page
  local jar=$1 em=$2 sl=$3 ch=$4 n=$5; shift 5
  c "$jar" -X POST "$BASE/signup" --data-urlencode "challenge=$ch" -d "nonce=$n" -d "name=Trial Owner" --data-urlencode "email=$em" \
    -d "password=$TPW" -d "password2=$TPW" -d "org_name=Trial $sl" -d "org_slug=$sl" "$@"; }
CH=$(challenge jt); N=$(pow "$CH")
if signup jt "$TEMAIL" "$T" "$CH" "$N" | grep -q "Check your email"; then ok "signup accepted, confirmation email requested"; else bad "signup form refused"; fi
LINK=$(mail_body "$TEMAIL" "Confirm your email" | grep -o "$BASE/signup/verify/[A-Za-z0-9_-]*" | head -1)
if [ -n "$LINK" ]; then ok "confirmation email delivered (SMTP)"; else bad "no confirmation email"; fi
if c jt "$LINK" | grep -q "Confirm and continue"; then ok "opening the link only shows a confirm button"; else bad "verify page missing"; fi
check "opening the link alone creates nothing" "$(owner_sql "SELECT count(*) FROM orgs WHERE slug = '$T'")" 0
check "replayed proof of work refused" "$(signup jt "x1@$T.test" "${T}x" "$CH" "$N" | grep -c 'browser check')" 1
check "[signup] confirmed: on to two-step login setup" "$(c jt -o /dev/null -w '%{redirect_url}' -X POST "$LINK")" "$BASE/totp/setup"
PAGE=$(c jt "$BASE/totp/setup"); SEC=$(echo "$PAGE" | grep -o 'class="select">[A-Z2-7]\{20,\}<' | head -1 | sed 's/.*">//;s/<$//')
check "[signup] two-step login enrolled" "$(code_of jt -X POST "$BASE/totp/setup" -d "csrf=$(echo "$PAGE" | csrf_of)" -d "code=$(admin totp-code --secret "$SEC" | tr -d '\r')")" 200
check "confirmation link works only once" "$(code_of jx "$LINK")" 404
check "new organization is on the Trial plan" "$(owner_sql "SELECT plan_id || ':' || (trial_ends_at > now() + interval '13 days') FROM orgs WHERE slug = '$T'")" "trial:true"
if c jt "$BASE/o/$T" | grep -q "Trial: 14 days left"; then ok "trial banner shows the days left"; else bad "trial banner missing"; fi
CH=$(challenge jx); N=$(pow "$CH")
if signup jx "spam@mailinator.com" "${T}d" "$CH" "$N" | grep -q "permanent email"; then ok "disposable email domain refused"; else bad "disposable email accepted"; fi
CH=$(challenge jx)
if signup jx "x2@$T.test" "${T}p" "$CH" "" | grep -q "browser check"; then ok "signup without proof of work refused"; else bad "missing proof of work accepted"; fi
CH=$(challenge jx); N=$(pow "$CH")
signup jx "bot@$T.test" "${T}h" "$CH" "$N" -d website=http://spam.example >/dev/null
check "honeypot: bot gets the same answer, nothing stored" "$(owner_sql "SELECT count(*) FROM signups WHERE email = 'bot@$T.test'")" 0
CH=$(challenge jx); N=$(pow "$CH")
if signup jx "$TEMAIL" "${T}e" "$CH" "$N" | grep -q "Check your email" && mail_body "$TEMAIL" "Your Platform account" | grep -q "already has an account"; then
  ok "existing address: same answer on screen, owner told by email"; else bad "existing-account handling"; fi
CSRF_T=$(c jt "$BASE/o/$T" | csrf_of)
code_of jt -X POST "$BASE/o/$T/projects" -d csrf=$CSRF_T -d slug=app -d name=App >/dev/null
code_of jt -X POST "$BASE/o/$T/p/app/services" -d csrf=$CSRF_T -d slug=burn -d image=nginx:1.27-alpine >/dev/null
SVC_T=/o/$T/p/app/s/burn
code_of jt -X POST "$BASE$SVC_T/source" -d csrf=$CSRF_T --data-urlencode "repo=https://github.com/example/app.git" -d branch=main >/dev/null
if c jt -L "$BASE$SVC_T/builds" -d csrf=$CSRF_T -d deploy=1 | grep -q "not included in your plan"; then ok "trial: builds from git refused"; else bad "trial could start a build"; fi
code_of jt -X POST "$BASE$SVC_T/domains" -d csrf=$CSRF_T -d hostname=shop.example.com >/dev/null
check "trial: custom domains refused" "$(owner_sql "SELECT count(*) FROM domains d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$T'")" 0
# A container that burns its whole CPU allowance, like a crypto miner (ANOMALY_MINUTES=1 in dev).
mkdir -p "$TMP/burn"; printf '%s\n' 'FROM nginx:1.27-alpine' 'CMD ["sh", "-c", "yes > /dev/null & exec nginx -g \"daemon off;\""]' > "$TMP/burn/Dockerfile"
docker build -q -t "$REG/$T/burn:1" "$(cygpath -w "$TMP/burn")" >/dev/null
TOK_T=$(new_token jt "$T" "$CSRF_T")
echo "$TOK_T" | dk login $REG -u "$T" --password-stdin >/dev/null; dk push "$REG/$T/burn:1" >/dev/null
code_of jt -X POST "$BASE$SVC_T/settings" -d csrf=$CSRF_T -d "image=$REG/$T/burn:1" -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[trial] CPU-burning app deployed" "$(wait_status jt $SVC_T)" live
for _ in $(seq 1 100); do R=$(owner_sql "SELECT suspend_reason FROM orgs WHERE slug = '$T'"); [ "$R" = cpu_anomaly ] && break; sleep 3; done
check "trial suspended automatically for sustained full CPU" "$R" cpu_anomaly
check "suspension stops the app" "$(for _ in $(seq 1 30); do x=$(curl -s -o /dev/null -w '%{http_code}' --resolve "burn-app-$T.apps.localhost:$PORT:127.0.0.1" "http://burn-app-$T.apps.localhost:$PORT/"); [ "$x" = 404 ] && { echo stopped; break; }; sleep 2; done)" stopped
if MSYS_NO_PATHCONV=1 docker exec -i "$NTFY_C" wget -qO- "http://localhost/platform-operator/json?poll=1&since=all" 2>/dev/null | grep -q "Trial suspended: $T"; then ok "operator notified"; else bad "no operator notice"; fi
if c jt "$BASE/o/$T" | grep -q "crypto-mining"; then ok "owner sees why the organization is suspended"; else bad "suspension reason not shown"; fi
admin suspend-org --slug "$T" --undo >/dev/null
owner_sql "UPDATE orgs SET trial_ends_at = now() - interval '1 minute' WHERE slug = '$T'" >/dev/null
for _ in $(seq 1 40); do R=$(owner_sql "SELECT suspend_reason FROM orgs WHERE slug = '$T'"); [ "$R" = trial_ended ] && break; sleep 3; done
check "expired trial suspended" "$R" trial_ended
if mail_body "$TEMAIL" "Your trial has ended" | grep -q "has ended"; then ok "owner emailed when the trial ends"; else bad "no trial-ended email"; fi
admin set-plan --slug "$T" --plan starter >/dev/null
check "upgrading lifts the trial suspension" "$(owner_sql "SELECT suspended::text || ':' || suspend_reason || ':' || (trial_ends_at IS NULL) FROM orgs WHERE slug = '$T'")" "false::true"
code_of jt -X POST "$BASE$SVC_T/delete" -d csrf=$CSRF_T -d confirm=burn >/dev/null

section "Alerts"
ntfy_has() { # ntfy_has <topic> <text> [tries]: wait until a message on topic contains text
  for _ in $(seq 1 "${3:-30}"); do
    if MSYS_NO_PATHCONV=1 docker exec -i "$NTFY_C" wget -qO- "http://localhost/$1/json?poll=1&since=all" 2>/dev/null | grep -q "$2"; then
      echo yes; return
    fi
    sleep 2
  done
  echo no
}
incident_status() { owner_sql "SELECT i.status FROM alert_incidents i JOIN services s ON s.id = i.service_id JOIN orgs o ON o.id = i.org_id
                               WHERE o.slug = '$A' AND s.slug = '$1' AND i.kind = '$2' ORDER BY i.started_at DESC LIMIT 1"; }
check "[A] ntfy channel added" "$(code_of ja -X POST "$BASE/o/$A/alerts/channels" -d csrf=$CSRF_A -d name=push -d kind=ntfy \
  --data-urlencode url=http://ntfy -d topic=alerts-$A -d events=service_down -d events=deploy_failed -d events=backup_failed -d events=critical_vulns)" 303
check "[A] webhook channel added" "$(code_of ja -X POST "$BASE/o/$A/alerts/channels" -d csrf=$CSRF_A -d name=hook -d kind=webhook \
  --data-urlencode url=http://ntfy/hook-$A -d secret=s3cret -d events=service_down -d events=deploy_failed)" 303
CH_PUSH=$(owner_sql "SELECT c.id FROM alert_channels c JOIN orgs o ON o.id = c.org_id WHERE o.slug = '$A' AND c.name = 'push'")
CH_HOOK=$(owner_sql "SELECT c.id FROM alert_channels c JOIN orgs o ON o.id = c.org_id WHERE o.slug = '$A' AND c.name = 'hook'")
code_of ja -X POST "$BASE/o/$A/alerts/channels/$CH_PUSH/test" -d csrf=$CSRF_A >/dev/null
code_of ja -X POST "$BASE/o/$A/alerts/channels/$CH_HOOK/test" -d csrf=$CSRF_A >/dev/null
check "[A] test push notification delivered" "$(ntfy_has "alerts-$A" "Test notification")" yes
check "[A] test webhook delivered as JSON" "$(ntfy_has "hook-$A" "console_url")" yes
check "[B->A] B cannot trigger A's channel through A's org (404)" "$(code_of jb -X POST "$BASE/o/$A/alerts/channels/$CH_PUSH/test" -d csrf=$CSRF_B)" 404
check "[B->A] B cannot use A's channel ID in its own org (404)" "$(code_of jb -X POST "$BASE/o/$B/alerts/channels/$CH_PUSH/test" -d csrf=$CSRF_B)" 404
check "[B->A] B cannot delete A's channel (404)" "$(code_of jb -X POST "$BASE/o/$B/alerts/channels/$CH_PUSH/delete" -d csrf=$CSRF_B)" 404

CID_F=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'files' AND d.status = 'live'")
docker pause "$CID_F" >/dev/null
ST=""; for _ in $(seq 1 30); do ST=$(incident_status files service_down); [ "$ST" = firing ] && break; sleep 2; done
check "[A] paused app detected: service_down incident open" "$ST" firing
check "[A] down alert pushed" "$(ntfy_has "alerts-$A" "Service down")" yes
check "[A] down alert sent to the webhook" "$(ntfy_has "hook-$A" "service_down")" yes
docker unpause "$CID_F" >/dev/null
ST=""; for _ in $(seq 1 30); do ST=$(incident_status files service_down); [ "$ST" = resolved ] && break; sleep 2; done
check "[A] recovery detected: incident resolved" "$ST" resolved
check "[A] resolved alert pushed" "$(ntfy_has "alerts-$A" "Resolved: Service down")" yes
if grep -q "Service down" <<<"$(c ja "$BASE/o/$A/monitoring")"; then ok "[A] incident listed in the console"; else bad "[A] incident missing from the console"; fi

code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d "image=nginx:no-such-tag-$RUN" -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[A] deployment with a missing image fails" "$(wait_status ja $SVC_A)" failed
check "[A] failed deployment pushed" "$(ntfy_has "alerts-$A" "Deployment failed")" yes

# Regression: a recreated agent (every upgrade) must re-attach to tenant networks,
# not report healthy apps as down.
docker compose -f compose.dev.yml up -d --force-recreate --no-deps agent >/dev/null 2>&1
sleep 35
check "[agent recreated] healthy apps not reported down" "$(incident_status files service_down)" resolved
check "[agent recreated] app still reachable through the proxy" \
  "$(curl -s -o /dev/null -w '%{http_code}' --resolve "web-shop-$B.apps.localhost:$PORT:127.0.0.1" "http://web-shop-$B.apps.localhost:$PORT/")" 200

section "Registry cleanup"
echo "$TOK_A" | dk login $REG -u "$A" --password-stdin >/dev/null
docker tag nginx:1.27-alpine "$REG/$A/old:1"; docker tag alpine:3.20 "$REG/$A/fresh:1" 2>/dev/null || docker tag nginx:1.27-alpine "$REG/$A/fresh:1"
check "[A] pushed an old and a fresh tag" "$(dk push "$REG/$A/old:1")$(dk push "$REG/$A/fresh:1")" yesyes
for _ in $(seq 1 20); do [ "$(owner_sql "SELECT count(*) FROM image_scans WHERE image = '$REG/$A/old:1'")" -ge 1 ] && break; sleep 1; done
owner_sql "UPDATE image_scans SET created_at = now() - interval '60 days' WHERE image = '$REG/$A/old:1'" >/dev/null
owner_sql "SELECT pg_notify('deployments', 'registry-clean')" >/dev/null
for _ in $(seq 1 30); do [ "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'registry.cleaned' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" -ge 1 ] && break; sleep 2; done
check "old unused tag deleted by the daily cleanup" "$(dk pull "$REG/$A/old:1")" no
check "recently pushed tag kept" "$(dk pull "$REG/$A/fresh:1")" yes
check "tag used by a deployment kept" "$(dk pull "$REG/$A/web:v1")" yes
check "cleanup audited" "$(owner_sql "SELECT detail->>'tags_deleted' FROM audit_events WHERE action = 'registry.cleaned' AND org_id = (SELECT id FROM orgs WHERE slug = '$A') ORDER BY id DESC LIMIT 1")" 1

section "Disk limits"
owner_sql "INSERT INTO plans (id, name, max_services, max_cpu_millis, max_memory_mb, max_domains, max_databases, max_disk_mb)
           VALUES ('tiny', 'Tiny (test)', 20, 8000, 8192, 5, 5, 1) ON CONFLICT (id) DO NOTHING" >/dev/null
admin set-plan --slug "$A" --plan tiny >/dev/null
owner_sql "UPDATE orgs SET disk_alert_level = 0, disk_measured_at = NULL WHERE slug = '$A'" >/dev/null
owner_sql "SELECT pg_notify('deployments', 'disk')" >/dev/null
for _ in $(seq 1 30); do [ -n "$(owner_sql "SELECT disk_measured_at FROM orgs WHERE slug = '$A'")" ] && break; sleep 2; done
check "disk use measured by the agent" "$(owner_sql "SELECT disk_measured_at IS NOT NULL AND disk_used_mb > 0 FROM orgs WHERE slug = '$A'")" t
if grep -q "[0-9]* / 1 MB" <<<"$(c ja "$BASE/o/$A")"; then ok "[A] console home shows disk used / allowed"; else bad "disk usage not shown"; fi
# 500% of the 1 MB test plan (per-node figure too, so a measurement pass in between keeps it)
# The fake 5 MB sit on a node row of their own, which measurement passes never rewrite.
owner_sql "INSERT INTO node_disk (node_id, org_id, used_mb, measured_at) SELECT 'e2e-fake', id, 5, now() FROM orgs WHERE slug = '$A' ON CONFLICT (node_id, org_id) DO UPDATE SET used_mb = 5, measured_at = now(); UPDATE orgs SET disk_used_mb = disk_used_mb + 5 WHERE slug = '$A'" >/dev/null
check "owner alerted when over the disk limit" "$(owner_sql "SELECT count(*) FROM alert_incidents i JOIN orgs o ON o.id = i.org_id WHERE o.slug = '$A' AND i.kind = 'disk_quota'")" 1
check "[API] deploy refused above 110% of the disk limit (507)" "$(api_code "$TOK_DEV" -X POST "$BASE/api/v1/projects/shop/services/web/deploy")" 507
if c ja -L "$BASE$SVC_A/deploy" -d csrf=$CSRF_A | grep -q "Disk limit exceeded"; then ok "[A] console explains why the deploy is refused"; else bad "console disk message missing"; fi
admin set-plan --slug "$A" --plan pro >/dev/null
check "[API] deploys work again within the limit" "$(api_code "$TOK_DEV" -X POST "$BASE/api/v1/projects/shop/services/web/deploy")" 202
owner_sql "DELETE FROM node_disk WHERE node_id = 'e2e-fake'" >/dev/null

section "Operator console"
check "the console is hidden from non-operators (404)" "$(code_of ja "$BASE/operator")" 404
admin make-operator --email "owner@$A.test" >/dev/null
check "[A] operator console opens for an operator" "$(code_of ja "$BASE/operator")" 200
if c ja "$BASE/o/$A" | grep -q 'href="/operator"'; then ok "operator link shown in the header"; else bad "operator link missing"; fi
check "[B] still no access for other users (404)" "$(code_of jb "$BASE/operator")" 404
OPCSRF=$(c ja "$BASE/operator" | csrf_of)
check "operator changes an organization's plan" "$(code_of ja -X POST "$BASE/operator/orgs/$B/plan" -d csrf=$OPCSRF -d plan=pro)" 303
check "plan changed in the database" "$(owner_sql "SELECT plan_id FROM orgs WHERE slug = '$B'")" pro
code_of ja -X POST "$BASE/operator/orgs/$B/suspend" -d csrf=$OPCSRF -d on=1 >/dev/null
check "operator suspends an organization" "$(owner_sql "SELECT suspended::text || ':' || suspend_reason FROM orgs WHERE slug = '$B'")" "true:operator"
code_of ja -X POST "$BASE/operator/orgs/$B/suspend" -d csrf=$OPCSRF >/dev/null
check "operator restores it" "$(owner_sql "SELECT suspended::text FROM orgs WHERE slug = '$B'")" false
check "operator actions audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action IN ('operator.set_plan', 'operator.suspend', 'operator.restore') AND target = 'org:$B'")" 3
admin set-plan --slug "$B" --plan starter >/dev/null
NODE=$(owner_sql "SELECT id FROM nodes ORDER BY last_seen DESC LIMIT 1")
check "the server reports itself as a node (heartbeat)" "$(owner_sql "SELECT (last_seen > now() - interval '2 minutes') AND cpus > 0 AND mem_mb > 0 FROM nodes WHERE id = '$NODE'")" t
check "node health: API, database and proxy answer" "$(owner_sql "SELECT (services->>'api') || (services->>'database') || (services->>'proxy') FROM nodes WHERE id = '$NODE'")" okokok
OPPAGE=$(c ja "$BASE/operator")
if grep -q "op-nodes" <<<"$OPPAGE" && grep -q "progress class=\"meter\"" <<<"$OPPAGE"; then ok "operator Nodes tab shows capacity meters"; else bad "Nodes tab missing"; fi
if grep -q "/operator/orgs/$A\">$A</a>" <<<"$OPPAGE"; then ok "operator Usage tab lists every organization"; else bad "Usage tab missing"; fi
check "operator cordons a node" "$(code_of ja -X POST "$BASE/operator/nodes/$NODE" -d csrf=$OPCSRF -d cordoned=1 -d cluster=default)" 303
check "node marked cordoned" "$(owner_sql "SELECT cordoned FROM nodes WHERE id = '$NODE'")" t
code_of ja -X POST "$BASE/operator/nodes/$NODE" -d csrf=$OPCSRF -d cluster=default >/dev/null
check "and uncordons it" "$(owner_sql "SELECT cordoned FROM nodes WHERE id = '$NODE'")" f
ORGPAGE=$(c jb "$BASE/operator/orgs/$A")
check "[B] cannot open the operator org page (404)" "$(code_of jb "$BASE/operator/orgs/$A")" 404
ORGPAGE=$(c ja "$BASE/operator/orgs/$B")
if echo "$ORGPAGE" | grep -q "shop/web" && echo "$ORGPAGE" | grep -q "Operator view"; then ok "operator sees an organization's services and usage"; else bad "operator org page incomplete"; fi
APAGE=$(c ja "$BASE/operator/orgs/$A")
if echo "$APAGE" | grep -q "tenant-a-secret\|GREETING=hello\|API_KEY"; then bad "operator page leaks secrets or environment"; else ok "operator page shows no secrets or environment values"; fi
check "measured CPU and memory recorded per service" "$(owner_sql "SELECT count(*) > 0 FROM service_stats st JOIN orgs o ON o.id = st.org_id WHERE o.slug = '$A' AND st.mem_mb > 0")" t
check "operator visits audited" "$(owner_sql "SELECT count(*) >= 2 FROM audit_events WHERE action = 'operator.viewed_org'")" t

section "Operator: user management"
check "[B] the users page is hidden from non-operators (404)" "$(code_of jb "$BASE/operator/users")" 404
check "the users page finds an account by email" "$(c ja "$BASE/operator/users?q=owner@$B.test" | grep -c "owner@$B.test" | sed 's/^[1-9][0-9]*$/yes/')" yes
NEWU="newbie-$RUN@example.test"; NEWO="nw$RUN"
UPAGE=$(c ja -X POST "$BASE/operator/users" -d csrf=$OPCSRF --data-urlencode "email=$NEWU" -d org_mode=new -d "new_slug=$NEWO" -d "new_name=Newbie $RUN" -d plan=starter)
ULINK=$(grep -o "$BASE/invite/[A-Za-z0-9_-]*" <<<"$UPAGE" | head -1)
check "invitation into a new organization: link shown once" "$([ -n "$ULINK" ] && echo yes)" yes
check "the organization was created on the chosen plan with an owner invitation" "$(owner_sql "SELECT o.plan_id || ':' || i.role FROM orgs o JOIN invites i ON i.org_id = o.id WHERE o.slug = '$NEWO' AND i.email = '$NEWU'")" "starter:owner"
check "the invitation is pending in the list" "$(c ja "$BASE/operator/users" | grep -c "$NEWU" | sed 's/^[1-9][0-9]*$/yes/')" yes
check "the invitation link opens" "$(code_of jx "$ULINK")" 200
UB=$(owner_sql "SELECT id FROM users WHERE email = 'owner@$B.test'")
check "an existing account is added to the organization directly" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/operator/users" -d csrf=$OPCSRF --data-urlencode "email=owner@$B.test" -d org_mode=existing -d "org_slug=$NEWO" -d role=viewer | grep -c "/operator/users/$UB"):$(owner_sql "SELECT m.role FROM memberships m JOIN orgs o ON o.id = m.org_id WHERE o.slug = '$NEWO' AND m.user_id = '$UB'")" "1:viewer"
check "the user page lists memberships and activity" "$(c ja "$BASE/operator/users/$UB" | grep -o -e "/operator/orgs/$B\"" -e "/operator/orgs/$NEWO\"" -e 'member.added_by_operator' | sort -u | wc -l | tr -d ' ')" 3
code_of ja -X POST "$BASE/operator/users/$UB/membership" -d csrf=$OPCSRF -d "org=$NEWO" -d action=role -d role=developer >/dev/null
check "operator changes the role" "$(owner_sql "SELECT m.role FROM memberships m JOIN orgs o ON o.id = m.org_id WHERE o.slug = '$NEWO' AND m.user_id = '$UB'")" developer
check "the only owner cannot be removed" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/operator/users/$UB/membership" -d csrf=$OPCSRF -d "org=$B" -d action=remove | grep -c 'only+owner')" 1
code_of ja -X POST "$BASE/operator/users/$UB/membership" -d csrf=$OPCSRF -d "org=$NEWO" -d action=remove >/dev/null
check "operator removes the membership" "$(owner_sql "SELECT count(*) FROM memberships m JOIN orgs o ON o.id = m.org_id WHERE o.slug = '$NEWO' AND m.user_id = '$UB'")" 0
code_of ja -X POST "$BASE/operator/users/$UB/disable" -d csrf=$OPCSRF -d on=1 -d "reason=e2e check" >/dev/null
check "a disabled account is signed out at once" "$(code_of jb "$BASE/o/$B")" 303
check "and cannot sign in, even with the right password" "$(c jdis -o /dev/null -w '%{http_code}' -X POST "$BASE/login" --data-urlencode "email=owner@$B.test" --data-urlencode "password=correct-horse-battery-$B")" 403
code_of ja -X POST "$BASE/operator/users/$UB/disable" -d csrf=$OPCSRF -d on=0 >/dev/null
check "enabled again: the password works" "$(c jdis -o /dev/null -w '%{redirect_url}' -X POST "$BASE/login" --data-urlencode "email=owner@$B.test" --data-urlencode "password=correct-horse-battery-$B")" "$BASE/totp/verify"
check "password reset link created by the operator" "$(c ja -X POST "$BASE/operator/users/$UB/reset-password" -d csrf=$OPCSRF | grep -c "$BASE/password/reset/" | sed 's/^[1-9][0-9]*$/yes/')" yes
check "two-step login reset" "$(code_of ja -X POST "$BASE/operator/users/$UB/reset-2fa" -d csrf=$OPCSRF):$(owner_sql "SELECT totp_enabled FROM users WHERE id = '$UB'")" "303:f"
UA=$(owner_sql "SELECT id FROM users WHERE email = 'owner@$A.test'")
check "an operator cannot disable their own account" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/operator/users/$UA/disable" -d csrf=$OPCSRF -d on=1 | grep -c 'own+account'):$(owner_sql "SELECT disabled_at IS NULL FROM users WHERE id = '$UA'")" "1:t"
check "operator rights need two-step login" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/operator/users/$UB/operator" -d csrf=$OPCSRF -d on=1 | grep -c 'two-step'):$(owner_sql "SELECT is_operator FROM users WHERE id = '$UB'")" "1:f"
check "user management actions audited" "$(owner_sql "SELECT count(DISTINCT action) FROM audit_events WHERE target = 'user:$UB' AND action IN ('user.disabled', 'user.enabled', 'user.password_reset_by_operator', 'totp.reset_by_operator', 'member.role_changed_by_operator')")" 5
code_of ja -X POST "$BASE/operator/users/$UB/profile" -d csrf=$OPCSRF -d "name=Owner of $B" --data-urlencode "email=owner@$A.test" >/dev/null
check "an email already in use is refused" "$(owner_sql "SELECT email FROM users WHERE id = '$UB'")" "owner@$B.test"
# B signs in again: its sessions were ended and its two-step login reset above
c jb -o /dev/null -X POST "$BASE/login" --data-urlencode "email=owner@$B.test" --data-urlencode "password=correct-horse-battery-$B"
B_SETUP=$(c jb "$BASE/totp/setup"); B_SEC=$(grep -o 'class="select">[A-Z2-7]\{20,\}<' <<<"$B_SETUP" | head -1 | sed 's/.*">//;s/<$//')
c jb -o /dev/null -X POST "$BASE/totp/setup" -d "csrf=$(csrf_of <<<"$B_SETUP")" -d "code=$(admin totp-code --secret "$B_SEC" | tr -d '
')"
check "[B] signs in again with a new second factor" "$(code_of jb "$BASE/o/$B")" 200
CSRF_B=$(c jb "$BASE/o/$B" | csrf_of)
NEWUID=$(owner_sql "INSERT INTO users (email, name, password_hash) VALUES ('gone-$RUN@example.test', 'Gone', 'x') RETURNING id")
check "operator deletes an account" "$(code_of ja -X POST "$BASE/operator/users/$NEWUID/delete" -d csrf=$OPCSRF -d "confirm=gone-$RUN@example.test"):$(owner_sql "SELECT count(*) FROM users WHERE id = '$NEWUID'")" "303:0"

section "Operator: admin panel (settings, plans, resources, health)"
for p in settings plans resources health; do check "[B] /operator/$p hidden from non-operators (404)" "$(code_of jb "$BASE/operator/$p")" 404; done
check "every admin page opens" "$(for p in settings plans resources health; do code_of ja "$BASE/operator/$p"; echo; done | sort -u | tr -d '\n')" 200
SET_BASE=(-d csrf=$OPCSRF -d banner_level=warning -d signup_enabled= -d signup_daily_limit= -d signup_trial_days= -d signup_allowed_domains_env=1 -d signup_blocked_domains_env=1)
check "announcement saved" "$(code_of ja -X POST "$BASE/operator/settings" "${SET_BASE[@]}" --data-urlencode "banner_message=Maintenance $RUN tonight")" 303
banner_n() { c jb "$BASE/o/$B" | grep -c "Maintenance $RUN tonight" | sed 's/^[1-9][0-9]*$/yes/'; }
check "the announcement shows on tenant pages" "$(until_is yes 15 banner_n)" yes
check "[A] deployments paused" "$(code_of ja -X POST "$BASE/operator/settings" "${SET_BASE[@]}" -d deploys_paused=1)" 303
code_of ja -X POST "$BASE/o/$A/p/shop/s/web/deploy" -d csrf=$CSRF_A >/dev/null
sleep 4
check "a deployment waits while paused" "$(owner_sql "SELECT d.status FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'web' ORDER BY d.revision DESC LIMIT 1")" pending
check "health shows the waiting deployment" "$(c ja "$BASE/operator/health" | grep -c 'Deployments are paused' | sed 's/^[1-9][0-9]*$/yes/')" yes
code_of ja -X POST "$BASE/operator/settings" "${SET_BASE[@]}" >/dev/null
check "resumed: the waiting deployment is picked up" "$(for _ in $(seq 1 30); do st=$(owner_sql "SELECT d.status FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'web' ORDER BY d.revision DESC LIMIT 1"); [ "$st" != pending ] && { echo picked; break; }; sleep 2; done)" picked
check "the announcement is gone" "$(until_is 0 15 banner_n)" 0
check "signup closed by the operator" "$(code_of ja -X POST "$BASE/operator/settings" -d csrf=$OPCSRF -d banner_level=info -d signup_enabled=off -d signup_allowed_domains_env=1 -d signup_blocked_domains_env=1):$(until_is 404 15 code_of jx "$BASE/signup")" "303:404"
code_of ja -X POST "$BASE/operator/settings" -d csrf=$OPCSRF -d banner_level=info -d signup_allowed_domains_env=1 -d signup_blocked_domains_env=1 >/dev/null
check "settings changes audited" "$(owner_sql "SELECT count(*) >= 4 FROM audit_events WHERE action = 'platform.settings_changed'")" t
PLAN=e2e$RUN
check "new plan copied from starter" "$(code_of ja -X POST "$BASE/operator/plans" -d csrf=$OPCSRF -d "id=$PLAN" -d from=starter):$(owner_sql "SELECT max_services = (SELECT max_services FROM plans WHERE id = 'starter') FROM plans WHERE id = '$PLAN'")" "303:t"
PFORM=$(c ja "$BASE/operator/plans" | python -c '
import sys, re, html
page = sys.stdin.read()
sec = page[page.index("id=\"plan-'"$PLAN"'\""):]
sec = sec[:sec.index("</form>")]
out = []
for m in re.finditer(r"<input name=\"([a-z_]+)\" value=\"([^\"]*)\"", sec):
    k, v = m.group(1), html.unescape(m.group(2))
    if k == "max_services": v = "7"
    if k == "price_monthly_cents": v = "1234"
    out.append("%s=%s" % (k, v))
print("&".join(out))')
check "plan limits and prices saved" "$(code_of ja -X POST "$BASE/operator/plans/$PLAN" --data "csrf=$OPCSRF&$PFORM"):$(owner_sql "SELECT max_services || ':' || price_monthly_cents FROM plans WHERE id = '$PLAN'")" "303:7:1234"
check "a negative limit is refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/operator/plans/$PLAN" --data "csrf=$OPCSRF&${PFORM/max_services=7/max_services=-1}" | grep -c 'err=')" 1
check "a built-in plan cannot be deleted" "$(code_of ja -X POST "$BASE/operator/plans/starter/delete" -d csrf=$OPCSRF >/dev/null; owner_sql "SELECT count(*) FROM plans WHERE id = 'starter'")" 1
check "an unused plan is deleted" "$(code_of ja -X POST "$BASE/operator/plans/$PLAN/delete" -d csrf=$OPCSRF):$(owner_sql "SELECT count(*) FROM plans WHERE id = '$PLAN'")" "303:0"
RES=$(c ja "$BASE/operator/resources?q=$A")
check "resources list A's services" "$(grep -c '<td>service</td>' <<<"$RES" | sed 's/^[1-9][0-9]*$/yes/')" yes
if grep -q "tenant-a-secret\|API_KEY" <<<"$RES"; then bad "resources page leaks secrets"; else ok "resources page shows no secrets"; fi
check "invoice generation requested from the panel" "$(code_of ja -X POST "$BASE/operator/invoices/generate" -d csrf=$OPCSRF -d "month=$(date -u -d "$(date -u +%Y-%m-01) -1 day" +%Y-%m)")" 303
check "a future month is refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/operator/invoices/generate" -d csrf=$OPCSRF -d "month=$(date -u -d '+1 month' +%Y-%m)" | grep -c 'err=')" 1

section "Fine-grained access (project-scoped members and tokens)"
code_of ja -X POST "$BASE/o/$A/projects" -d csrf=$CSRF_A -d slug=hidden -d name=Hidden >/dev/null
SDEV="scoped-$RUN@$A.test"
SLINK=$(admin invite --org "$A" --email "$SDEV" --role developer | tail -1 | tr -d '\r')
code_of jsd -X POST "$BASE/invite/${SLINK##*/invite/}" --data-urlencode "name=Scoped Dev" --data-urlencode "password=correct-horse-battery-scoped" --data-urlencode "password2=correct-horse-battery-scoped" >/dev/null
SPAGE=$(c jsd "$BASE/totp/setup"); SSEC=$(grep -o 'class="select">[A-Z2-7]\{20,\}<' <<<"$SPAGE" | head -1 | sed 's/.*">//;s/<$//')
c jsd -o /dev/null -X POST "$BASE/totp/setup" -d "csrf=$(csrf_of <<<"$SPAGE")" -d "code=$(admin totp-code --secret "$SSEC" | tr -d '\r')"
check "[scoped] developer joined A" "$(code_of jsd "$BASE/o/$A/compute")" 200
SUID=$(owner_sql "SELECT id FROM users WHERE email = '$SDEV'")
check "[A admin] access limited to project shop (developer)" "$(code_of ja -X POST "$BASE/o/$A/members/$SUID/access" -d csrf=$CSRF_A -d project_shop=developer)" 303
check "[scoped] sees project shop" "$(code_of jsd "$BASE/o/$A/p/shop")" 200
check "[scoped] another project is hidden (404)" "$(code_of jsd "$BASE/o/$A/p/hidden")" 404
check "[scoped] organization-wide pages are hidden (404)" "$(code_of jsd "$BASE/o/$A/compute"):$(code_of jsd "$BASE/o/$A/iam"):$(code_of jsd "$BASE/o/$A/billing")" "404:404:404"
check "[scoped] the organization home lists only shop" "$(c jsd "$BASE/o/$A/projects" | grep -c "/o/$A/p/hidden\""):$(c jsd "$BASE/o/$A/projects" | grep -c "/o/$A/p/shop\"" | sed 's/^[1-9][0-9]*$/yes/')" "0:yes"
SCSRF=$(c jsd "$BASE/o/$A/p/shop" | csrf_of)
check "[scoped] can deploy in shop (developer)" "$(code_of jsd -X POST "$BASE/o/$A/p/shop/s/web/deploy" -d csrf=$SCSRF)" 303
code_of ja -X POST "$BASE/o/$A/members/$SUID/access" -d csrf=$CSRF_A -d project_shop=viewer >/dev/null
check "[scoped viewer] cannot deploy in shop (403)" "$(code_of jsd -X POST "$BASE/o/$A/p/shop/s/web/deploy" -d csrf=$SCSRF)" 403
check "[A admin] an admin cannot be limited to projects" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/members/$(owner_sql "SELECT id FROM users WHERE email = 'owner@$A.test'")/access" -d csrf=$CSRF_A -d project_shop=viewer | grep -c 'err=')" 1
PTOK=$(c ja -X POST "$BASE/o/$A/api-tokens" -d csrf=$CSRF_A -d name=shop-only -d role=developer -d project=shop | grep -o 'pla_[A-Za-z0-9_-]*' | head -1)
papi() { curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $PTOK" "$BASE/api/v1$1"; }
check "[project token] reaches its project" "$(papi /projects/shop/services):$(papi /me)" "200:200"
check "[project token] other projects and organization APIs refused (403)" "$(papi /projects/hidden/services):$(papi /queues):$(papi /projects)" "403:403:403"
check "[project token] an admin token cannot be limited to a project" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/api-tokens" -d csrf=$CSRF_A -d name=bad -d role=admin -d project=shop | grep -c 'err=')" 1
code_of ja -X POST "$BASE/o/$A/members/$SUID/access" -d csrf=$CSRF_A >/dev/null
check "[A admin] access back to the whole organization" "$(code_of jsd "$BASE/o/$A/compute"):$(code_of jsd "$BASE/o/$A/p/hidden")" "200:200"
check "access changes audited" "$(owner_sql "SELECT count(*) >= 3 FROM audit_events WHERE action = 'member.access_changed' AND target = 'user:$SUID'")" t
code_of ja -X POST "$BASE/o/$A/members/$SUID/remove" -d csrf=$CSRF_A >/dev/null
code_of ja -X POST "$BASE/o/$A/p/hidden/delete" -d csrf=$CSRF_A -d confirm=hidden >/dev/null

section "DNS hosting and edge cache"
ZONE=z$RUN.example
ask_dns() { MSYS_NO_PATHCONV=1 docker run --rm --network platform-core alpine:3.20 nslookup "-type=$2" "$1" platform-agent 2>&1; }
check "[A] zone added (pending until delegated)" "$(code_of ja -X POST "$BASE/o/$A/dns" -d csrf=$CSRF_A -d "domain=$ZONE")" 303
check "[B] the same domain cannot be taken by another organization" "$(c jb -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$B/dns" -d csrf=$CSRF_B -d "domain=$ZONE" | grep -c 'already')" 1
check "the platform's own domain cannot be hosted" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/dns" -d csrf=$CSRF_A -d "domain=apps.localhost" | grep -c 'err=')" 1
zrec() { code_of ja -X POST "$BASE/o/$A/dns/$ZONE" -d csrf=$CSRF_A -d action=add "$@"; }
zerr() { c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/dns/$ZONE" -d csrf=$CSRF_A -d action=add "$@" | grep -c 'err='; }
check "records added (A, TXT, MX)" "$(zrec -d name=@ -d type=A -d value=203.0.113.5):$(zrec -d name=@ -d type=TXT --data-urlencode "value=v=spf1 -all $RUN"):$(zrec -d name=@ -d type=MX -d value=mail.$ZONE -d priority=10)" "303:303:303"
check "a name pointed at a service (CNAME)" "$(code_of ja -X POST "$BASE/o/$A/dns/$ZONE" -d csrf=$CSRF_A -d action=service -d name=www -d "host=web-shop-$A.apps.localhost")" 303
check "invalid records refused (bad address, CNAME at the apex, CNAME next to another record)" "$(zerr -d name=@ -d type=A -d value=999.1.1.1):$(zerr -d name=@ -d type=CNAME -d value=x.example.org):$(zerr -d name=www -d type=TXT -d value=x)" "1:1:1"
check "a pending zone is not served (REFUSED)" "$(ask_dns "$ZONE" A | grep -c REFUSED)" 1
owner_sql "UPDATE dns_zones SET status = 'active', active_at = now() WHERE domain = '$ZONE'; SELECT pg_notify('deployments', 'dns')" >/dev/null   # stands in for the registrar delegation
sleep 2
check "active zone: the A record answers" "$(for _ in $(seq 1 10); do ask_dns "$ZONE" A | grep -q '203.0.113.5' && { echo yes; break; }; sleep 1; done)" yes
check "the CNAME, TXT and MX records answer" "$(ask_dns "www.$ZONE" CNAME | grep -c "web-shop-$A.apps.localhost"):$(ask_dns "$ZONE" TXT | grep -c "v=spf1 -all $RUN"):$(ask_dns "$ZONE" MX | grep -c "mail.$ZONE")" "1:1:1"
check "the platform's name servers answer for the zone" "$(ask_dns "$ZONE" NS | grep -c 'ns[12].platform.test')" 2
check "an unknown name answers NXDOMAIN" "$(ask_dns "nothing.$ZONE" A | grep -c NXDOMAIN)" 1
check "a domain not hosted here is refused (not a resolver)" "$(ask_dns example.org A | grep -c REFUSED)" 1
check "[B->A] B cannot open A's zone (404)" "$(code_of jb "$BASE/o/$A/dns/$ZONE")" 404
check "[API] records listed" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/dns/zones/$ZONE/records" | grep -o '"Type":"[A-Z]*"' | sort -u | wc -l | tr -d ' ')" 4
# Automatic DNS: domains inside a hosted zone get their records from the platform
admin set-plan --slug "$A" --plan pro >/dev/null
check "[A] a custom domain in the hosted zone is set up and verified at once" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/shop/s/web/domains" -d csrf=$CSRF_A -d "hostname=shop.$ZONE" | grep -c 'is+verified'):$(owner_sql "SELECT verified_at IS NOT NULL FROM domains WHERE hostname = 'shop.$ZONE'")" "1:t"
check "its records were written (CNAME to the service, verification TXT), marked automatic" "$(owner_sql "SELECT string_agg(type || ' ' || name, ',' ORDER BY type) FROM dns_records r JOIN dns_zones z ON z.id = r.zone_id WHERE z.domain = '$ZONE' AND r.managed_by LIKE 'domain:%'"):$(c ja "$BASE/o/$A/dns/$ZONE" | grep -c 'automatic · custom domain' | sed 's/^[1-9][0-9]*$/yes/')" "CNAME shop,TXT _platform-challenge.shop:yes"
check "the name answers on the platform's DNS" "$(sleep 2; ask_dns "shop.$ZONE" CNAME | grep -c "web-shop-$A.apps.localhost")" 1
check "the service's Domains tab says the platform handles it" "$(c ja "$BASE/o/$A/p/shop/s/web" | grep -c "DNS handled by the platform in your hosted zone")" 1
check "[A] an email domain in the hosted zone gets its records and is verified" "$(code_of ja -X POST "$BASE/o/$A/email/domains" -d csrf=$CSRF_A -d "domain=$ZONE"):$(owner_sql "SELECT status FROM email_domains WHERE domain = '$ZONE'"):$(owner_sql "SELECT count(*) FROM dns_records r JOIN dns_zones z ON z.id = r.zone_id WHERE z.domain = '$ZONE' AND r.managed_by LIKE 'email:%'")" "303:verified:3"
check "a person's own SPF record is kept (no second SPF)" "$(owner_sql "SELECT count(*) FROM dns_records r JOIN dns_zones z ON z.id = r.zone_id WHERE z.domain = '$ZONE' AND r.name = '@' AND r.value LIKE 'v=spf1%'")" 1
check "removing the domain removes its records" "$(code_of ja -X POST "$BASE/o/$A/p/shop/s/web/domains/$(owner_sql "SELECT id FROM domains WHERE hostname = 'shop.$ZONE'")/delete" -d csrf=$CSRF_A):$(owner_sql "SELECT count(*) FROM dns_records WHERE managed_by LIKE 'domain:%' AND zone_id = (SELECT id FROM dns_zones WHERE domain = '$ZONE')")" "303:0"
EMDID=$(owner_sql "SELECT id FROM email_domains WHERE domain = '$ZONE'")
code_of ja -X POST "$BASE/o/$A/email/d/$ZONE/delete" -d csrf=$CSRF_A -d "confirm=$ZONE" >/dev/null
check "so does removing the email domain" "$(owner_sql "SELECT count(*) FROM dns_records WHERE managed_by = 'email:$EMDID'")" 0
LATEZ=late$RUN.example
code_of ja -X POST "$BASE/o/$A/p/shop/s/web/domains" -d csrf=$CSRF_A -d "hostname=www.$LATEZ" >/dev/null
check "a zone added after its domain writes the domain's records too" "$(code_of ja -X POST "$BASE/o/$A/dns" -d csrf=$CSRF_A -d "domain=$LATEZ"):$(owner_sql "SELECT count(*) FROM dns_records r JOIN dns_zones z ON z.id = r.zone_id WHERE z.domain = '$LATEZ' AND r.managed_by LIKE 'domain:%'")" "303:2"
check "the pending zone's domain is verified when the delegation is confirmed" "$(owner_sql "SELECT verified_at IS NULL FROM domains WHERE hostname = 'www.$LATEZ'")" t
code_of ja -X POST "$BASE/o/$A/dns/$LATEZ" -d csrf=$CSRF_A -d action=remove -d "confirm=$LATEZ" >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/s/web/domains/$(owner_sql "SELECT id FROM domains WHERE hostname = 'www.$LATEZ'")/delete" -d csrf=$CSRF_A >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null
check "[A] zone deleted" "$(code_of ja -X POST "$BASE/o/$A/dns/$ZONE" -d csrf=$CSRF_A -d action=remove -d "confirm=$ZONE")" 303
check "a deleted zone is no longer served" "$(sleep 2; ask_dns "$ZONE" A | grep -c REFUSED)" 1

admin set-plan --slug "$A" --plan pro >/dev/null
mkdir -p "$TMP/cdnapp"
printf '%s\n' 'FROM nginx:1.27-alpine' 'COPY default.conf /etc/nginx/conf.d/default.conf' > "$TMP/cdnapp/Dockerfile"
printf '%s\n' 'server {' '  listen 80;' '  location /cached { add_header Cache-Control "public, max-age=60"; return 200 "cached $msec\n"; }' '  location / { return 200 "fresh $msec\n"; }' '}' > "$TMP/cdnapp/default.conf"
docker build -q -t "$REG/$A/cdnapp:1" "$(cygpath -w "$TMP/cdnapp")" >/dev/null
echo "$TOK_MX" | dk login $REG -u "$A" --password-stdin >/dev/null; dk push "$REG/$A/cdnapp:1" >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=cdn -d image=nginx:1.27-alpine >/dev/null
check "[A] service with the edge cache on" "$(code_of ja -X POST "$BASE/o/$A/p/shop/s/cdn/settings" -d csrf=$CSRF_A -d "image=$REG/$A/cdnapp:1" -d port=80 -d health_path=%2F -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env= -d edge_cache=1 -d deploy=1):$(wait_status ja /o/$A/p/shop/s/cdn)" "303:live"
CDNH=cdn-shop-$A.apps.localhost
cget() { curl -sS -D - -o "$TMP/cdn.body" --resolve "$CDNH:$PORT:127.0.0.1" "http://$CDNH:$PORT$1" | tr -d '\r'; }
C1=; for _ in $(seq 1 20); do C1=$(cget /cached); grep -qi '^cache-status:' <<<"$C1" && break; sleep 2; done
B1=$(cat "$TMP/cdn.body"); C2=$(cget /cached); B2=$(cat "$TMP/cdn.body")
check "a cacheable response is served from the edge cache (same body, hit)" "$(grep -ci '^cache-status:.*hit' <<<"$C2"):$([ "$B1" = "$B2" ] && echo same)" "1:same"
F1=$(cget /page); FB1=$(cat "$TMP/cdn.body"); sleep 1; F2=$(cget /page); FB2=$(cat "$TMP/cdn.body")
check "a response without Cache-Control is never kept" "$(grep -ci '^cache-status:.*hit' <<<"$F2"):$([ "$FB1" != "$FB2" ] && echo fresh)" "0:fresh"
code_of ja -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/cdn >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "Hosted mailboxes (Stalwart)"
MZ=m$RUN.test
MP="Mailbox-pass-$RUN"
admin set-plan --slug "$A" --plan pro >/dev/null   # earlier sections used the starter plan's sending domains
mapi() { curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" -H 'Content-Type: application/json' "$@"; }
imap_count() { curl -sk --max-time 10 "imaps://127.0.0.1:2993/" -u "$1:$2" -X "STATUS \"$3\" (MESSAGES)" 2>/dev/null | grep -o 'MESSAGES [0-9]*' | grep -o '[0-9]*'; }
inbox_total() { echo $(( $(imap_count "$1" "$2" INBOX || echo 0) + $(imap_count "$1" "$2" "Junk Mail" || echo 0) )); }
smtp_in() { printf 'From: someone@sender.test\r\nTo: %s\r\nSubject: hello %s\r\n\r\nhi\r\n' "$1" "$RUN" > "$TMP/mail.eml"; curl -s --max-time 15 "smtp://127.0.0.1:2525" --mail-from someone@sender.test --mail-rcpt "$1" -T "$TMP/mail.eml" -o /dev/null -w '%{response_code}'; }
# every test connection comes from the Docker gateway: lift bans earlier failed sign-ins (from earlier runs) caused
stw() { MSYS_NO_PATHCONV=1 docker run --rm --network platform-core curlimages/curl:8.11.0 -s -u admin:dev-stalwart-admin -H 'Content-Type: application/json' -d "{\"using\":[\"urn:ietf:params:jmap:core\",\"urn:stalwart:jmap\"],\"methodCalls\":$1}" http://stalwart:8080/jmap/; }
stw '[["x:BlockedIp/query",{},"q"],["x:BlockedIp/set",{"#destroy":{"resultOf":"q","name":"x:BlockedIp/query","path":"/ids"}},"d"],["x:Action/set",{"create":{"r":{"@type":"ReloadBlockedIps"}}},"a"]]' >/dev/null
check "the mail server is set up by the agent (submission port 587 answers)" "$(until_is yes 90 sh -c 'curl -s --max-time 3 smtp://127.0.0.1:2588 -o /dev/null && echo yes')" yes
code_of ja -X POST "$BASE/o/$A/dns" -d csrf=$CSRF_A -d "domain=$MZ" >/dev/null
owner_sql "UPDATE dns_zones SET status = 'active', active_at = now() WHERE domain = '$MZ'; SELECT pg_notify('deployments', 'dns')" >/dev/null   # stands in for the delegation
check "[A] a domain in an active hosted zone is verified and receives mail at once" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/mail" -d csrf=$CSRF_A -d "domain=$MZ" | grep -c 'create+its+mailboxes'):$(owner_sql "SELECT stalwart_id IS NOT NULL FROM mail_domains WHERE domain = '$MZ'")" "1:t"
check "its MX, DKIM, SPF, DMARC and client SRV records were written" "$(owner_sql "SELECT string_agg(DISTINCT type, ',' ORDER BY type) FROM dns_records WHERE managed_by = 'mail:' || (SELECT id FROM mail_domains WHERE domain = '$MZ')"):$(owner_sql "SELECT count(*) FROM dns_records WHERE type = 'TXT' AND name LIKE '%._domainkey' AND managed_by LIKE 'mail:%' AND zone_id = (SELECT id FROM dns_zones WHERE domain = '$MZ')")" "MX,SRV,TXT:2"
check "the MX answers on the platform's DNS" "$(sleep 2; ask_dns "$MZ" MX | grep -c 'console.localhost')" 1
check "[A] mailbox created with a chosen password" "$(code_of ja -X POST "$BASE/o/$A/mail/$MZ" -d csrf=$CSRF_A -d action=add -d local_part=anna -d "display_name=Anna Test" -d quota_mb=200 --data-urlencode "password=$MP")" 303
check "a too-short password is refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/mail/$MZ" -d csrf=$CSRF_A -d action=add -d local_part=short -d quota_mb=100 -d password=short | grep -c 'err=')" 1
check "anna signs in over IMAP (TLS)" "$(curl -sk --max-time 10 "imaps://127.0.0.1:2993/" -u "anna@$MZ:$MP" -X 'CAPABILITY' -o /dev/null -w '%{exitcode}' 2>/dev/null; echo)" 0
check "a wrong password is refused" "$(curl -sk --max-time 10 "imaps://127.0.0.1:2993/" -u "anna@$MZ:wrong-password-x" -X 'CAPABILITY' -o /dev/null 2>/dev/null && echo in || echo refused)" refused
check "mail sent to anna from the internet arrives (port 25)" "$(smtp_in "anna@$MZ"):$(until_is 1 20 inbox_total "anna@$MZ" "$MP")" "250:1"
check "anna sends with sign-in on port 587 (STARTTLS)" "$(curl -sk --max-time 15 "smtp://127.0.0.1:2588" --ssl-reqd -u "anna@$MZ:$MP" --mail-from "anna@$MZ" --mail-rcpt "anna@$MZ" -T "$TMP/mail.eml" -o /dev/null -w '%{response_code}')" 250
check "[A] alias added; its mail reaches anna" "$(code_of ja -X POST "$BASE/o/$A/mail/$MZ" -d csrf=$CSRF_A -d action=alias -d mailbox=anna -d alias=sales):$(smtp_in "sales@$MZ"):$(until_is 3 20 inbox_total "anna@$MZ" "$MP")" "303:250:3"
check "an unknown address is refused" "$(smtp_in "nobody@$MZ")" 550
check "not an open relay (no sign-in: refused on 587 and 25)" "$(curl -sk --max-time 15 smtp://127.0.0.1:2588 --ssl-reqd --mail-from "anna@$MZ" --mail-rcpt someone@example.org -T "$TMP/mail.eml" -o /dev/null -w '%{response_code}'):$(curl -s --max-time 15 smtp://127.0.0.1:2525 --mail-from someone@sender.test --mail-rcpt someone@example.org -T "$TMP/mail.eml" -o /dev/null -w '%{response_code}')" "503:550"
check "[A] catch-all on anna: unknown addresses are delivered to her" "$(code_of ja -X POST "$BASE/o/$A/mail/$MZ" -d csrf=$CSRF_A -d action=catchall -d mailbox=anna):$(smtp_in "nobody@$MZ"):$(until_is 4 20 inbox_total "anna@$MZ" "$MP")" "303:250:4"
check "the domain page lists the mailbox, its alias and storage" "$(c ja "$BASE/o/$A/mail/$MZ" | grep -c -e "anna@$MZ" -e "sales@$MZ" -e '/ 200 MB' | sed 's/^[3-9]$/yes/')" yes
check "[API] mailbox with a generated password (shown once)" "$(mapi -X POST "$BASE/api/v1/mail/domains/$MZ/mailboxes" -d '{"name":"bob","quota_mb":100}' | grep -o '"password":"[A-Za-z0-9]\{20\}"' | wc -l | tr -d ' ')" 1
check "[API] mailboxes listed" "$(mapi "$BASE/api/v1/mail/domains/$MZ/mailboxes" | grep -o '"address":"[a-z]*@' | sort | tr -d '\n')" '"address":"anna@"address":"bob@'
check "[API] new password for bob" "$(mapi -X POST "$BASE/api/v1/mail/domains/$MZ/mailboxes/bob/password" -d "{\"password\":\"$MP-bob\"}" -o /dev/null -w '%{http_code}'):$(curl -sk --max-time 10 "imaps://127.0.0.1:2993/" -u "bob@$MZ:$MP-bob" -X CAPABILITY -o /dev/null && echo in)" "200:in"
check "the plan's mailbox storage limit is enforced" "$(mapi -X POST "$BASE/api/v1/mail/domains/$MZ/mailboxes" -d '{"name":"huge","quota_mb":900000}' -o /dev/null -w '%{http_code}')" 422
check "[B] another organization cannot take the domain" "$(c jb -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$B/mail" -d csrf=$CSRF_B -d "domain=$MZ" | grep -c 'err=')" 1
check "[B->A] B cannot open A's mail domain (404)" "$(code_of jb "$BASE/o/$B/mail/$MZ"):$(code_of jb "$BASE/o/$A/mail/$MZ")" "404:404"
check "the sending domain cannot be deleted while it has mailboxes" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/email/d/$MZ/delete" -d csrf=$CSRF_A -d "confirm=$MZ" | grep -c 'has+mailboxes')" 1
check "[A] mailbox deleted: it can no longer sign in" "$(code_of ja -X POST "$BASE/o/$A/mail/$MZ" -d csrf=$CSRF_A -d action=delete -d mailbox=bob -d "confirm=bob@$MZ"):$(curl -sk --max-time 10 "imaps://127.0.0.1:2993/" -u "bob@$MZ:$MP-bob" -X CAPABILITY -o /dev/null && echo in || echo out)" "303:out"
check "[A] domain removed with its mailboxes and records" "$(code_of ja -X POST "$BASE/o/$A/mail/$MZ" -d csrf=$CSRF_A -d action=remove -d "confirm=$MZ"):$(curl -sk --max-time 10 "imaps://127.0.0.1:2993/" -u "anna@$MZ:$MP" -X CAPABILITY -o /dev/null && echo in || echo out):$(owner_sql "SELECT count(*) FROM dns_records WHERE managed_by LIKE 'mail:%' AND zone_id = (SELECT id FROM dns_zones WHERE domain = '$MZ')")" "303:out:0"
check "mail to the removed domain is no longer accepted" "$(smtp_in "anna@$MZ" | grep -c '^250$')" 0
code_of ja -X POST "$BASE/o/$A/email/d/$MZ/delete" -d csrf=$CSRF_A -d "confirm=$MZ" >/dev/null
code_of ja -X POST "$BASE/o/$A/dns/$MZ" -d csrf=$CSRF_A -d action=remove -d "confirm=$MZ" >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "Bulk actions and create forms on the lists"
admin set-plan --slug "$A" --plan pro >/dev/null
BK_CODE='export const handler = async () => ({ ok: true });'
for f in bulka bulkb; do fn_api PUT "$f" -o /dev/null -d "$(printf '%s' "$BK_CODE" | python -c 'import json,sys; print(json.dumps({"runtime": "nodejs22", "code": sys.stdin.read(), "url_enabled": True, "url_auth": "none"}))')"; done
check "[bulk] URLs of two functions turned off" "$(code_of ja -X POST "$BASE/o/$A/functions/actions" -d csrf=$CSRF_A -d action=url-off -d sel=shop/bulka -d sel=shop/bulkb):$(owner_sql "SELECT count(*) FROM functions WHERE org_id = '$ORG_A' AND slug IN ('bulka', 'bulkb') AND url_enabled")" "303:0"
check "[bulk] deleting needs the typed confirmation" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/functions/actions" -d csrf=$CSRF_A -d action=delete -d sel=shop/bulka | grep -c 'err=')" 1
check "[bulk] two functions deleted" "$(code_of ja -X POST "$BASE/o/$A/functions/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/bulka -d sel=shop/bulkb):$(owner_sql "SELECT count(*) FROM functions WHERE org_id = '$ORG_A' AND slug IN ('bulka', 'bulkb')")" "303:0"
for b in bulk1 bulk2; do code_of ja -X POST "$BASE/o/$A/storage/buckets" -d csrf=$CSRF_A -d name=$b -d quota_mb=5 >/dev/null; done
check "[bulk] two buckets deleted" "$(code_of ja -X POST "$BASE/o/$A/storage/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=bulk1 -d sel=bulk2):$(owner_sql "SELECT count(*) FROM buckets WHERE org_id = '$ORG_A' AND name IN ('bulk1', 'bulk2')")" "303:0"
code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=bulkdb -d engine=redis:7 >/dev/null
check "[bulk] database deleted from the list" "$(code_of ja -X POST "$BASE/o/$A/databases/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/bulkdb):$(owner_sql "SELECT count(*) FROM databases WHERE org_id = '$ORG_A' AND slug = 'bulkdb'")" "303:0"
check "[B->A] B's bulk action cannot touch A's database" "$(code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=bulkdb2 -d engine=redis:7 >/dev/null; c jb -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$B/databases/actions" -d csrf=$CSRF_B -d action=delete -d confirm=delete -d sel=shop/bulkdb2 | grep -c 'Skipped'):$(owner_sql "SELECT count(*) FROM databases WHERE org_id = '$ORG_A' AND slug = 'bulkdb2'")" "1:1"
code_of ja -X POST "$BASE/o/$A/databases/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/bulkdb2 >/dev/null
check "[A] domain added from the Domains page" "$(code_of ja -X POST "$BASE/o/$A/domains" -d csrf=$CSRF_A -d hostname=bulk-$RUN.example.test -d service=shop/web):$(owner_sql "SELECT count(*) FROM domains WHERE org_id = '$ORG_A' AND hostname = 'bulk-$RUN.example.test'")" "303:1"
check "[bulk] domain removed from the list" "$(code_of ja -X POST "$BASE/o/$A/domains/actions" -d csrf=$CSRF_A -d action=remove -d confirm=delete -d sel=bulk-$RUN.example.test):$(owner_sql "SELECT count(*) FROM domains WHERE org_id = '$ORG_A' AND hostname = 'bulk-$RUN.example.test'")" "303:0"
admin set-plan --slug "$A" --plan starter >/dev/null

section "Exposure: private services, extra ports, path routing, public databases"
admin set-plan --slug "$A" --plan pro >/dev/null
code_of ja -X POST "$BASE/o/$A/projects" -d csrf=$CSRF_A -d slug=expo -d name=Expo >/dev/null
# an image answering on two ports: 80 ("main <path>") and 9000 ("admin <path>")
mkdir -p "$TMP/multiport"
printf '%s\n' 'FROM nginx:1.27-alpine' 'COPY default.conf /etc/nginx/conf.d/default.conf' > "$TMP/multiport/Dockerfile"
printf '%s\n' 'server { listen 80; location / { return 200 "main $uri\n"; } }' 'server { listen 9000; location / { return 200 "admin $uri\n"; } }' > "$TMP/multiport/default.conf"
docker build -q -t "$REG/$A/multiport:1" "$(cygpath -w "$TMP/multiport")" >/dev/null
echo "$TOK_MX" | dk login $REG -u "$A" --password-stdin >/dev/null; dk push "$REG/$A/multiport:1" >/dev/null
ex_svc() { # ex_svc <slug> <image> [extra settings]
  code_of ja -X POST "$BASE/o/$A/p/expo/services" -d csrf=$CSRF_A -d slug=$1 -d image=nginx:1.27-alpine >/dev/null
  code_of ja -X POST "$BASE/o/$A/p/expo/s/$1/settings" -d csrf=$CSRF_A -d "image=$2" -d port=80 -d health_path=%2F \
    -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env= -d deploy=1 "${@:3}" >/dev/null; }
ex_svc front nginx:1.27-alpine; ex_svc api "$REG/$A/multiport:1" -d private=1
check "[A] two services live, api set to private" "$(wait_status ja /o/$A/p/expo/s/front):$(wait_status ja /o/$A/p/expo/s/api):$(owner_sql "SELECT public FROM services s JOIN projects p ON p.id = s.project_id WHERE p.org_id = '$ORG_A' AND p.slug = 'expo' AND s.slug = 'api'")" "live:live:f"
ex_get() { curl -s --max-time 10 --resolve "$1:$PORT:127.0.0.1" "http://$1:$PORT$2"; }
API_HOST=api-expo-$A.apps.localhost
check "a private service has no public address" "$(sleep 3; ex_get "$API_HOST" / | grep -c 'No application is published')" 1
EX_FRONT=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id WHERE p.org_id = '$ORG_A' AND p.slug = 'expo' AND s.slug = 'front' AND d.status = 'live'")
check "but its project's services reach it at its private name" "$(until_is 1 30 sh -c "MSYS_NO_PATHCONV=1 docker exec '$EX_FRONT' wget -qO- -T 5 http://api.expo.internal/hello 2>/dev/null | grep -c 'main /hello'")" 1
check "the service page says it is private" "$(c ja "$BASE/o/$A/p/expo/s/api" | grep -c '<span class="badge">private</span>')" 1
# extra public port: make api public again, publish port 9000 as admin.<address>
code_of ja -X POST "$BASE/o/$A/p/expo/s/api/settings" -d csrf=$CSRF_A -d "image=$REG/$A/multiport:1" -d port=80 -d health_path=%2F -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env= >/dev/null
check "[A] port 9000 published as admin" "$(code_of ja -X POST "$BASE/o/$A/p/expo/s/api/ports" -d csrf=$CSRF_A -d label=admin -d port=9000)" 303
check "invalid ports refused (the main port, a label starting with pr)" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/expo/s/api/ports" -d csrf=$CSRF_A -d label=other -d port=80 | grep -c 'err='):$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/expo/s/api/ports" -d csrf=$CSRF_A -d label=pr1 -d port=9001 | grep -c 'err=')" "1:1"
check "the main address and the extra port answer (public again)" "$(until_is 'main /x' 30 sh -c "curl -s --max-time 5 --resolve '$API_HOST:$PORT:127.0.0.1' 'http://$API_HOST:$PORT/x'"):$(until_is 'admin /y' 30 sh -c "curl -s --max-time 5 --resolve 'admin.$API_HOST:$PORT:127.0.0.1' 'http://admin.$API_HOST:$PORT/y'")" "main /x:admin /y"
check "[API] ports listed" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/projects/expo/services/api/ports" | grep -c '"label":"admin","port":9000')" 1
check "[B->A] B cannot publish a port on A's service" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_B" -H 'Content-Type: application/json' -d '{"label":"x","port":9100}' "$BASE/api/v1/projects/expo/services/api/ports")" 404
# path routing: shop-<run>.example.test serves front, /api goes to api with the prefix removed
EXD=expo-$RUN.example.test
code_of ja -X POST "$BASE/o/$A/p/expo/s/front/domains" -d csrf=$CSRF_A -d "hostname=$EXD" >/dev/null
owner_sql "UPDATE domains SET verified_at = now() WHERE hostname = '$EXD'" >/dev/null   # stands in for the DNS check
EXD_ID=$(owner_sql "SELECT id FROM domains WHERE hostname = '$EXD'")
check "[A] path /api routed to the api service (prefix removed)" "$(code_of ja -X POST "$BASE/o/$A/p/expo/s/front/domains/$EXD_ID/paths" -d csrf=$CSRF_A -d prefix=%2Fapi -d target=expo/api -d strip=1)" 303
check "an invalid path is refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/expo/s/front/domains/$EXD_ID/paths" -d csrf=$CSRF_A -d prefix=api/ -d target=expo/api | grep -c 'err=')" 1
owner_sql "SELECT pg_notify('deployments', '')" >/dev/null
check "the domain sends /api/... to api without the prefix, the rest to front" "$(until_is 'main /orders/7' 30 sh -c "curl -s --max-time 5 --resolve '$EXD:$PORT:127.0.0.1' 'http://$EXD:$PORT/api/orders/7'"):$(ex_get "$EXD" / | grep -c 'Welcome to nginx' | sed 's/^[1-9][0-9]*$/yes/')" "main /orders/7:yes"
check "a path route works to a private service too" "$(code_of ja -X POST "$BASE/o/$A/p/expo/s/api/settings" -d csrf=$CSRF_A -d "image=$REG/$A/multiport:1" -d port=80 -d health_path=%2F -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env= -d private=1):$(sleep 3; ex_get "$EXD" /api/z)" "303:main /z"
check "path changes audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE org_id = '$ORG_A' AND action IN ('domain.path_added', 'service.port_published')")" 2
# public database: PostgreSQL over TLS, only from allowed addresses
code_of ja -X POST "$BASE/o/$A/p/expo/databases" -d csrf=$CSRF_A -d slug=pubdb -d engine=postgres:17 >/dev/null
st=; for _ in $(seq 1 90); do st=$(owner_sql "SELECT status FROM databases d WHERE d.org_id = '$ORG_A' AND d.slug = 'pubdb'"); [ "$st" = running ] && break; sleep 2; done
check "[A] database running" "$st" running
check "an empty or invalid allow-list is refused" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/expo/d/pubdb/public" -d csrf=$CSRF_A -d allow=not-an-ip | grep -c 'err=')" 1
check "[A] public access opened" "$(code_of ja -X POST "$BASE/o/$A/p/expo/d/pubdb/public" -d csrf=$CSRF_A -d 'allow=0.0.0.0/0')" 303
PUBPORT=$(owner_sql "SELECT pp.public_port FROM public_ports pp JOIN databases d ON d.id = pp.database_id WHERE d.org_id = '$ORG_A' AND d.slug = 'pubdb'")
DBPAGE=$(c ja "$BASE/o/$A/p/expo/d/pubdb")
DBPW=$(grep -o '<summary>show</summary><code class="select">[^<]*' <<<"$DBPAGE" | head -1 | sed 's/.*">//')
check "the page shows the endpoint and its certificate" "$(grep -c "<code class=\"select\">$PUBPORT</code>" <<<"$DBPAGE"):$(grep -c 'BEGIN CERTIFICATE' <<<"$DBPAGE")" "1:1"
check "the endpoint's key is stored encrypted" "$(owner_sql "SELECT count(*) FROM databases WHERE slug = 'pubdb' AND org_id = '$ORG_A' AND position(convert_to('PRIVATE KEY', 'UTF8') in proxy_key_enc) > 0")" 0
pgq() { MSYS_NO_PATHCONV=1 docker run --rm postgres:17-alpine psql "host=host.docker.internal port=$PUBPORT dbname=app user=app password=$DBPW $1" -tAc 'SELECT 40 + 2' 2>&1 | tr -d '\r'; }
check "psql connects from outside with TLS (sslmode=require)" "$(until_is 42 60 pgq 'sslmode=require connect_timeout=5')" 42
check "a plain-text connection is refused" "$(pgq 'sslmode=disable connect_timeout=5' | grep -c '^42$')" 0
code_of ja -X POST "$BASE/o/$A/p/expo/d/pubdb/public" -d csrf=$CSRF_A -d 'allow=203.0.113.7' >/dev/null
check "an address outside the allow-list cannot connect" "$(sleep 25; pgq 'sslmode=require connect_timeout=5' | grep -c '^42$')" 0
check "[B] cannot open A's database (404)" "$(code_of jb -X POST "$BASE/o/$A/p/expo/d/pubdb/public" -d csrf=$CSRF_B -d 'allow=0.0.0.0/0')" 404
code_of ja -X POST "$BASE/o/$A/p/expo/d/pubdb/public" -d csrf=$CSRF_A -d close=1 >/dev/null
check "closing public access removes the endpoint and frees the port" "$(for _ in $(seq 1 30); do [ -z "$(docker ps -q --filter label=platform.dbproxy)" ] && { echo yes; break; }; sleep 2; done):$(owner_sql "SELECT count(*) FROM public_ports pp JOIN databases d ON d.id = pp.database_id WHERE d.slug = 'pubdb' AND d.org_id = '$ORG_A'")" "yes:0"
check "MySQL offers no public endpoint" "$(code_of ja -X POST "$BASE/o/$A/p/expo/databases" -d csrf=$CSRF_A -d slug=mydb -d engine=mysql:8.4 >/dev/null; c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/expo/d/mydb/public" -d csrf=$CSRF_A -d 'allow=203.0.113.7' | grep -c 'err=')" 1
code_of ja -X POST "$BASE/o/$A/p/expo/delete" -d csrf=$CSRF_A -d confirm=expo >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "Console actions on the API"
admin set-plan --slug "$A" --plan pro >/dev/null
TOK_ADMAPI=$(new_api_token ja "$A" "$CSRF_A" automation admin)
capi() { local m=$1 path=$2; shift 2; curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_ADMAPI" -H 'Content-Type: application/json' -X "$m" "$BASE/api/v1$path" "$@"; }
check "[API] project created" "$(capi POST /projects -d '{"slug": "apiproj", "name": "From the API"}' | grep -c '"message"')" 1
check "[API] a developer token cannot create a project (403)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_DEV" -H 'Content-Type: application/json' -d '{"slug": "nope"}' "$BASE/api/v1/projects")" 403
check "[API] service created and its settings saved (private, worker refused port)" "$(capi POST /projects/apiproj/services -d '{"slug": "svc", "image": "nginx:1.27-alpine"}' | grep -c '"message"'):$(capi PUT /projects/apiproj/services/svc/settings -d '{"image": "nginx:1.27-alpine", "port": 80, "health_path": "/", "cpu_millis": 100, "memory_mb": 64, "instances": 1, "env": "A=1", "private": true, "deploy": true}' | grep -c '"message"'):$(owner_sql "SELECT public FROM services s JOIN projects p ON p.id = s.project_id WHERE p.org_id = '$ORG_A' AND p.slug = 'apiproj' AND s.slug = 'svc'")" "1:1:f"
check "[API] a refused form answers 422 with the console's message" "$(capi PUT /projects/apiproj/services/svc/settings -o /dev/null -w '%{http_code}' -d '{"image": "nginx:1.27-alpine", "port": 0, "health_path": "/", "cpu_millis": 100, "memory_mb": 64}')" 422
check "[API] database created and attached as a secret" "$(capi POST /projects/apiproj/databases -d '{"slug": "kv", "engine": "redis:7"}' | grep -c '"message"'):$(capi POST /projects/apiproj/databases/kv/attach -d '{"service": "svc", "name": "REDIS_URL"}' | grep -c '"message"')" "1:1"
check "[API] a missing database answers 404" "$(capi POST /projects/apiproj/databases/nope/attach -o /dev/null -w '%{http_code}' -d '{"service": "svc", "name": "X_URL"}')" 404
check "[B->A API] B's token cannot act in A's project (refused, nothing created in A)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_B" -H 'Content-Type: application/json' -d '{"slug": "bx", "engine": "redis:7"}' "$BASE/api/v1/projects/apiproj/databases" | sed 's/^404$/refused/; s/^422$/refused/'):$(owner_sql "SELECT count(*) FROM databases WHERE org_id = '$ORG_A' AND slug = 'bx'")" "refused:0"
check "[API] console actions audited with the token" "$(owner_sql "SELECT count(*) > 0 FROM audit_events WHERE org_id = '$ORG_A' AND action = 'project.created' AND detail::text LIKE '%automation%'")" t
code_of ja -X POST "$BASE/o/$A/p/apiproj/delete" -d csrf=$CSRF_A -d confirm=apiproj >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "Cost reports and budgets"
# 1000 vCPU-hours this hour in project shop: 10.00 at the starter plan's 1 cent per vCPU-hour
owner_sql "INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, cpu_milli_minutes) VALUES ('$ORG_A', gen_random_uuid(), 'service', 'shop/e2e-cost-$RUN', date_trunc('hour', now()), 60000000)" >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null
check "the billing page shows the cost by project" "$(c ja "$BASE/o/$A/billing" | grep -c -e "id=\"costs-table\"" -e "/o/$A/p/shop\">shop</a>")" 2
TOK_ADM=$(new_api_token ja "$A" "$CSRF_A" finance admin)
COSTS=$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_ADM" "$BASE/api/v1/costs")
check "[API] cost report per project (before tax, with a forecast)" "$(python -c 'import json,sys; d=json.load(sys.stdin); p={r["project"]: r for r in d["projects"]}; s=p.get("shop", {}); print(s.get("month_to_date_cents", 0) >= 1000, s.get("forecast_cents", 0) >= s.get("month_to_date_cents", 0), d["before_tax"])' <<<"$COSTS" | tr -d '\r')" "True True True"
check "[API] costs need the admin role (403 for a developer token)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/costs")" 403
check "invalid budgets refused (no limit, threshold above 500%)" "$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/billing/budgets" -d csrf=$CSRF_A -d project=shop -d limit=abc | grep -c 'err='):$(c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/billing/budgets" -d csrf=$CSRF_A -d project=shop -d limit=5 -d thresholds=50,900 | grep -c 'err=')" "1:1"
check "[A] budget of 5.00 for project shop saved" "$(code_of ja -X POST "$BASE/o/$A/billing/budgets" -d csrf=$CSRF_A -d project=shop -d limit=5 -d "thresholds=50, 80, 100" -d forecast=1)" 303
BQ="SELECT b.level || ':' || (SELECT count(*) FROM alert_incidents i WHERE i.budget_id = b.id AND i.status = 'firing' AND i.kind = 'budget') FROM budgets b JOIN projects p ON p.id = b.project_id WHERE b.org_id = '$ORG_A' AND p.slug = 'shop'"
check "spending above the limit opens a budget incident (100% reached)" "$(until_is 100:1 40 owner_sql "$BQ")" 100:1
check "the incident says how much was spent" "$(owner_sql "SELECT count(*) FROM alert_incidents i JOIN budgets b ON b.id = i.budget_id WHERE b.org_id = '$ORG_A' AND i.status = 'firing' AND i.summary LIKE '%project shop: 100% reached%'")" 1
check "the budget shows its spending on the page" "$(c ja "$BASE/o/$A/billing" | grep -A3 'project <strong>shop</strong>' | grep -c 'error-text small">[0-9]*%')" 1
owner_sql "SELECT pg_notify('deployments', 'budgets')" >/dev/null; sleep 2
check "a crossed threshold notifies only once per month" "$(owner_sql "SELECT count(*) FROM alert_incidents i JOIN budgets b ON b.id = i.budget_id WHERE b.org_id = '$ORG_A' AND i.kind = 'budget'")" 1
check "[API] budgets listed; B does not see A's" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_ADM" "$BASE/api/v1/budgets" | grep -c '"project":"shop"'):$(owner_sql "SELECT count(*) FROM budgets WHERE org_id = '$ORG_B'")" "1:0"
check "[API] organization budget created" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_ADM" -H 'Content-Type: application/json' -d '{"limit_cents": 100000000, "thresholds": [90]}' "$BASE/api/v1/budgets")" 201
BID=$(owner_sql "SELECT id FROM budgets WHERE org_id = '$ORG_A' AND project_id IS NULL")
check "[B->A] B cannot delete A's budget" "$(c jb -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$B/billing/budgets/$BID/delete" -d csrf=$CSRF_B | grep -c 'not+found\|not%20found'):$(owner_sql "SELECT count(*) FROM budgets WHERE id = '$BID'")" "1:1"
check "[API] budget deleted" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $TOK_ADM" "$BASE/api/v1/budgets/$BID")" 204
check "budget changes audited" "$(owner_sql "SELECT count(*) >= 3 FROM audit_events WHERE org_id = '$ORG_A' AND action IN ('budget.saved', 'budget.deleted')")" t
owner_sql "DELETE FROM budgets WHERE org_id = '$ORG_A'; DELETE FROM usage_hourly WHERE name = 'shop/e2e-cost-$RUN'" >/dev/null

section "Private network between projects"
admin set-plan --slug "$A" --plan pro >/dev/null
for pj in front back lone; do code_of ja -X POST "$BASE/o/$A/projects" -d csrf=$CSRF_A -d slug=$pj -d name=$pj >/dev/null; done
pn_svc() { # pn_svc <project> <service>: an nginx service, deployed
  code_of ja -X POST "$BASE/o/$A/p/$1/services" -d csrf=$CSRF_A -d slug=$2 -d image=nginx:1.27-alpine >/dev/null
  code_of ja -X POST "$BASE/o/$A/p/$1/s/$2/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
    -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env= -d deploy=1 >/dev/null; }
pn_svc back api; pn_svc front caller; pn_svc lone solo
check "[A] three projects with a service each are live" "$(wait_status ja /o/$A/p/back/s/api):$(wait_status ja /o/$A/p/front/s/caller):$(wait_status ja /o/$A/p/lone/s/solo)" "live:live:live"
pn_cid() { owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id WHERE p.org_id = '$ORG_A' AND p.slug = '$1' AND s.slug = '$2' AND d.status = 'live'"; }
pn_get() { dx exec "$(pn_cid "$1" "$2")" wget -qO- -T 5 "$3" 2>&1 | grep -q 'Welcome to nginx' && echo 1 || echo 0; }
check "a service reaches another of its own project by its private name" "$(until_is 1 30 pn_get back api http://api.back.internal/)" 1
check "before connecting, front does not know back's names" "$(dx exec "$(pn_cid front caller)" grep -c 'api.back.internal' /etc/hosts)" 0
check "[A admin] front connected to back" "$(code_of ja -X POST "$BASE/o/$A/p/front/connections" -d csrf=$CSRF_A -d project=back)" 303
check "both projects redeploy to learn the names" "$(for _ in $(seq 1 60); do c1=$(pn_cid front caller); [ -n "$c1" ] && dx exec "$c1" grep -q 'api.back.internal' /etc/hosts 2>/dev/null && { echo yes; break; }; sleep 2; done)" yes
check "front reaches back's service over the private network" "$(until_is 1 30 pn_get front caller http://api.back.internal/)" 1
check "and back reaches front (connections go both ways)" "$(until_is 1 45 pn_get back api http://caller.front.internal/)" 1
LONE_CADDY=$(dx exec "$(pn_cid lone solo)" sh -c "grep ' console.localhost' /etc/hosts | awk '{print \$1}'" | tr -d '\r')
check "an unconnected project is refused even through the proxy address" "$(dx exec "$(pn_cid lone solo)" wget -qO- -T 5 --header 'Host: api.back.internal' "http://$LONE_CADDY/" 2>&1 | grep -q 'Welcome to nginx' && echo 1 || echo 0)" 0
B_CID=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$B' AND d.status = 'live' LIMIT 1")
B_CADDY=$(dx exec "$B_CID" sh -c "grep ' console.localhost' /etc/hosts | awk '{print \$1}'" 2>/dev/null | tr -d '\r')
check "[B->A] another organization cannot use A's private names" "$(dx exec "$B_CID" wget -qO- -T 5 --header 'Host: api.back.internal' "http://$B_CADDY/" 2>&1 | grep -q 'Welcome to nginx' && echo 1 || echo 0)" 0
check "the internet cannot use the private names" "$(curl -s --resolve "api.back.internal:$PORT:127.0.0.1" "http://api.back.internal:$PORT/" | grep -q 'Welcome to nginx' && echo 1 || echo 0)" 0
check "project map: the connected project is drawn" "$(c ja -H 'HX-Request: true' "$BASE/o/$A/p/front/map" | grep -q 'connected project' && echo yes)" yes
check "[API] connections listed with their names" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/projects/front/connections" | grep -c '"api.back.internal"')" 1
check "[API] only admins connect projects (403 for a developer token)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_DEV" -H 'Content-Type: application/json' -d '{"project":"lone"}' "$BASE/api/v1/projects/front/connections")" 403
check "[B->A] B cannot connect to A's project" "$(c jb -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$B/p/shop/connections" -d csrf=$CSRF_B -d project=back | grep -c 'not+found\|not%20found')" 1
check "[A admin] disconnected" "$(code_of ja -X POST "$BASE/o/$A/p/front/connections" -d csrf=$CSRF_A -d project=back -d remove=1):$(owner_sql "SELECT count(*) FROM project_peerings WHERE org_id = '$ORG_A'")" "303:0"
check "after disconnecting, front no longer reaches back" "$(for _ in $(seq 1 60); do c1=$(pn_cid front caller); [ -n "$c1" ] && ! dx exec "$c1" grep -q 'api.back.internal' /etc/hosts 2>/dev/null && { echo yes; break; }; sleep 2; done)" yes
check "connections audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE org_id = '$ORG_A' AND action IN ('project.connected', 'project.disconnected')")" 2
for pj in front back lone; do code_of ja -X POST "$BASE/o/$A/p/$pj/delete" -d csrf=$CSRF_A -d confirm=$pj >/dev/null; done
admin set-plan --slug "$A" --plan starter >/dev/null

section "Scheduled scaling"
admin set-plan --slug "$A" --plan pro >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/services" -d csrf=$CSRF_A -d slug=sched -d image=nginx:1.27-alpine >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/s/sched/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
  -d cpu_millis=100 -d memory_mb=64 -d instances=1 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[A] service to schedule is live" "$(wait_status ja /o/$A/p/shop/s/sched)" live
sched_err() { c ja -o /dev/null -w '%{redirect_url}' -X POST "$BASE/o/$A/p/shop/s/sched/schedules" -d csrf=$CSRF_A "$@" | grep -c 'err='; }
check "invalid schedules refused (bad cron, more often than every 15 minutes, 11 instances)" "$(sched_err -d 'cron=99 * * * *' -d instances=2):$(sched_err -d 'cron=*/5 * * * *' -d instances=2):$(sched_err -d 'cron=0 8 * * *' -d instances=11)" "1:1:1"
check "[A] schedule added (2 instances on 1 January)" "$(code_of ja -X POST "$BASE/o/$A/p/shop/s/sched/schedules" -d csrf=$CSRF_A -d 'cron=0 0 1 1 *' -d instances=2 -d label=e2e)" 303
check "[API] schedules listed" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/projects/shop/services/sched/schedules" | grep -c '"cron":"0 0 1 1 \*"')" 1
check "[B->A] B cannot add a schedule to A's service (404)" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK_B" -H 'Content-Type: application/json' -d '{"cron":"0 9 * * *","instances":3}' "$BASE/api/v1/projects/shop/services/sched/schedules" -X POST):$(owner_sql "SELECT count(*) FROM scaling_schedules sc JOIN services s ON s.id = sc.service_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'sched'")" "404:1"
owner_sql "UPDATE scaling_schedules SET created_at = now() - interval '400 days' WHERE label = 'e2e'" >/dev/null   # its last 1 January is now due
SQ="SELECT s.instances || ':' || coalesce((SELECT jsonb_array_length(d.instances) FROM deployments d WHERE d.service_id = s.id AND d.status = 'live'), 0) FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$A' AND s.slug = 'sched'"
check "the due schedule scales the service to 2 running instances" "$(until_is 2:2 150 owner_sql "$SQ")" 2:2
check "applied once, recorded and audited" "$(owner_sql "SELECT last_run IS NOT NULL AND last_error = '' FROM scaling_schedules WHERE label = 'e2e'"):$(owner_sql "SELECT count(*) FROM audit_events WHERE org_id = '$ORG_A' AND action = 'service.scheduled_scaling' AND target = 'service:shop/sched'")" "t:1"
check "the service page lists the schedule" "$(c ja "$BASE/o/$A/p/shop/s/sched" | grep -c 'id="schedules-table"')" 1
check "[API] schedule deleted" "$(curl -sS --resolve "console.localhost:$PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $TOK_DEV" "$BASE/api/v1/projects/shop/services/sched/schedules/$(owner_sql "SELECT id FROM scaling_schedules WHERE label = 'e2e'")")" 204
code_of ja -X POST "$BASE/o/$A/compute/actions" -d csrf=$CSRF_A -d action=delete -d confirm=delete -d sel=shop/sched >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null

section "Several servers (a second node on the same machine)"
docker compose -f compose.dev.yml --profile multinode up -d agent2 >/dev/null 2>&1
for _ in $(seq 1 40); do [ "$(owner_sql "SELECT count(*) FROM nodes WHERE id = 'node-2' AND last_seen > now() - interval '1 minute'")" = 1 ] && break; sleep 2; done
check "a second node joins with its heartbeat" "$(owner_sql "SELECT role || ':' || (cpus > 0) FROM nodes WHERE id = 'node-2'")" "worker:true"
PRIMARY=$(owner_sql "SELECT id FROM nodes WHERE role = 'primary' ORDER BY last_seen DESC LIMIT 1")
code_of ja -X POST "$BASE/operator/nodes/$PRIMARY" -d csrf=$OPCSRF -d cordoned=1 -d cluster=default >/dev/null
check "[A] project created while the primary is cordoned" "$(code_of ja -X POST "$BASE/o/$A/projects" -d csrf=$CSRF_A -d slug=edge -d name=Edge)" 303
code_of ja -X POST "$BASE/o/$A/p/edge/services" -d csrf=$CSRF_A -d slug=web -d image=nginx:1.27-alpine >/dev/null
code_of ja -X POST "$BASE/o/$A/p/edge/s/web/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "deployment on the second node reaches live" "$(wait_status ja /o/$A/p/edge/s/web)" live
check "project placed on the uncordoned node" "$(owner_sql "SELECT node_id FROM projects p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'edge'")" node-2
EDGE_CID=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'edge' AND d.status = 'live'")
check "the container is run by node-2" "$(docker inspect -f '{{index .Config.Labels "platform.node"}}' "$EDGE_CID")" node-2
check "the primary's proxy routes to the other node" "$(owner_sql "SELECT upstream LIKE 'host.docker.internal:%' FROM deployments WHERE container_id = '$EDGE_CID'")" t
EDGE_HOST=web-edge-$A.apps.localhost
if curl -s --resolve "$EDGE_HOST:$PORT:127.0.0.1" "http://$EDGE_HOST:$PORT/" | grep -q "Welcome to nginx"; then ok "app on node-2 answers through the proxy"; else bad "app on node-2 not reachable"; fi
# A project that keeps data, also on node-2: it moves with its folder when node-2 is drained.
admin set-plan --slug "$A" --plan pro >/dev/null
code_of ja -X POST "$BASE/o/$A/projects" -d csrf=$CSRF_A -d slug=vault -d name=Vault >/dev/null
code_of ja -X POST "$BASE/o/$A/p/vault/services" -d csrf=$CSRF_A -d slug=web -d image=nginx:1.27-alpine >/dev/null
code_of ja -X POST "$BASE/o/$A/p/vault/s/web/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
  -d cpu_millis=100 -d memory_mb=64 -d volume_path=%2Fdata -d env= -d deploy=1 >/dev/null
check "[A] project with a persistent folder runs on node-2" "$(wait_status ja /o/$A/p/vault/s/web):$(owner_sql "SELECT node_id FROM projects p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'vault'")" "live:node-2"
VAULT_CID=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'vault' AND d.status = 'live'")
dx exec "$VAULT_CID" sh -c "echo moved-data-$RUN > /data/move.txt"
VAULT_HOST=web-vault-$A.apps.localhost
code_of ja -X POST "$BASE/operator/nodes/$PRIMARY" -d csrf=$OPCSRF -d cluster=default >/dev/null
check "operator drains node-2, moving projects with data too" "$(code_of ja -X POST "$BASE/operator/nodes/node-2/drain" -d csrf=$OPCSRF -d move_stateful=1)" 303
for _ in $(seq 1 45); do [ "$(owner_sql "SELECT node_id FROM projects p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'edge'")" = "$PRIMARY" ] && break; sleep 2; done
check "the drained project moves to the primary" "$(owner_sql "SELECT node_id FROM projects p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'edge'")" "$PRIMARY"
check "and keeps serving after the move" "$(for _ in $(seq 1 45); do x=$(curl -s --resolve "$EDGE_HOST:$PORT:127.0.0.1" "http://$EDGE_HOST:$PORT/" | grep -c 'Welcome to nginx'); [ "$x" -gt 0 ] && [ "$(owner_sql "SELECT upstream FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'edge' AND d.status = 'live'")" = "" ] && { echo yes; break; }; sleep 2; done)" yes
check "node-2 removed its old container" "$(for _ in $(seq 1 20); do docker inspect "$EDGE_CID" >/dev/null 2>&1 || { echo gone; break; }; sleep 2; done)" gone
check "the project with data moves: snapshot, then restore on the primary" "$(for _ in $(seq 1 60); do st=$(owner_sql "SELECT m.status FROM project_moves m JOIN projects p ON p.id = m.project_id JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'vault' ORDER BY m.created_at DESC LIMIT 1"); [ "$st" = done ] || [ "$st" = failed ] && { echo "$st"; break; }; sleep 2; done)" done
check "it now runs on the primary" "$(owner_sql "SELECT node_id FROM projects p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'vault'")" "$PRIMARY"
check "its data came along (file written on node-2 read in the new container)" "$(for _ in $(seq 1 60); do NEWC=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'vault' AND d.status = 'live'"); [ -n "$NEWC" ] && [ "$NEWC" != "$VAULT_CID" ] && dx exec "$NEWC" cat /data/move.txt 2>/dev/null | grep -q "moved-data-$RUN" && { echo yes; break; }; sleep 2; done)" yes
if grep -q "vault" <<<"$(c ja "$BASE/operator")"; then ok "the operator console lists the move"; else bad "move missing from the operator console"; fi
check "move audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'project.moved' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" 1
code_of ja -X POST "$BASE/o/$A/p/vault/delete" -d csrf=$CSRF_A -d confirm=vault >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null
check "drain audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'operator.node_drain' AND target = 'node:node-2' AND created_at > now() - interval '10 minutes'")" 1
# Node failover: node-2 stops reporting; the primary cordons it and redeploys its stateless project.
admin set-plan --slug "$A" --plan pro >/dev/null
code_of ja -X POST "$BASE/operator/nodes/node-2" -d csrf=$OPCSRF -d cluster=default >/dev/null
code_of ja -X POST "$BASE/operator/nodes/$PRIMARY" -d csrf=$OPCSRF -d cordoned=1 -d cluster=default >/dev/null
code_of ja -X POST "$BASE/o/$A/projects" -d csrf=$CSRF_A -d slug=fover -d name=Failover >/dev/null
code_of ja -X POST "$BASE/o/$A/p/fover/services" -d csrf=$CSRF_A -d slug=web -d image=nginx:1.27-alpine >/dev/null
code_of ja -X POST "$BASE/o/$A/p/fover/s/web/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
  -d cpu_millis=100 -d memory_mb=64 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[A] stateless project running on node-2" "$(wait_status ja /o/$A/p/fover/s/web):$(owner_sql "SELECT node_id FROM projects p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'fover'")" "live:node-2"
FOVER_CID=$(owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'fover' AND d.status = 'live'")
code_of ja -X POST "$BASE/operator/nodes/$PRIMARY" -d csrf=$OPCSRF -d cluster=default >/dev/null
docker compose -f compose.dev.yml --profile multinode stop agent2 >/dev/null 2>&1
owner_sql "UPDATE nodes SET last_seen = now() - interval '10 minutes' WHERE id = 'node-2'" >/dev/null   # silent for 10 minutes
check "the silent node is failed over: cordoned, its project placed on the primary" "$(until_is "true:$PRIMARY" 90 owner_sql "SELECT (SELECT cordoned AND failed_over_at IS NOT NULL FROM nodes WHERE id = 'node-2') || ':' || coalesce((SELECT node_id FROM projects p JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'fover'), '')")" "true:$PRIMARY"
FOVER_HOST=web-fover-$A.apps.localhost
check "the app is redeployed on the primary and answers" "$(for _ in $(seq 1 60); do [ "$(owner_sql "SELECT upstream FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = p.org_id WHERE o.slug = '$A' AND p.slug = 'fover' AND d.status = 'live'")" = "" ] && curl -s --resolve "$FOVER_HOST:$PORT:127.0.0.1" "http://$FOVER_HOST:$PORT/" | grep -q 'Welcome to nginx' && { echo yes; break; }; sleep 2; done)" yes
check "failover audited and the operator notified" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'node.failed_over' AND target = 'node:node-2'" | sed 's/^[1-9][0-9]*$/yes/'):$(ntfy_has platform-operator "Node node-2 failed over")" "yes:yes"
docker compose -f compose.dev.yml --profile multinode start agent2 >/dev/null 2>&1
check "when node-2 comes back, it removes its old container" "$(for _ in $(seq 1 45); do docker inspect "$FOVER_CID" >/dev/null 2>&1 || { echo gone; break; }; sleep 2; done)" gone
code_of ja -X POST "$BASE/o/$A/p/fover/delete" -d csrf=$CSRF_A -d confirm=fover >/dev/null
admin set-plan --slug "$A" --plan starter >/dev/null
docker compose -f compose.dev.yml --profile multinode rm -sf agent2 >/dev/null 2>&1
owner_sql "DELETE FROM nodes WHERE id = 'node-2'" >/dev/null

section "Email alerts, trial reminder, build minutes"
check "[A] email alert channel added" "$(code_of ja -X POST "$BASE/o/$A/alerts/channels" -d csrf=$CSRF_A -d name=Email -d kind=email --data-urlencode "email=ops@$A.test" -d events=service_down -d events=deploy_failed)" 303
EMAIL_CH=$(owner_sql "SELECT c.id FROM alert_channels c JOIN orgs o ON o.id = c.org_id WHERE o.slug = '$A' AND c.kind = 'email'")
code_of ja -X POST "$BASE/o/$A/alerts/channels/$EMAIL_CH/test" -d csrf=$CSRF_A >/dev/null
if mail_body "ops@$A.test" "Test notification" | grep -q "alert channel of $A"; then ok "email alert delivered"; else bad "no alert email"; fi
owner_sql "UPDATE orgs SET plan_id = 'trial', trial_ends_at = now() + interval '2 days', trial_reminded_at = NULL, suspended = false, suspend_reason = '' WHERE slug = '$T'" >/dev/null
for _ in $(seq 1 40); do [ -n "$(owner_sql "SELECT trial_reminded_at FROM orgs WHERE slug = '$T'")" ] && break; sleep 3; done
if mail_body "$TEMAIL" "Your trial ends soon" | grep -q "ends in 2 day"; then ok "trial reminder emailed 3 days before the end"; else bad "no trial reminder"; fi
check "reminder sent only once" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'trial.reminded' AND org_id = (SELECT id FROM orgs WHERE slug = '$T')")" 1
admin set-plan --slug "$T" --plan starter >/dev/null
if c ja "$BASE/o/$A/billing" | grep -q "Builds from git"; then ok "build minutes appear on the bill"; else bad "build minutes missing from the bill"; fi

section "Account recovery"
check "[A] 10 recovery codes issued at setup" "$(wc -l < "$TMP/rc-$A" | tr -d ' ')" 10
RC=$(head -1 "$TMP/rc-$A")
login() { # login <jar> <email> <password>: prints the redirect target
  c "$1" -o /dev/null -w '%{redirect_url}' -X POST "$BASE/login" --data-urlencode "email=$2" --data-urlencode "password=$3"; }
verify() { # verify <jar> <code>: prints the HTTP status of the two-step step
  local cs; cs=$(c "$1" "$BASE/totp/verify" | csrf_of); code_of "$1" -X POST "$BASE/totp/verify" -d "csrf=$cs" --data-urlencode "code=$2"; }
check "[A] password login asks for the second step" "$(login jr1 "owner@$A.test" "correct-horse-battery-$A")" "$BASE/totp/verify"
check "[A] a recovery code replaces the 6-digit code" "$(verify jr1 "$RC")" 303
check "[A] recovery-code login is a full session" "$(code_of jr1 "$BASE/o/$A")" 200
login jr2 "owner@$A.test" "correct-horse-battery-$A" >/dev/null
check "the same recovery code works only once" "$(verify jr2 "$RC")" 401
check "recovery-code login audited" "$(owner_sql "SELECT count(*) FROM audit_events WHERE action = 'login.recovery_code' AND user_id = (SELECT id FROM users WHERE email = 'owner@$A.test')")" 1
if c jr1 -L "$BASE/account/recovery-codes" -d "csrf=$(c jr1 "$BASE/account" | csrf_of)" | grep -q "Save your recovery codes"; then ok "[A] new recovery codes from the Account page"; else bad "recovery codes not regenerated"; fi
check "old recovery codes stop working after regeneration" "$(login jr3 "owner@$A.test" "correct-horse-battery-$A" >/dev/null; verify jr3 "$(sed -n 2p "$TMP/rc-$A")")" 401
# Operator reset link (no email needed)
RLINK=$(admin reset-password --email "owner@$A.test" | grep -o "$BASE/password/reset/[A-Za-z0-9_-]*" | tr -d '\r')
check "operator reset link opens" "$(code_of jx "$RLINK")" 200
check "[A] new password saved through the link" "$(code_of jx -X POST "$RLINK" -d password=new-correct-horse-battery -d password2=new-correct-horse-battery)" 303
check "reset ends every session of the user" "$(code_of jr1 "$BASE/o/$A")" 303
check "reset link works only once" "$(code_of jx "$RLINK")" 404
check "old password refused" "$(login jr4 "owner@$A.test" "correct-horse-battery-$A")" ""
check "new password still needs the second step" "$(login jr4 "owner@$A.test" new-correct-horse-battery)" "$BASE/totp/verify"
# Self-service by email (Mailpit in dev)
code_of jx -X POST "$BASE/password/forgot" --data-urlencode "email=owner@$B.test" >/dev/null
code_of jx -X POST "$BASE/password/forgot" --data-urlencode "email=nobody-$RUN@example.test" >/dev/null
ELINK=$(mail_body "owner@$B.test" "Reset your password" | grep -o "$BASE/password/reset/[A-Za-z0-9_-]*" | head -1)
if [ -n "$ELINK" ]; then ok "forgot password: reset link emailed"; else bad "no reset email"; fi
check "unknown address: no email, same answer" "$(curl -s -G "http://localhost:8025/api/v1/search" --data-urlencode "query=to:nobody-$RUN@example.test" | grep -o '"messages_count":[0-9]*' | cut -d: -f2)" 0
check "[B] password reset by email" "$(code_of jx -X POST "$ELINK" -d password=b-new-correct-horse-battery -d password2=b-new-correct-horse-battery)" 303
if mail_body "owner@$B.test" "Your password was changed" | grep -q "was just changed"; then ok "owner told the password changed"; else bad "no password-changed email"; fi
admin reset-totp --email "owner@$B.test" >/dev/null
login jr5 "owner@$B.test" b-new-correct-horse-battery >/dev/null
check "after operator two-step reset, the user sets it up again" "$(c jr5 -o /dev/null -w '%{redirect_url}' "$BASE/totp/verify")" "$BASE/totp/setup"

echo
# Remove this run's test organizations (their apps, databases and networks follow), so
# repeated runs do not pile up containers. KEEP=1 keeps them for debugging.
if [ "${KEEP:-0}" != 1 ]; then
  owner_sql "DELETE FROM orgs WHERE slug IN ('$A', '$B', 't$RUN')" >/dev/null
  owner_sql "SELECT pg_notify('deployments', '')" >/dev/null
fi
section done; echo "Total: $(( $(date +%s) - T0 )) s"
echo "Result: $PASS passed, $FAIL failed"
rm -rf "$TMP"
[ "$FAIL" -eq 0 ]
