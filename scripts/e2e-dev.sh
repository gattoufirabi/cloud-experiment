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
  code=$(code_of "$jar" -X POST "$BASE/totp/setup" -d "csrf=$csrf" -d "code=$(admin totp-code --secret "$secret" | tr -d '\r')")
  check "[$org] two-step login enrolled" "$code" 303
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
echo "== Tenant A ($A) and tenant B ($B)"
onboard ja "$A" "owner@$A.test"
onboard jb "$B" "owner@$B.test"

echo "== Unauthenticated access"
check "console redirects anonymous users to login" "$(curl -sS --resolve console.localhost:$PORT:127.0.0.1 -o /dev/null -w '%{http_code}' $BASE/o/$A)" 303

echo "== Tenant A deploys nginx with a secret"
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

echo "== Tenant B deploys its own app"
CSRF_B=$(c jb "$BASE/o/$B" | csrf_of)
code_of jb -X POST "$BASE/o/$B/projects" -d csrf=$CSRF_B -d slug=shop -d name=Shop >/dev/null
code_of jb -X POST "$BASE/o/$B/p/shop/services" -d csrf=$CSRF_B -d slug=web -d image=nginx:1.27-alpine >/dev/null
SVC_B=/o/$B/p/shop/s/web
code_of jb -X POST "$BASE$SVC_B/settings" -d csrf=$CSRF_B -d image=nginx:1.27-alpine -d port=80 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[B] deployment reaches live" "$(wait_status jb $SVC_B)" live
CID_B=$(live_cid "$B")

echo "== Isolation: B tries to reach A"
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

echo "== Failed deploy keeps the old version serving (A)"
code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=9999 -d health_path=%2F \
  -d cpu_millis=250 -d memory_mb=128 -d volume_path= -d env= -d deploy=1 >/dev/null
check "[A] broken deployment marked failed" "$(wait_status ja $SVC_A)" failed
BODY=$(curl -sS --resolve "$HOST_A:$PORT:127.0.0.1" "http://$HOST_A:$PORT/")
if echo "$BODY" | grep -q "Welcome to nginx"; then ok "[A] previous version still serving"; else bad "[A] app down after failed deploy"; fi

echo "== Logs and audit"
for _ in 1 2 3; do curl -sS --resolve "$HOST_A:$PORT:127.0.0.1" -o /dev/null "http://$HOST_A:$PORT/?probe=$RUN"; done
sleep 6
if c ja "$BASE$SVC_A/logs.json?q=probe=$RUN" | grep -q "probe=$RUN"; then ok "[A] request logs searchable in the console"; else bad "[A] logs not found"; fi
if c ja "$BASE/o/$A/audit" | grep -q "deployment.live"; then ok "[A] audit log records deployments"; else bad "[A] audit log missing events"; fi

echo "== Private registry and vulnerability scanning"
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

echo "== Per-client backups"
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

echo "== Managed PostgreSQL"
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

echo "== Managed Redis"
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

echo "== API tokens and platformctl"
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
TOK_ID=$(owner_sql "SELECT t.id FROM api_tokens t JOIN orgs o ON o.id = t.org_id WHERE o.slug = '$A' AND t.name = 'ci'")
code_of ja -X POST "$BASE/o/$A/api-tokens/$TOK_ID/delete" -d csrf=$CSRF_A >/dev/null
check "[API] revoked token refused (401)" "$(api_code "$TOK_DEV" "$BASE/api/v1/me")" 401

echo "== Public status pages"
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

echo "== Passkeys"
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

echo "== Traffic protection"
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
sleep 3
N429=0; for _ in $(seq 1 15); do [ "$(app_code /)" = 429 ] && N429=$((N429+1)); done
if [ "$N429" -ge 4 ]; then ok "visitors over the limit get 429 ($N429 of 15)"; else bad "rate limit not enforced ($N429 of 15 got 429)"; fi
check "[A] block every address" "$(code_of ja -X POST "$BASE$SVC_A/protection" -d csrf=$CSRF_A --data-urlencode $'blocked=0.0.0.0/0\n::/0')" 303
check "blocked visitors get 403" "$(wait_app_code / 403)" 403
check "protection saved in the audit log" "$(owner_sql "SELECT count(*) >= 3 FROM audit_events WHERE action = 'service.protection' AND target = 'service:web' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
check "[A] protection turned off" "$(code_of ja -X POST "$BASE$SVC_A/protection" -d csrf=$CSRF_A)" 303
check "app answers normally again" "$(wait_app_code "$XSS" 200)" 200

echo "== Usage metering and invoices"
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

echo "== Preview environments"
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

echo "== Builds from git"
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
if echo "$OUT" | grep -q "no Dockerfile found in nope"; then ok "missing Dockerfile reported clearly"; else bad "missing Dockerfile: $(echo "$OUT" | tail -2)"; fi
code_of ja -X POST "$BASE$SVC_A/source" -d csrf=$CSRF_A -d repo= -d branch=main >/dev/null
check "builds audited" "$(owner_sql "SELECT count(*) >= 2 FROM audit_events WHERE action = 'build.done' AND org_id = (SELECT id FROM orgs WHERE slug = '$A')")" t
docker rm -f e2e-git >/dev/null 2>&1

echo "== Self-signup and trials"
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
check "[signup] two-step login enrolled" "$(code_of jt -X POST "$BASE/totp/setup" -d "csrf=$(echo "$PAGE" | csrf_of)" -d "code=$(admin totp-code --secret "$SEC" | tr -d '\r')")" 303
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
# A container that burns its whole CPU allowance, like a crypto miner (ANOMALY_MINUTES=2 in dev).
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

echo "== Alerts"
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

docker pause "$CID_F" >/dev/null
ST=""; for _ in $(seq 1 30); do ST=$(incident_status files service_down); [ "$ST" = firing ] && break; sleep 2; done
check "[A] paused app detected: service_down incident open" "$ST" firing
check "[A] down alert pushed" "$(ntfy_has "alerts-$A" "Service down")" yes
check "[A] down alert sent to the webhook" "$(ntfy_has "hook-$A" "service_down")" yes
docker unpause "$CID_F" >/dev/null
ST=""; for _ in $(seq 1 30); do ST=$(incident_status files service_down); [ "$ST" = resolved ] && break; sleep 2; done
check "[A] recovery detected: incident resolved" "$ST" resolved
check "[A] resolved alert pushed" "$(ntfy_has "alerts-$A" "Resolved: Service down")" yes
if c ja "$BASE/o/$A" | grep -q "Service down"; then ok "[A] incident listed in the console"; else bad "[A] incident missing from the console"; fi

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

echo
echo "Result: $PASS passed, $FAIL failed"
rm -rf "$TMP"
[ "$FAIL" -eq 0 ]
