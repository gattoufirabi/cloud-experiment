#!/usr/bin/env bash
# Sets up the example store in your organization, using most of the platform:
# a project with PostgreSQL, Redis, MongoDB and Kafka; three services built from ./app
# (public storefront, private API, worker); a bucket with an upload trigger; a queue; email;
# two functions (one behind a webhook); a scheduled job; a metric alarm and a budget.
#
# Usage (run again at any time: what exists is kept):
#   PLATFORM_URL=https://console.example.com PLATFORM_TOKEN=pla_... ./setup.sh
# PLATFORM_TOKEN is an API token with the admin role (console › Access › API tokens).
# Optional: PROJECT (default shop), EMAIL_FROM ("Shop <shop@yourdomain.com>", a verified
# sending domain), REPORT_TO (an address for the daily report).
# Needs curl, and platformctl (or Go, to run it from this repository).
set -euo pipefail
cd "$(dirname "$0")"
HERE=$(pwd -W 2>/dev/null || pwd)   # absolute (a Windows path under Git Bash, for platformctl)
: "${PLATFORM_URL:?set PLATFORM_URL}" "${PLATFORM_TOKEN:?set PLATFORM_TOKEN (an admin API token)}"
URL=${PLATFORM_URL%/}
TOKEN=$PLATFORM_TOKEN
P=${PROJECT:-shop}
CURL=(curl -sS --max-time 120)
[ -n "${CURL_RESOLVE:-}" ] && CURL+=(--resolve "$CURL_RESOLVE")   # development: console.localhost:8088:127.0.0.1

