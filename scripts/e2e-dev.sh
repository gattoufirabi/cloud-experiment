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
# section "name": prints how long the previous section took (to find the slow parts)
T0=$(date +%s); SEC_T=$T0; SEC_N=setup
section() { local now; now=$(date +%s); echo "   ($SEC_N: $((now - SEC_T)) s)"; SEC_N=$1; SEC_T=$now; echo "== $1"; }
# MSYS_NO_PATHCONV: Git Bash on Windows would otherwise rewrite /platform into a Windows path
admin() { MSYS_NO_PATHCONV=1 docker compose -f compose.dev.yml exec -T api /platform admin "$@"; }
live_cid() { docker compose -f compose.dev.yml exec -T postgres psql -qtA -U platform_owner platform -c "SELECT d.container_id FROM deployments d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$1' AND d.status = 'live'" | tr -d '\r'; }
psql_api() { docker compose -f compose.dev.yml exec -T postgres psql -qtA "postgres://platform_api:dev-api-password@localhost/platform" -c "$1"; }

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
owner_sql() { docker compose -f compose.dev.yml exec -T postgres psql -qtA -U platform_owner platform -c "$1" | tr -d '\r'; }
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
rd_cli() { dx exec "$RD_CID" sh -c "redis-cli -a \"\$REDIS_PASSWORD\" --no-auth-warning $1" 2>&1 | tr -d '\r'; }
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
check "timeout stops the run" "$(fn_api POST hello/invoke -d '{"sleep": 6000}' | grep -o 'Sandbox.Timedout')" Sandbox.Timedout
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
MSYS_NO_PATHCONV=1 docker run -d --name e2e-dns --network platform-core -p 8098:8098 -v "$(cygpath -w "$TMP/oidc"):/m:ro" alpine:3.20 /m/mockoidc -addr :8098 -dns :53 >/dev/null
dnsset() { curl -sS -X POST -G "http://127.0.0.1:8098/dns/set" --data-urlencode "name=$1" --data-urlencode "txt=$2"; }
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
st=; for _ in $(seq 1 10); do st=$(eapi "$TOK_DEV" GET "/messages/$EID" | grep -o '"status":"[a-z]*"' | cut -d'"' -f4); [ "$st" = sent ] && break; sleep 1; done
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
EMAX=$(owner_sql "SELECT p.max_emails_day FROM plans p JOIN orgs o ON o.plan_id = p.id WHERE o.slug = '$A'")
owner_sql "UPDATE plans SET max_emails_day = 2 WHERE id = (SELECT plan_id FROM orgs WHERE slug = '$A')" >/dev/null
check "[API] the daily limit answers 429" "$(ecode "$TOK_DEV" POST /send -d "$(ESEND buyer3-$RUN@example.org over)")" 429
owner_sql "UPDATE plans SET max_emails_day = $EMAX WHERE id = (SELECT plan_id FROM orgs WHERE slug = '$A')" >/dev/null
check "sent emails are metered" "$(for _ in $(seq 1 10); do n=$(owner_sql "SELECT coalesce(sum(emails), 0) FROM usage_hourly WHERE kind = 'email' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')"); [ "${n:-0}" -ge 2 ] && { echo yes; break; }; sleep 1; done)" yes
check "[A] domain deleted" "$(code_of ja -X POST "$BASE/o/$A/email/d/$ED/delete" -d csrf=$CSRF_A -d "confirm=$ED")" 303
owner_sql "DELETE FROM api_tokens WHERE name = 'email-probe'" >/dev/null
docker rm -f e2e-dns >/dev/null 2>&1

section "Single sign-on (OIDC)"
mkdir -p "$TMP/oidc"
(cd .. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$TMP/oidc/mockoidc" ./scripts/mockoidc) || bad "mock OIDC provider did not build"
docker rm -f e2e-oidc >/dev/null 2>&1
MSYS_NO_PATHCONV=1 docker run -d --name e2e-oidc -p 8097:8097 -v "$(cygpath -w "$TMP/oidc"):/m:ro" alpine:3.20 /m/mockoidc -addr :8097 -issuer http://host.docker.internal:8097 >/dev/null
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
check "Railpack-built app deployed and serving" "$(for _ in $(seq 1 45); do b=$(curl -s --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/"); echo "$b" | grep -q "built-with-railpack-$RUN" && { echo yes; break; }; sleep 2; done)" yes
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
  local n=0 h; while :; do h=$(printf '%s' "$1:$n" | sha256sum); [ "${h:0:2}" = 00 ] && { echo "$n"; return; }; n=$((n+1)); done; }
challenge() { c "$1" "$BASE/signup" | grep -o 'name="challenge" value="[^"]*"' | sed 's/.*value="//;s/"$//'; }
mail_body() { # mail_body <to> <subject text>: body of the newest matching message (Mailpit API)
  local id; for _ in $(seq 1 20); do
    id=$(curl -s -G "http://localhost:8025/api/v1/search" --data-urlencode "query=to:$1 subject:\"$2\"" | grep -o '"ID":"[^"]*"' | head -1 | cut -d'"' -f4)
    if [ -n "$id" ]; then curl -s "http://localhost:8025/api/v1/message/$id"; return; fi; sleep 1
  done; }
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
if MSYS_NO_PATHCONV=1 docker compose -f compose.dev.yml exec -T ntfy wget -qO- "http://localhost/platform-operator/json?poll=1&since=all" 2>/dev/null | grep -q "Trial suspended: $T"; then ok "operator notified"; else bad "no operator notice"; fi
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
    if MSYS_NO_PATHCONV=1 docker compose -f compose.dev.yml exec -T ntfy wget -qO- "http://localhost/$1/json?poll=1&since=all" 2>/dev/null | grep -q "$2"; then
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
owner_sql "UPDATE node_disk SET used_mb = 5, measured_at = now() WHERE org_id = (SELECT id FROM orgs WHERE slug = '$A'); UPDATE orgs SET disk_used_mb = 5 WHERE slug = '$A'" >/dev/null
check "owner alerted when over the disk limit" "$(owner_sql "SELECT count(*) FROM alert_incidents i JOIN orgs o ON o.id = i.org_id WHERE o.slug = '$A' AND i.kind = 'disk_quota'")" 1
check "[API] deploy refused above 110% of the disk limit (507)" "$(api_code "$TOK_DEV" -X POST "$BASE/api/v1/projects/shop/services/web/deploy")" 507
if c ja -L "$BASE$SVC_A/deploy" -d csrf=$CSRF_A | grep -q "Disk limit exceeded"; then ok "[A] console explains why the deploy is refused"; else bad "console disk message missing"; fi
admin set-plan --slug "$A" --plan pro >/dev/null
check "[API] deploys work again within the limit" "$(api_code "$TOK_DEV" -X POST "$BASE/api/v1/projects/shop/services/web/deploy")" 202

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
