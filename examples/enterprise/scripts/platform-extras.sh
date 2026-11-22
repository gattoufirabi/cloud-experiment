#!/usr/bin/env bash
# The parts of an environment the Terraform provider does not manage yet, set up through the
# REST API after "tofu apply": queue, connections (bucket, queue, email), functions with a
# bucket trigger and a webhook, the daily report job, metric alarms and a monthly budget.
# Safe to run again: what exists is kept.
#
#   STAYSA_ENDPOINT=https://console.example.com STAYSA_TOKEN=pla_... ./platform-extras.sh staging
#
# Reads the project and bucket names from "tofu output" in ../infra.
set -euo pipefail
PY=; for p in python3 python; do "$p" -c 1 >/dev/null 2>&1 && { PY=$p; break; }; done
[ -n "$PY" ] || { echo "python3 is needed" >&2; exit 1; }
ENVIRONMENT=${1:?usage: platform-extras.sh staging|production}
HERE=$(cd "$(dirname "$0")" && (pwd -W 2>/dev/null || pwd))
cd "$(dirname "$0")"
: "${STAYSA_ENDPOINT:?set STAYSA_ENDPOINT}" "${STAYSA_TOKEN:?set STAYSA_TOKEN (admin)}"
URL=${STAYSA_ENDPOINT%/}
TOFU=${TOFU:-tofu}
CURL=(curl -sS --max-time 120)
[ -n "${CURL_RESOLVE:-}" ] && CURL+=(--resolve "$CURL_RESOLVE") # development: console.localhost:8088:127.0.0.1

out() { (cd ../infra && "$TOFU" output -json "$ENVIRONMENT") | "$PY" -c "import json,sys; d=json.load(sys.stdin); print(d['$1'] if d else '')"; }
P=$(out project)
BUCKET=$(out photos)
[ -n "$P" ] || { echo "environment $ENVIRONMENT is not applied (tofu output is empty)" >&2; exit 1; }
QUEUE="$P-emails"

api() { local m=$1 path=$2; shift 2; "${CURL[@]}" -H "Authorization: Bearer $STAYSA_TOKEN" -H 'Content-Type: application/json' -X "$m" "$URL/api/v1$path" "$@"; }
# act <method> <path> <json>: prints the answer's message or error, never stops the script
act() { local out; out=$(api "$1" "$2" -d "$3" || true); printf '   %s %s: %s\n' "$1" "$2" "$(printf '%s' "$out" | sed -n 's/.*"\(message\|error\|status\)":"\([^"]*\)".*/\2/p' | head -1)"; }
code() { local m=$1 path=$2; shift 2; printf '   %s %s: HTTP %s\n' "$m" "$path" "$(api "$m" "$path" -o /dev/null -w '%{http_code}' "$@")"; }
pctl() {
  if command -v platformctl >/dev/null; then PLATFORM_URL=$URL PLATFORM_TOKEN=$STAYSA_TOKEN platformctl "$@"
  else (cd ../../.. && PLATFORM_URL=$URL PLATFORM_TOKEN=$STAYSA_TOKEN go run ./cmd/platformctl "$@"); fi
}
step() { printf '\n== %s\n' "$*"; }

step "$ENVIRONMENT: project $P"

step "Queue $QUEUE (order emails, retried, dead letters kept) and connections"
code POST /queues -d "{\"name\": \"$QUEUE\"}"
act POST "/queues/$QUEUE/attach" "{\"service\": \"$P/api\"}"
act POST "/queues/$QUEUE/attach" "{\"service\": \"$P/worker\"}"
act POST "/storage/buckets/$BUCKET/attach" "{\"service\": \"$P/api\"}"
act POST /email/attach "{\"service\": \"$P/worker\"}"
# The connections are secrets: deploy so the services get them.
for s in api worker; do code POST "/projects/$P/services/$s/deploy"; done

step "Functions: photo-uploaded (bucket trigger) and payment-webhook (signed webhook)"
pctl fn deploy "$P/photo-uploaded" --file "$HERE/../../store/functions/photo-uploaded.js" --runtime nodejs22
pctl fn deploy "$P/payment-webhook" --file "$HERE/../../store/functions/payment-webhook.js" --runtime nodejs22
act POST "/projects/$P/functions/photo-uploaded/triggers" "{\"bucket\": \"$BUCKET\", \"events\": \"created\", \"prefix\": \"products/\", \"suffix\": \".jpg\"}"
HOOKFILE=".payment-webhook-$ENVIRONMENT"
if [ ! -s "$HOOKFILE" ]; then
  api POST "/projects/$P/functions/payment-webhook/webhook" -d '{}' | sed -n 's/.*"url":"\([^"]*\)".*/\1/p' > "$HOOKFILE" || true
fi
[ -s "$HOOKFILE" ] && echo "   payment webhook URL (give it to the payment provider): $(cat "$HOOKFILE")"

if [ "$ENVIRONMENT" = production ]; then
  step "Daily sales report at 07:00 UTC (scheduled job in the api service)"
  code PUT "/projects/$P/services/api/jobs/daily-report" -d '{"schedule": "0 7 * * *", "command": "node src/report.js", "timeout_seconds": 120}'
fi

step "Alarms (notified on the organization's alert channels) and a monthly budget"
code POST /alarms -d "{\"name\": \"$P memory above 90%\", \"query\": \"max(platform_container_memory_bytes / platform_container_memory_limit_bytes)\", \"op\": \">\", \"threshold\": 0.9, \"for_minutes\": 10}"
code POST /alarms -d "{\"name\": \"$P CPU above 3 cores\", \"query\": \"sum(rate(platform_container_cpu_seconds_total[5m]))\", \"op\": \">\", \"threshold\": 3, \"for_minutes\": 15}"
LIMIT=$([ "$ENVIRONMENT" = production ] && echo 20000 || echo 3000)
code POST /budgets -d "{\"project\": \"$P\", \"limit_cents\": $LIMIT, \"thresholds\": [50, 80, 100], \"forecast_alert\": true}"

step "Done"
echo "   map: $URL/o/$(api GET /me | sed -n 's/.*"organization":"\([^"]*\)".*/\1/p')/p/$P#map"