step() { printf '\n== %s\n' "$*"; }
api() { local m=$1 path=$2; shift 2; "${CURL[@]}" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -X "$m" "$URL/api/v1$path" "$@"; }
# act <method> <path> <json>: a console action through the API; prints its message, never stops
act() {
  local out
  out=$(api "$1" "$2" -d "$3" || true)
  printf '   %s\n' "$(printf '%s' "$out" | sed -n 's/.*"\(message\|error\)":"\([^"]*\)".*/\1: \2/p' | sed 's/^message: //')"
}
pctl() {
  if command -v platformctl >/dev/null; then PLATFORM_URL=$URL PLATFORM_TOKEN=$TOKEN platformctl "$@"
  else (cd ../.. && PLATFORM_URL=$URL PLATFORM_TOKEN=$TOKEN go run ./cmd/platformctl "$@"); fi
}
svc() { # svc <name> <kind> <env> [extra JSON fields]: create the service (if needed) and save its settings
  local name=$1 kind=$2 env=$3 extra=${4:-}
  act POST "/projects/$P/services" "{\"slug\": \"$name\", \"image\": \"nginx:1.27-alpine\"}" >/dev/null
  act PUT "/projects/$P/services/$name/settings" "{\"image\": \"${IMAGE:-nginx:1.27-alpine}\", \"port\": 8080, \"health_path\": \"/\",
    \"cpu_millis\": 250, \"memory_mb\": 256, \"instances\": 1, \"volume_path\": \"\", \"kind\": \"$kind\", \"env\": \"$env\"$extra}"
}

api GET /me -o /dev/null -f || { echo "the token is not accepted by $URL" >&2; exit 1; }

step "Project $P"
act POST /projects "{\"slug\": \"$P\", \"name\": \"Corner Shop\"}"

step "Databases: PostgreSQL (orders), Redis (cache), MongoDB (reviews, statistics), Kafka (order events)"
act POST "/projects/$P/databases" '{"slug": "main", "engine": "postgres:17"}'
act POST "/projects/$P/databases" '{"slug": "cache", "engine": "redis:7"}'
act POST "/projects/$P/databases" '{"slug": "docs", "engine": "mongodb:8.0"}'
act POST "/projects/$P/databases" '{"slug": "events", "engine": "kafka:3.9"}'

step "Shared variables: the API's private address, the token of internal calls, the payment secret"
printf 'http://api.%s.internal' "$P" | pctl var set "$P" API_URL --plain
if ! pctl var list "$P" | grep -q INTERNAL_TOKEN; then
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' | pctl var set "$P" INTERNAL_TOKEN
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' | pctl var set "$P" PAYMENT_SECRET
fi
[ -n "${EMAIL_FROM:-}" ] && printf '%s' "$EMAIL_FROM" | pctl var set "$P" EMAIL_FROM --plain
[ -n "${REPORT_TO:-}" ] && printf '%s' "$REPORT_TO" | pctl var set "$P" REPORT_TO --plain

step "Services: api (private, metrics), web (public storefront, edge cache), worker"
svc api web "ROLE=api" ', "private": true, "metrics_path": "/metrics"'
svc web web "ROLE=web" ', "edge_cache": true'
svc worker worker "ROLE=worker"

step "Connections (stored as encrypted secrets of the services)"
for s in api worker; do
  act POST "/projects/$P/databases/docs/attach" "{\"service\": \"$s\", \"name\": \"MONGODB_URI\"}"
  act POST "/projects/$P/databases/events/attach" "{\"service\": \"$s\", \"name\": \"KAFKA_URL\"}"
done
act POST "/projects/$P/databases/main/attach" '{"service": "api", "name": "DATABASE_URL"}'
act POST "/projects/$P/databases/cache/attach" '{"service": "api", "name": "REDIS_URL"}'
pctl bucket create photos --quota 512 >/dev/null 2>&1 || true
act POST /storage/buckets/photos/attach "{\"service\": \"$P/api\"}"
pctl queue create emails >/dev/null 2>&1 || true
act POST /queues/emails/attach "{\"service\": \"$P/api\"}"
act POST /queues/emails/attach "{\"service\": \"$P/worker\"}"
act POST /email/attach "{\"service\": \"$P/worker\"}"

step "Build the app once (zip upload, built in the platform's sandbox) and deploy it to the three services"
pctl zip "$P/api" "$HERE/app" --wait
IMAGE=$(api GET "/projects/$P/services/api" | grep -o '"image":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "   image $IMAGE"
for s in web worker; do pctl deploy "$P/$s" --image "$IMAGE" --wait; done

step "Functions: photo-uploaded (bucket trigger) and payment-webhook (webhook URL)"
pctl fn deploy "$P/photo-uploaded" --file "$HERE/functions/photo-uploaded.js" --runtime nodejs22
pctl fn deploy "$P/payment-webhook" --file "$HERE/functions/payment-webhook.js" --runtime nodejs22
act POST "/projects/$P/functions/photo-uploaded/triggers" '{"bucket": "photos", "events": "created", "prefix": "products/", "suffix": ".jpg"}'
if [ ! -s .payment-webhook ]; then
  HOOK=$(api POST "/projects/$P/functions/payment-webhook/webhook" -d '{}' | grep -o '"url":"[^"]*"' | cut -d'"' -f4 || true)
  [ -n "$HOOK" ] && echo "$HOOK" > .payment-webhook
fi
[ -s .payment-webhook ] && echo "   payment webhook: $(cat .payment-webhook)"

step "Scheduled job: yesterday's sales, every morning at 07:00 UTC"
api PUT "/projects/$P/services/api/jobs/daily-report" -o /dev/null -w '   HTTP %{http_code}\n' -d '{"schedule": "0 7 * * *", "command": "node src/report.js", "timeout_seconds": 120}'

step "Metric alarm and monthly budget"
api POST /alarms -o /dev/null -w '   alarm: HTTP %{http_code}\n' -d '{"name": "store api memory", "query": "max(platform_container_memory_bytes)", "op": ">", "threshold": 230000000, "for_minutes": 10}'
api POST /budgets -o /dev/null -w '   budget: HTTP %{http_code}\n' -d "{\"project\": \"$P\", \"limit_cents\": 3000, \"thresholds\": [50, 80, 100]}"

step "Done"
WEB=$(api GET "/projects/$P/services/web" | grep -o '"url":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "   storefront: $WEB"
echo "   project map: $URL/o/$(api GET /me | grep -o '"organization":"[^"]*"' | head -1 | cut -d'"' -f4)/p/$P#map"
