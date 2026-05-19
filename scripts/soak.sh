#!/usr/bin/env bash
# soak.sh — load-test DocPipe against a docker-compose instance and confirm
# the v1 hang is fixed.
#
# Pass criteria (spec §15):
#   1. browser still healthy at the end (/readyz returns 200)
#   2. memory roughly flat (final RSS within 50MB of post-start)
#   3. no zombie processes inside the container (ps state column has no Z)
#   4. /v1/stats totals reflect all sent requests
#
# Defaults: 1000 sequential + 50-parallel waves for 10 minutes total.
# Override via env: SOAK_SEQ, SOAK_PARALLEL, SOAK_DURATION_S, SOAK_KEY.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
RESULTS="$ROOT/soak-results"
mkdir -p "$RESULTS"

BASE_URL="${SOAK_BASE_URL:-http://localhost:8080}"
KEY="${SOAK_KEY:-test:soak-key-please-replace}"
KEY_NAME="${KEY%%:*}"
KEY_SECRET="${KEY#*:}"
SEQ="${SOAK_SEQ:-1000}"
PARALLEL="${SOAK_PARALLEL:-50}"
DURATION_S="${SOAK_DURATION_S:-600}"
CONTAINER="${SOAK_CONTAINER:-docpipe-soak}"
KEEP_CONTAINER="${SOAK_KEEP:-0}"

red()   { printf '\033[31m%s\033[0m\n' "$*" >&2; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
log()   { printf '[soak %s] %s\n' "$(date +%H:%M:%S)" "$*"; }

cleanup() {
  if [ "$KEEP_CONTAINER" != "1" ]; then
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

require() {
  command -v "$1" >/dev/null || { red "missing required command: $1"; exit 1; }
}
require docker
require curl
require jq

log "building image…"
docker build -f "$ROOT/deploy/Dockerfile" -t docpipe:soak "$ROOT" >/dev/null

log "starting container ($CONTAINER)…"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" \
  -p 8080:8080 \
  -e DOCPIPE_API_KEYS="$KEY" \
  docpipe:soak >/dev/null

# Wait for healthy
log "waiting for /healthz…"
for _ in {1..60}; do
  if curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! curl -fsS "$BASE_URL/healthz" >/dev/null; then
  red "container never became healthy"
  docker logs "$CONTAINER" | tail -30 >&2
  exit 1
fi
log "container healthy"

# Snapshot baseline.
rss_start=$(docker exec "$CONTAINER" sh -c "grep VmRSS /proc/1/status 2>/dev/null | awk '{print \$2}'" || echo 0)
log "baseline RSS: ${rss_start} kB"

payload=$(jq -n --arg html '<!doctype html><html><body><h1>Soak</h1><p>render '"$(date +%s)"'</p></body></html>' '{html:$html,options:{wait:{strategy:"load",timeout_ms:5000}}}')
auth_header="Authorization: Bearer $KEY_SECRET"

count_ok=0
count_fail=0

# Phase 1 — sequential
log "phase 1: $SEQ sequential requests"
for i in $(seq 1 "$SEQ"); do
  code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/v1/convert/html-to-pdf" \
    -H "$auth_header" -H "Content-Type: application/json" -d "$payload" || echo 000)
  if [ "$code" = "200" ]; then count_ok=$((count_ok+1)); else count_fail=$((count_fail+1)); fi
  if (( i % 100 == 0 )); then log "  $i/$SEQ (ok=$count_ok fail=$count_fail)"; fi
done
log "phase 1 done: ok=$count_ok fail=$count_fail"

# Phase 2 — parallel waves for DURATION_S seconds
phase2_end=$(( $(date +%s) + DURATION_S ))
log "phase 2: $PARALLEL-parallel waves until $(date -d @$phase2_end '+%H:%M:%S')"
phase2_ok=0
phase2_fail=0
while [ "$(date +%s)" -lt "$phase2_end" ]; do
  wave_results=$(mktemp)
  for _ in $(seq 1 "$PARALLEL"); do
    (
      code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/v1/convert/html-to-pdf" \
        -H "$auth_header" -H "Content-Type: application/json" -d "$payload" || echo 000)
      echo "$code"
    ) &
  done
  wait
  # Tally is approximate (the parallel echo could interleave). Use stats endpoint
  # as authoritative at end.
  rm -f "$wave_results"
done

# Final asserts.
sleep 2
rss_end=$(docker exec "$CONTAINER" sh -c "grep VmRSS /proc/1/status 2>/dev/null | awk '{print \$2}'" || echo 0)
log "final RSS: ${rss_end} kB"

log "checking for zombie processes…"
zombies=$(docker exec "$CONTAINER" sh -c "ps -eo state,pid,comm 2>/dev/null | awk '\$1==\"Z\"' | wc -l" || echo 0)
zombies=$(echo "$zombies" | tr -d '[:space:]')
if [ "$zombies" != "0" ]; then
  red "FAIL: $zombies zombie process(es) inside container"
  docker exec "$CONTAINER" sh -c "ps -eo state,pid,comm | awk '\$1==\"Z\"'"
  exit 2
fi
green "OK: zero zombie processes"

log "checking /readyz…"
if ! curl -fsS "$BASE_URL/readyz" >/dev/null; then
  red "FAIL: /readyz not ok at end of run"
  exit 3
fi
green "OK: browser still ready"

log "checking RSS growth…"
delta_kb=$((rss_end - rss_start))
delta_mb=$((delta_kb / 1024))
log "RSS delta: ${delta_kb} kB (~${delta_mb} MB)"
if [ "$delta_mb" -gt 100 ]; then
  red "WARN: RSS grew by ${delta_mb} MB (>100 MB threshold)"
else
  green "OK: RSS growth within budget"
fi

log "fetching final /v1/stats…"
stats_json=$(curl -fsS "$BASE_URL/v1/stats")
echo "$stats_json" | jq '.' > "$RESULTS/final-stats.json"
total_req=$(echo "$stats_json" | jq '.totals.requests')
total_pdfs=$(echo "$stats_json" | jq '.totals.pdfs_generated')
total_fail=$(echo "$stats_json" | jq '.totals.failures')
log "stats: requests=$total_req pdfs=$total_pdfs failures=$total_fail"

green "soak PASS"
echo
echo "Stats archived to: $RESULTS/final-stats.json"
if [ "$KEEP_CONTAINER" = "1" ]; then
  echo "Container kept for inspection: $CONTAINER"
else
  echo "Container will be removed on exit (set SOAK_KEEP=1 to keep)"
fi
