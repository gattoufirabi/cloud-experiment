#!/usr/bin/env bash
# Sets up the example store in your organization, using most of the platform:
# a project with PostgreSQL, Redis, MongoDB and Kafka; three services built from ./app
# (public storefront, private API, worker); a bucket with an upload trigger; a queue; email;
# two functions (one behind a webhook); a scheduled job; a metric alarm and a budget.
#
# Usage (run again at any time: what exists is kept):
#   PLATFORM_URL=https://console.example.com ORG=acme SESSION=<session cookie> ./setup.sh
# SESSION is the value of your signed-in console cookie (__Host-sid; "sid" on a plain-HTTP
# development install: set COOKIE_NAME=sid). Optional: PROJECT (default shop),
# EMAIL_FROM ("Shop <shop@yourdomain.com>", a verified sending domain), REPORT_TO (an address
# for the daily report). Needs curl and platformctl (or Go, to run it from this repository).
set -euo pipefail
cd "$(dirname "$0")"
HERE=$(pwd -W 2>/dev/null || pwd)   # absolute (a Windows path under Git Bash, for platformctl)
: "${PLATFORM_URL:?set PLATFORM_URL}" "${ORG:?set ORG}" "${SESSION:?set SESSION (your console session cookie)}"
URL=${PLATFORM_URL%/}
P=${PROJECT:-shop}
CK=${COOKIE_NAME:-__Host-sid}
CURL=(curl -sS --max-time 120)
[ -n "${CURL_RESOLVE:-}" ] && CURL+=(--resolve "$CURL_RESOLVE")   # development: console.localhost:8088:127.0.0.1

