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
