#!/usr/bin/env bash
# Checks a deployed environment from the outside, as a shopper would. Exit code 0 = healthy.
#   ./smoke-test.sh https://staging.example.com
set -uo pipefail
PY=; for p in python3 python; do "$p" -c 1 >/dev/null 2>&1 && { PY=$p; break; }; done
[ -n "$PY" ] || { echo "python3 is needed" >&2; exit 1; }
BASE=${1:?usage: smoke-test.sh <storefront URL>}
BASE=${BASE%/}
CURL=(curl -sS --max-time 15)
[ -n "${CURL_RESOLVE:-}" ] && CURL+=(--resolve "$CURL_RESOLVE")
fail=0
check() { if [ "$2" = "$3" ]; then echo "  ok    $1"; else echo "  FAIL  $1 (expected $3, got $2)"; fail=1; fi; }

check "storefront answers" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/")" 200
check "catalogue served through the private API" "$("${CURL[@]}" "$BASE/api/products" | "$PY" -c 'import json,sys; print(len(json.load(sys.stdin)) > 0)' 2>/dev/null)" True
check "internal API routes are not exposed" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' -X POST "$BASE/api/internal/paid")" 404
check "metrics are not public" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/metrics")" 404
# Latency of 20 catalogue reads (served from the Redis cache after the first)
t=$(for _ in $(seq 1 20); do "${CURL[@]}" -o /dev/null -w '%{time_total}\n' "$BASE/api/products"; done | sort -n | awk '{a[NR]=$1} END {print a[int(NR*0.95)]}')
echo "  info  p95 catalogue latency: ${t}s"
exit $fail