step() { printf '\n== %s\n' "$*"; }
c() { "${CURL[@]}" -b "$CK=$SESSION" "$@"; }
csrf() { c "$URL/o/$ORG" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"$//'; }
CSRF=$(csrf)
[ -n "$CSRF" ] || { echo "not signed in: check SESSION and COOKIE_NAME" >&2; exit 1; }
# post <path> <form fields...>: a console form; prints its message, never stops the script
post() {
  local path=$1; shift
  local loc
  loc=$(c -o /dev/null -w '%{redirect_url}' -X POST "$URL$path" -d "csrf=$CSRF" "$@")
  local msg
  msg=$(printf '%s' "$loc" | grep -o '[?&]\(msg\|err\)=[^&#]*' | head -1 | sed 's/^.//; s/^msg=//; s/^err=/error: /' | sed 's/+/ /g; s/%\([0-9A-F][0-9A-F]\)/\\x\1/g')
  printf '   %b\n' "${msg:-done}"
}
pctl() {
  if command -v platformctl >/dev/null; then PLATFORM_URL=$URL PLATFORM_TOKEN=$TOKEN platformctl "$@"
  else (cd ../.. && PLATFORM_URL=$URL PLATFORM_TOKEN=$TOKEN go run ./cmd/platformctl "$@"); fi
}
api() { local m=$1 path=$2; shift 2; "${CURL[@]}" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -X "$m" "$URL/api/v1$path" "$@"; }
svc() { # svc <name> <kind> <env> [extra form fields]: create the service (if needed) and save its settings
  local name=$1 kind=$2 env=$3; shift 3
  post "/o/$ORG/p/$P/services" -d "slug=$name" -d image=nginx:1.27-alpine >/dev/null
  post "/o/$ORG/p/$P/s/$name/settings" -d "image=${IMAGE:-nginx:1.27-alpine}" -d port=8080 -d health_path=%2F -d cpu_millis=250 -d memory_mb=256 \
    -d instances=1 -d volume_path= -d "kind=$kind" --data-urlencode "env=$env" "$@"
}

step "API token for this setup (admin, kept in .setup-token)"
if [ -s .setup-token ]; then TOKEN=$(cat .setup-token); else
  TOKEN=$(c -X POST "$URL/o/$ORG/api-tokens" -d "csrf=$CSRF" -d name=store-setup -d role=admin | grep -o 'pla_[A-Za-z0-9_-]*' | head -1)
  [ -n "$TOKEN" ] || { echo "could not create an API token (admins only)" >&2; exit 1; }
  umask 077; echo "$TOKEN" > .setup-token
fi
echo "   ok"

step "Project $P"
post "/o/$ORG/projects" -d "slug=$P" -d name="Corner Shop"

step "Databases: PostgreSQL (orders), Redis (cache), MongoDB (reviews, statistics), Kafka (order events)"
post "/o/$ORG/p/$P/databases" -d slug=main -d engine=postgres:17
post "/o/$ORG/p/$P/databases" -d slug=cache -d engine=redis:7
post "/o/$ORG/p/$P/databases" -d slug=docs -d engine=mongodb:8.0
post "/o/$ORG/p/$P/databases" -d slug=events -d engine=kafka:3.9

step "Shared variables: the API's private address, the token of internal calls, the payment secret"
printf 'http://api.%s.internal' "$P" | pctl var set "$P" API_URL --plain
if ! pctl var list "$P" | grep -q INTERNAL_TOKEN; then
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' | pctl var set "$P" INTERNAL_TOKEN
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' | pctl var set "$P" PAYMENT_SECRET
fi
[ -n "${EMAIL_FROM:-}" ] && printf '%s' "$EMAIL_FROM" | pctl var set "$P" EMAIL_FROM --plain
[ -n "${REPORT_TO:-}" ] && printf '%s' "$REPORT_TO" | pctl var set "$P" REPORT_TO --plain

step "Services: api (private, metrics), web (public storefront), worker"
svc api web "ROLE=api" -d private=1 -d metrics_path=%2Fmetrics
svc web web "ROLE=web" -d edge_cache=1
svc worker worker "ROLE=worker"

step "Connections (stored as encrypted secrets of the services)"
for s in api worker; do
  post "/o/$ORG/p/$P/d/docs/attach" -d "service=$s" -d name=MONGODB_URI
  post "/o/$ORG/p/$P/d/events/attach" -d "service=$s" -d name=KAFKA_URL
done
post "/o/$ORG/p/$P/d/main/attach" -d service=api -d name=DATABASE_URL
post "/o/$ORG/p/$P/d/cache/attach" -d service=api -d name=REDIS_URL
pctl bucket create photos --quota 512 >/dev/null 2>&1 || true
post "/o/$ORG/storage/b/photos/attach" -d "service=$P/api"
pctl queue create emails >/dev/null 2>&1 || true
post "/o/$ORG/queues/emails/attach" -d "service=$P/api"
post "/o/$ORG/queues/emails/attach" -d "service=$P/worker"
post "/o/$ORG/email/attach" -d "service=$P/worker"

step "Build the app once (zip upload, built in the platform's sandbox) and deploy it to the three services"
pctl zip "$P/api" "$HERE/app" --wait
IMAGE=$(api GET "/projects/$P/services/api" | grep -o '"image":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "   image $IMAGE"
for s in web worker; do pctl deploy "$P/$s" --image "$IMAGE" --wait; done

step "Functions: photo-uploaded (bucket trigger) and payment-webhook (webhook URL)"
pctl fn deploy "$P/photo-uploaded" --file "$HERE/functions/photo-uploaded.js" --runtime nodejs22
pctl fn deploy "$P/payment-webhook" --file "$HERE/functions/payment-webhook.js" --runtime nodejs22
post "/o/$ORG/p/$P/f/photo-uploaded/triggers" -d bucket=photos -d events=created -d prefix=products/ -d suffix=.jpg
HOOK=$(c -X POST "$URL/o/$ORG/p/$P/f/payment-webhook/webhook" -d "csrf=$CSRF" | grep -o 'https\?://[^"<]*/hooks/fn/whk_[A-Za-z0-9_-]*' | head -1 || true)
[ -n "$HOOK" ] && { echo "   payment webhook: $HOOK"; echo "$HOOK" > .payment-webhook; }

step "Scheduled job: yesterday's sales, every morning at 07:00 UTC"
api PUT "/projects/$P/services/api/jobs/daily-report" -o /dev/null -w '   HTTP %{http_code}\n' -d '{"schedule": "0 7 * * *", "command": "node src/report.js", "timeout_seconds": 120}'

step "Metric alarm and monthly budget"
api POST /alarms -o /dev/null -w '   alarm: HTTP %{http_code}\n' -d '{"name": "store api memory", "query": "max(platform_container_memory_bytes)", "op": ">", "threshold": 230000000, "for_minutes": 10}'
api POST /budgets -o /dev/null -w '   budget: HTTP %{http_code}\n' -d "{\"project\": \"$P\", \"limit_cents\": 3000, \"thresholds\": [50, 80, 100]}"

step "Done"
WEB=$(api GET "/projects/$P/services/web" | grep -o '"url":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "   storefront: $WEB"
echo "   project map: $URL/o/$ORG/p/$P#map"
