#!/usr/bin/env bash
# End-to-end test against a PRODUCTION platform, run on the server as root:
#   sudo bash /opt/platform/scripts/e2e-prod.sh
# Safe for real users: it works only in two fixed test organizations (e2etest-a and
# e2etest-b, kept between runs so their HTTPS certificates are reused), deletes what it
# creates, never pauses deployments or shows a banner, and gives its test account
# operator rights only while it runs. It needs platformctl at /usr/local/bin (or
# PLATFORMCTL=...). Parts that need the dev stack's test helpers (mock single sign-on,
# DNS and mail servers, local git server) are left to scripts/e2e-dev.sh.
set -uo pipefail
cd "$(dirname "$0")/../deploy"
ENVF=.env
val() { grep -m1 "^$1=" "$ENVF" | cut -d= -f2-; }
CONSOLE=$(val CONSOLE_HOST); APPS=$(val APPS_DOMAIN); REGHOST=$(val REGISTRY_HOST); S3HOST=$(val S3_HOST)
[ -n "$CONSOLE" ] && [ -n "$APPS" ] || { echo "CONSOLE_HOST and APPS_DOMAIN must be set in deploy/.env"; exit 2; }
BASE=https://$CONSOLE
A=e2etest-a; B=e2etest-b
PCTL=${PLATFORMCTL:-/usr/local/bin/platformctl}
PASSF=secrets/e2e-password
[ -s "$PASSF" ] || { head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' > "$PASSF"; chmod 600 "$PASSF"; }
PW="e2e-$(cat "$PASSF")"
RUN=$(date +%s | tail -c 5)
TMP=$(mktemp -d)
PASS=0; FAIL=0
ok()   { echo "  PASS  $*"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL  $*"; FAIL=$((FAIL+1)); }
check() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (expected $3, got $2)"; fi; }
T0=$(date +%s); SEC_T=$T0; SEC_N=setup
section() { local now; now=$(date +%s); echo "   ($SEC_N: $((now - SEC_T)) s)"; SEC_N=$1; SEC_T=$now; echo "== $1"; }
dc() { docker compose -f compose.prod.yml "$@"; }
admin() { dc exec -T api /platform admin "$@"; }
owner_sql() { dc exec -T postgres psql -qtA -U platform_owner platform -c "$1" | tr -d '\r'; }
res() { echo "--resolve $1:443:127.0.0.1"; }
c() { local jar=$1; shift; curl -sS --resolve "$CONSOLE:443:127.0.0.1" -b "$TMP/$jar" -c "$TMP/$jar" "$@"; }
code_of() { c "$1" -o /dev/null -w '%{http_code}' "${@:2}"; }
csrf_of() { grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"$//'; }
app_get() { curl -sS --max-time 20 --resolve "$1:443:127.0.0.1" "https://$1${2:-/}"; }
wait_status() { local s; for _ in $(seq 1 90); do s=$(c "$1" "$BASE$2/status.json" | grep -o '"Status":"[a-z]*"' | head -1 | cut -d'"' -f4); { [ "$s" = live ] || [ "$s" = failed ]; } && { echo "$s"; return; }; sleep 2; done; echo timeout; }
live_cid() { owner_sql "SELECT d.container_id FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$1' AND s.slug = '$2' AND d.status = 'live' ORDER BY d.revision DESC LIMIT 1"; }
api() { local t=$1 m=$2 p=$3; shift 3; curl -sS --resolve "$CONSOLE:443:127.0.0.1" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -X "$m" "$BASE/api/v1$p" "$@"; }
apicode() { local t=$1 m=$2 p=$3; shift 3; api "$t" "$m" "$p" -o /dev/null -w '%{http_code}' "$@"; }
yes_if() { sed 's/^[1-9][0-9]*$/yes/'; }   # a positive count -> yes

# login <jar> <org>: password, then (two-step reset first) enrol a fresh TOTP secret
login() {
  local jar=$1 org=$2 page secret csrf
  rm -f "$TMP/$jar"
  admin reset-totp --email "owner@$org.test" >/dev/null 2>&1
  c "$jar" -o /dev/null -X POST "$BASE/login" --data-urlencode "email=owner@$org.test" --data-urlencode "password=$PW"
  page=$(c "$jar" "$BASE/totp/setup")
  secret=$(grep -o 'class="select">[A-Z2-7]\{20,\}<' <<<"$page" | head -1 | sed 's/.*">//;s/<$//')
  csrf=$(csrf_of <<<"$page")
  c "$jar" -o /dev/null -X POST "$BASE/totp/setup" -d "csrf=$csrf" -d "code=$(admin totp-code --secret "$secret" | tr -d '\r')"
  check "[$org] signed in with two-step login" "$(code_of "$jar" "$BASE/o/$org")" 200
}
# ensure_org <org>: create the test organization the first time
ensure_org() {
  local org=$1
  if [ -z "$(owner_sql "SELECT 1 FROM orgs WHERE slug = '$org'")" ]; then
    local link token
    link=$(admin create-org --slug "$org" --name "E2E test ${org: -1}" --owner-email "owner@$org.test" --plan pro | tail -1 | tr -d '\r')
    token=${link##*/invite/}
    c "x$org" -o /dev/null -X POST "$BASE/invite/$token" --data-urlencode "name=E2E owner" --data-urlencode "password=$PW" --data-urlencode "password2=$PW"
  fi
  owner_sql "UPDATE orgs SET suspended = false, suspend_reason = '' WHERE slug = '$org'" >/dev/null
  admin set-plan --slug "$org" --plan pro >/dev/null
}
# cleanup <org> <jar>: delete everything the suite creates (left over from a failed run too)
cleanup() {
  local org=$1 jar=$2 csrf p s
  csrf=$(c "$jar" "$BASE/o/$org/compute" | csrf_of)
  [ -n "$csrf" ] || return
  for row in $(owner_sql "SELECT p.slug || '/' || f.slug FROM functions f JOIN projects p ON p.id = f.project_id JOIN orgs o ON o.id = f.org_id WHERE o.slug = '$org'"); do
    code_of "$jar" -X POST "$BASE/o/$org/p/${row%%/*}/f/${row##*/}/delete" -d csrf=$csrf -d "confirm=${row##*/}" >/dev/null; done
  for row in $(owner_sql "SELECT p.slug || '/' || d.slug FROM databases d JOIN projects p ON p.id = d.project_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$org'"); do
    code_of "$jar" -X POST "$BASE/o/$org/p/${row%%/*}/d/${row##*/}/delete" -d csrf=$csrf -d "confirm=${row##*/}" >/dev/null; done
  local sel=()
  for row in $(owner_sql "SELECT p.slug || '/' || s.slug FROM services s JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$org' AND s.preview_of IS NULL"); do sel+=(-d "sel=$row"); done
  [ ${#sel[@]} -gt 0 ] && code_of "$jar" -X POST "$BASE/o/$org/compute/actions" -d csrf=$csrf -d action=delete -d confirm=delete "${sel[@]}" >/dev/null
  for b in $(owner_sql "SELECT b.name FROM buckets b JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$org'"); do
    code_of "$jar" -X POST "$BASE/o/$org/storage/b/${b#$org-}/delete" -d csrf=$csrf -d "confirm=${b#$org-}" >/dev/null; done
  for q in $(owner_sql "SELECT q.name FROM queues q JOIN orgs o ON o.id = q.org_id WHERE o.slug = '$org'"); do
    code_of "$jar" -X POST "$BASE/o/$org/queues/$q/delete" -d csrf=$csrf -d "confirm=$q" >/dev/null; done
  for sname in $(owner_sql "SELECT s.name FROM org_secrets s JOIN orgs o ON o.id = s.org_id WHERE o.slug = '$org'"); do
    code_of "$jar" -X POST "$BASE/o/$org/secrets/$sname/delete" -d csrf=$csrf -d "confirm=$sname" >/dev/null; done
  owner_sql "DELETE FROM api_tokens WHERE org_id = (SELECT id FROM orgs WHERE slug = '$org'); DELETE FROM registry_tokens WHERE org_id = (SELECT id FROM orgs WHERE slug = '$org'); DELETE FROM invites WHERE org_id = (SELECT id FROM orgs WHERE slug = '$org') AND accepted_at IS NULL" >/dev/null 2>&1
}
finish() {
  admin make-operator --email "owner@$A.test" --undo >/dev/null 2>&1
  [ -s "$TMP/ja" ] && cleanup "$A" ja
  [ -s "$TMP/jb" ] && cleanup "$B" jb
  for o in $A $B; do admin reset-totp --email "owner@$o.test" >/dev/null 2>&1; done   # nobody keeps a working second factor
  rm -rf "$TMP"
}
trap finish EXIT

section "Preflight"
check "API healthy" "$(dc ps api --format '{{.Status}}' | grep -c healthy)" 1
check "console login page" "$(curl -sS -o /dev/null -w '%{http_code}' --resolve "$CONSOLE:443:127.0.0.1" "$BASE/login")" 200
check "a node reported in the last 2 minutes" "$(owner_sql "SELECT count(*) > 0 FROM nodes WHERE last_seen > now() - interval '2 minutes'")" t
check "every migration applied" "$(ls ../migrations/*.sql | wc -l | tr -d ' ')" "$(owner_sql "SELECT count(*) FROM schema_migrations")"
check "deployments are not paused" "$(owner_sql "SELECT deploys_paused FROM platform_settings")" f

section "Test organizations"
ensure_org "$A"; ensure_org "$B"
login ja "$A"; login jb "$B"
CSRF_A=$(c ja "$BASE/o/$A" | csrf_of); CSRF_B=$(c jb "$BASE/o/$B" | csrf_of)
cleanup "$A" ja; cleanup "$B" jb
for o in A B; do j=j${o,,}; org=${!o}; cs=CSRF_$o
  [ -n "$(owner_sql "SELECT 1 FROM projects p JOIN orgs g ON g.id = p.org_id WHERE g.slug = '$org' AND p.slug = 'shop'")" ] || code_of $j -X POST "$BASE/o/$org/projects" -d csrf=${!cs} -d slug=shop -d name=Shop >/dev/null; done

section "Deploy, HTTPS and the sandbox"
SVC_A=/o/$A/p/shop/s/web; SVC_B=/o/$B/p/shop/s/web
HOST_A=web-shop-$A.$APPS; HOST_B=web-shop-$B.$APPS
for o in A B; do j=j${o,,}; org=${!o}; cs=CSRF_$o
  code_of $j -X POST "$BASE/o/$org/p/shop/services" -d csrf=${!cs} -d slug=web -d image=nginx:1.27-alpine >/dev/null
  code_of $j -X POST "$BASE/o/$org/p/shop/s/web/secrets" -d csrf=${!cs} -d name=API_KEY -d "value=secret-$org-$RUN" >/dev/null
  check "[$org] settings saved and deployed" "$(code_of $j -X POST "$BASE/o/$org/p/shop/s/web/settings" -d csrf=${!cs} -d image=nginx:1.27-alpine -d port=80 -d health_path=/ -d cpu_millis=250 -d memory_mb=128 -d instances=1 -d volume_path= -d env=GREETING=hello -d deploy=1)" 303
done
check "[A] live" "$(wait_status ja $SVC_A)" live
check "[B] live" "$(wait_status jb $SVC_B)" live
check "[A] served over HTTPS with a valid certificate" "$(for _ in $(seq 1 20); do app_get "$HOST_A" | grep -q nginx && { echo yes; break; }; sleep 3; done)" yes
CID_A=$(live_cid "$A" web); CID_B=$(live_cid "$B" web)
check "[A] runs under gVisor" "$(docker inspect -f '{{.HostConfig.Runtime}}' "$CID_A")" runsc
check "[A] secret injected" "$(docker exec "$CID_A" printenv API_KEY | tr -d '\r')" "secret-$A-$RUN"
check "[A] all capabilities dropped, not privileged, no new privileges" "$(docker inspect -f '{{json .HostConfig.CapDrop}}:{{.HostConfig.Privileged}}:{{json .HostConfig.SecurityOpt}}' "$CID_A")" '["ALL"]:false:["no-new-privileges:true"]'
check "[A] CPU and memory limits" "$(docker inspect -f '{{.HostConfig.NanoCpus}}:{{.HostConfig.Memory}}' "$CID_A")" "250000000:134217728"

section "Isolation between tenants"
for p in "/o/$A" "$SVC_A" "$SVC_A/logs.json"; do check "[B->A] $p hidden (404)" "$(code_of jb "$BASE$p")" 404; done
check "[B->A] deploy A's service with B's CSRF refused (404)" "$(code_of jb -X POST "$BASE$SVC_A/deploy" -d csrf=$CSRF_B)" 404
check "[B] request without CSRF refused (403)" "$(code_of jb -X POST "$BASE$SVC_B/deploy")" 403
IP_A=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$CID_A" | awk '{print $1}')
if docker exec "$CID_B" sh -c "wget -q -T 3 -O /dev/null http://$IP_A/" >/dev/null 2>&1; then bad "[B->A] B's container reached A's container"; else ok "[B->A] B's container cannot reach A's container"; fi
if docker exec "$CID_A" sh -c "wget -q -T 3 -O /dev/null http://postgres:5432" >/dev/null 2>&1; then bad "[A] tenant reached the platform database"; else ok "[A] tenant cannot reach the platform's services"; fi

section "Failed deploy keeps the old version"
code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=9999 -d health_path=/ -d cpu_millis=250 -d memory_mb=128 -d instances=1 -d volume_path= -d env=GREETING=hello -d deploy=1 >/dev/null
check "[A] broken deployment marked failed" "$(for _ in $(seq 1 90); do st=$(owner_sql "SELECT d.status FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND s.slug = 'web' ORDER BY d.revision DESC LIMIT 1"); [ "$st" = failed ] && { echo failed; break; }; sleep 3; done)" failed
check "[A] the previous version still serves" "$(app_get "$HOST_A" | grep -c nginx | yes_if)" yes
code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d image=nginx:1.27-alpine -d port=80 -d health_path=/ -d cpu_millis=250 -d memory_mb=128 -d instances=1 -d volume_path= -d env=GREETING=hello -d deploy=1 >/dev/null
check "[A] fixed and live again" "$(wait_status ja $SVC_A)" live

section "Logs and audit"
for _ in 1 2 3; do app_get "$HOST_A" "/?probe=$RUN" >/dev/null; done
check "[A] request lines searchable" "$(for _ in $(seq 1 30); do grep -q "probe=$RUN" <<<"$(c ja "$BASE$SVC_A/logs.json?q=probe=$RUN")" && { echo yes; break; }; sleep 2; done)" yes
check "[A] deployments audited" "$(owner_sql "SELECT count(*) FROM audit_events a JOIN orgs o ON o.id = a.org_id WHERE o.slug = '$A' AND a.action = 'deployment.live' AND a.created_at > now() - interval '1 hour'" | yes_if)" yes

section "Private registry"
if [ -n "$REGHOST" ]; then
  TOK_REG=$(c ja -X POST "$BASE/o/$A/registry-tokens" -d csrf=$CSRF_A -d name=e2e -d push=1 | grep -o 'plr_[A-Za-z0-9_-]*' | head -1)
  DCFG=$TMP/docker; mkdir -p "$DCFG"
  mkdir -p "$TMP/img"; printf '%s\n' 'FROM nginx:1.27-alpine' "RUN echo registry-$RUN > /usr/share/nginx/html/index.html" > "$TMP/img/Dockerfile"
  docker build -q -t "$REGHOST/$A/web:e2e" "$TMP/img" >/dev/null
  check "[A] docker login with its token" "$(echo "$TOK_REG" | DOCKER_CONFIG=$DCFG docker login "$REGHOST" -u "$A" --password-stdin >/dev/null 2>&1 && echo yes)" yes
  check "[A] push to its own namespace" "$(DOCKER_CONFIG=$DCFG docker push -q "$REGHOST/$A/web:e2e" >/dev/null 2>&1 && echo yes)" yes
  docker tag "$REGHOST/$A/web:e2e" "$REGHOST/$B/web:stolen"
  check "[A->B] push to B's namespace refused" "$(DOCKER_CONFIG=$DCFG docker push -q "$REGHOST/$B/web:stolen" >/dev/null 2>&1 && echo yes || echo no)" no
  code_of ja -X POST "$BASE$SVC_A/settings" -d csrf=$CSRF_A -d "image=$REGHOST/$A/web:e2e" -d port=80 -d health_path=/ -d cpu_millis=250 -d memory_mb=128 -d instances=1 -d volume_path= -d env= -d deploy=1 >/dev/null
  check "[A] deployed from its private registry" "$(wait_status ja $SVC_A):$(sleep 2; app_get "$HOST_A")" "live:registry-$RUN"
  check "[A] pushed image scanned" "$(for _ in $(seq 1 60); do s=$(owner_sql "SELECT status FROM image_scans WHERE image = '$REGHOST/$A/web:e2e' ORDER BY created_at DESC LIMIT 1"); [ "$s" = done ] || [ "$s" = error ] && { echo "$s"; break; }; sleep 3; done)" done
  docker rmi -f "$REGHOST/$A/web:e2e" "$REGHOST/$B/web:stolen" >/dev/null 2>&1
fi

section "API tokens and platformctl"
TOK=$(c ja -X POST "$BASE/o/$A/api-tokens" -d csrf=$CSRF_A -d name=e2e -d role=developer | grep -o 'pla_[A-Za-z0-9_-]*' | head -1)
TOK_B=$(c jb -X POST "$BASE/o/$B/api-tokens" -d csrf=$CSRF_B -d name=e2e -d role=developer | grep -o 'pla_[A-Za-z0-9_-]*' | head -1)
check "[API] whoami names the organization" "$(api "$TOK" GET /whoami | grep -o "\"organization\":\"$A\"")" "\"organization\":\"$A\""
check "[B API] B's token is bound to B (sees only its own organization)" "$(api "$TOK_B" GET /whoami | grep -o "\"organization\":\"[a-z0-9-]*\"")" "\"organization\":\"$B\""
check "[API] an invalid token is refused (401)" "$(apicode "pla_not-a-real-token" GET /whoami)" 401
pctl() { PLATFORM_URL="$BASE" PLATFORM_TOKEN="$TOK" "$PCTL" "$@" 2>&1; }
if [ -x "$PCTL" ]; then
  check "[CLI] platformctl status shows the live revision" "$(pctl status shop/web | grep -c live | yes_if)" yes
else bad "platformctl not installed at $PCTL"; fi

section "Managed databases (PostgreSQL, MongoDB, Redis)"
db_st() { owner_sql "SELECT d.$2 FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = '$1'"; }
db_job() { local st=; for _ in $(seq 1 120); do st=$(owner_sql "SELECT j.status FROM backup_jobs j JOIN databases d ON d.id = j.database_id JOIN orgs o ON o.id = j.org_id WHERE o.slug = '$A' AND d.slug = '$1' AND j.kind = '$2' ORDER BY j.created_at DESC LIMIT 1"); [ "$st" = done ] || [ "$st" = error ] && break; sleep 3; done; echo "$st"; }
db_wait() { local st=; for _ in $(seq 1 150); do st=$(db_st "$1" status); [ "$st" = running ] || [ "$st" = failed ] && break; sleep 2; done; echo "$st"; }
for e in pg:postgres:17 mongo:mongodb:8.0 cache:redis:7; do code_of ja -X POST "$BASE/o/$A/p/shop/databases" -d csrf=$CSRF_A -d slug=${e%%:*} -d "engine=${e#*:}" >/dev/null; done
check "[A] PostgreSQL running" "$(db_wait pg)" running
check "[A] MongoDB running (512 MB)" "$(db_wait mongo):$(db_st mongo memory_mb)" "running:512"
check "[A] Redis running" "$(db_wait cache)" running
PG=$(db_st pg container_id); MG=$(db_st mongo container_id)
docker exec "$PG" sh -c "PGPASSWORD=\$POSTGRES_PASSWORD psql -qtA -h 127.0.0.1 -U app app -c \"CREATE TABLE t (v text); INSERT INTO t VALUES ('pg-$RUN')\"" >/dev/null
mongo_js() { docker exec "$MG" sh -c "mongosh --quiet --host 127.0.0.1 -u \"\$MONGO_INITDB_ROOT_USERNAME\" -p \"\$MONGO_INITDB_ROOT_PASSWORD\" --authenticationDatabase admin app --eval '$1'" 2>&1 | tr -d '\r'; }
mongo_js "db.notes.insertOne({t: \"mg-$RUN\"})" >/dev/null
check "[A] MongoDB stores documents" "$(mongo_js 'db.notes.findOne().t')" "mg-$RUN"
for d in pg mongo; do code_of ja -X POST "$BASE/o/$A/p/shop/d/$d/backups" -d csrf=$CSRF_A >/dev/null; check "[A] $d backup completes" "$(db_job $d backup)" done; done
SNAP=$(owner_sql "SELECT b.snapshot_id FROM backup_snapshots b JOIN databases d ON d.id = b.database_id JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND d.slug = 'mongo' ORDER BY b.taken_at DESC LIMIT 1")
mongo_js "db.notes.updateOne({}, {\$set: {t: \"changed\"}})" >/dev/null
code_of ja -X POST "$BASE/o/$A/p/shop/d/mongo/backups/restore/$SNAP" -d csrf=$CSRF_A -d confirm=mongo >/dev/null
check "[A] MongoDB restore brings the document back" "$(db_job mongo restore):$(mongo_js 'db.notes.findOne().t')" "done:mg-$RUN"
HOSTPG=$(owner_sql "SELECT 'pd-' || left(replace(d.id::text, '-', ''), 12) FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug = '$A' AND d.slug = 'pg'")
if docker exec "$CID_B" sh -c "echo | nc -w 3 $HOSTPG 5432" >/dev/null 2>&1; then bad "[B->A] B reached A's database"; else ok "[B->A] B cannot reach A's database"; fi

section "Object storage"
code_of ja -X POST "$BASE/o/$A/storage/buckets" -d csrf=$CSRF_A -d name=files -d quota_mb=5 >/dev/null
check "[A] bucket created" "$(for _ in $(seq 1 20); do [ -n "$(owner_sql "SELECT 1 FROM buckets b JOIN orgs o ON o.id = b.org_id WHERE o.slug = '$A' AND b.name = '$A-files' AND b.garage_id IS NOT NULL")" ] && { echo yes; break; }; sleep 1; done)" yes
KP=$(c ja -X POST "$BASE/o/$A/storage/keys" -d csrf=$CSRF_A -d name=e2e -d "perm_files=write")
KID=$(grep -o 'GK[0-9a-f]\{24\}' <<<"$KP" | head -1); KSEC=$(sed -n 's/.*select wrap">\([0-9a-f]\{64\}\)<.*/\1/p' <<<"$KP" | head -1)
s3c() { curl -sS --resolve "$S3HOST:443:127.0.0.1" --aws-sigv4 "aws:amz:us-east-1:s3" --user "$KID:$KSEC" "$@"; }
sleep 3
check "[S3] signed upload" "$(echo "hello-$RUN" | s3c -o /dev/null -w '%{http_code}' -T - "https://$S3HOST/$A-files/hello.txt")" 200
check "[S3] object reads back" "$(s3c "https://$S3HOST/$A-files/hello.txt")" "hello-$RUN"
check "[S3] anonymous read refused" "$(curl -sS -o /dev/null -w '%{http_code}' --resolve "$S3HOST:443:127.0.0.1" "https://$S3HOST/$A-files/hello.txt")" 403
s3c -X DELETE "https://$S3HOST/$A-files/hello.txt" >/dev/null

section "Functions and web apps"
fn() { local m=$1 p=$2; shift 2; api "$TOK" "$m" "/projects/shop/functions/$p" "$@"; }
CODE='export const handler = async (event) => { if (event.sleep) await new Promise((r) => setTimeout(r, event.sleep)); return { echo: event.value ?? null, method: event.requestContext?.http?.method ?? null }; };'
check "[API] function deployed" "$(fn PUT hello -o /dev/null -w '%{http_code}' -d "$(python3 -c 'import json,sys; print(json.dumps({"runtime": "nodejs22", "code": sys.argv[1], "timeout_seconds": 3, "url_enabled": True, "url_auth": "none"}))' "$CODE")")" 201
check "[API] synchronous invoke" "$(fn POST hello/invoke -d '{"value": 42}' | grep -o '"echo":42')" '"echo":42'
check "the timeout stops the run" "$(fn POST hello/invoke -d '{"sleep": 6000}' | grep -o Sandbox.Timedout)" Sandbox.Timedout
FNH=hello-shop-$A.fn.$APPS
check "function URL over HTTPS" "$(for _ in $(seq 1 20); do r=$(app_get "$FNH" /x); grep -q '"method":"GET"' <<<"$r" && { echo yes; break; }; sleep 3; done)" yes
code_of ja -X POST "$BASE/o/$A/functions" -d csrf=$CSRF_A -d project=shop -d slug=webapp -d runtime=python3.13 -d kind=web >/dev/null
fn PUT webapp -o /dev/null -d '{"url_enabled": true, "url_auth": "none"}'
WAH=webapp-shop-$A.fn.$APPS
check "Python web app answers on its URL" "$(for _ in $(seq 1 20); do r=$(app_get "$WAH" /hi); grep -q 'Hello from a web app' <<<"$r" && { echo yes; break; }; sleep 3; done)" yes
check "[API] other events reach the web app as POST /events" "$(fn POST webapp/invoke -d '{"n": 7}' | grep -o '"received":{"n":7}')" '"received":{"n":7}'

section "Zip deployment"
mkdir -p "$TMP/zipapp/site"; printf '%s\n' 'FROM nginx:1.27-alpine' 'COPY site/index.html /usr/share/nginx/html/index.html' > "$TMP/zipapp/Dockerfile"
echo "zip-$RUN" > "$TMP/zipapp/site/index.html"
OUT=$(pctl zip shop/web "$TMP/zipapp" --wait)
if grep -q "done: .*/$A/web:zip-" <<<"$OUT"; then ok "[CLI] folder uploaded, built in the sandbox and pushed"; else bad "zip build: $(tail -3 <<<"$OUT")"; fi
check "the zip build is deployed over HTTPS" "$(for _ in $(seq 1 45); do app_get "$HOST_A" | grep -q "zip-$RUN" && { echo yes; break; }; sleep 2; done)" yes

section "Queues and secrets manager"
check "[API] queue created" "$(apicode "$TOK" POST /queues -d '{"name": "orders"}')" 201
apicode "$TOK" POST /queues/orders/messages -d '{"body": "order-1"}' >/dev/null
R=$(api "$TOK" POST "/queues/orders/receive?max=10"); RC=$(grep -o '"receipt":"[^"]*"' <<<"$R" | head -1 | cut -d'"' -f4)
check "[API] receive and delete a message" "$(grep -c order-1 <<<"$R"):$(apicode "$TOK" DELETE "/queues/orders/messages/$RC")" "1:204"
check "[B->A API] B cannot reach A's queue (404)" "$(apicode "$TOK_B" POST /queues/orders/messages -d '{"body": "x"}')" 404
check "[API] secret created" "$(apicode "$TOK" POST /secrets -d "{\"name\": \"e2e-key\", \"value\": \"v1-$RUN\"}")" 201
check "the secret is stored encrypted" "$(owner_sql "SELECT count(*) FROM org_secret_versions WHERE position(convert_to('v1-$RUN', 'UTF8') in value_enc) > 0")" 0
check "[B->A API] B cannot read A's secret (404)" "$(apicode "$TOK_B" GET /secrets/e2e-key)" 404

section "Operator and admin panel"
admin make-operator --email "owner@$A.test" >/dev/null
for p in /operator /operator/users /operator/resources /operator/plans /operator/health /operator/settings; do check "operator page $p" "$(code_of ja "$BASE$p")" 200; done
check "[B] operator pages hidden (404)" "$(code_of jb "$BASE/operator")" 404
check "resources list the test organization" "$(c ja "$BASE/operator/resources?q=$A" | grep -c "<td>service</td>" | yes_if)" yes
if c ja "$BASE/operator/orgs/$A" | grep -q "secret-$A-$RUN\|GREETING=hello"; then bad "operator view leaks secrets or environment"; else ok "operator view shows no secrets or environment"; fi
check "the user page lists the account" "$(c ja "$BASE/operator/users?q=owner@$B.test" | grep -c "owner@$B.test" | yes_if)" yes
admin make-operator --email "owner@$A.test" --undo >/dev/null
check "operator rights revoked after the checks" "$(owner_sql "SELECT is_operator FROM users WHERE email = 'owner@$A.test'")" f

section "Cleanup"
cleanup "$A" ja; cleanup "$B" jb
check "test resources removed" "$(for _ in $(seq 1 30); do n=$(owner_sql "SELECT (SELECT count(*) FROM services s JOIN orgs o ON o.id = s.org_id WHERE o.slug IN ('$A', '$B')) + (SELECT count(*) FROM databases d JOIN orgs o ON o.id = d.org_id WHERE o.slug IN ('$A', '$B')) + (SELECT count(*) FROM functions f JOIN orgs o ON o.id = f.org_id WHERE o.slug IN ('$A', '$B'))"); [ "$n" = 0 ] && { echo yes; break; }; sleep 2; done)" yes
check "their containers are gone" "$(for _ in $(seq 1 45); do n=$(docker ps -aq --filter "label=platform.org=$(owner_sql "SELECT id FROM orgs WHERE slug = '$A'")" | wc -l); [ "$n" = 0 ] && { echo yes; break; }; sleep 2; done)" yes

section done
echo "Total: $(( $(date +%s) - T0 )) s"
echo "Result: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
