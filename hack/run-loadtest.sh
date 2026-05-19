#!/usr/bin/env bash
# Run k6 load scenarios against a local API and write summaries under loadtest/results/.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
RESULTS="$ROOT/loadtest/results"
BASE_URL="${BASE_URL:-http://localhost:8080}"
mkdir -p "$RESULTS"

log() { echo "[loadtest] $*"; }

if ! command -v k6 >/dev/null; then
  echo "k6 not found — install with: brew install k6" >&2
  exit 1
fi

wait_ready() {
  for i in $(seq 1 90); do
    if curl -sf "$BASE_URL/ready" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "API not ready at $BASE_URL" >&2
  exit 1
}

run_k6() {
  local name="$1"
  shift
  log "running $name..."
  set +e
  k6 run "$@" \
    --summary-export="$RESULTS/${name}-summary.json" \
    2>&1 | tee "$RESULTS/${name}.log"
  local rc=${PIPESTATUS[0]}
  set -e
  if [[ "$rc" -ne 0 ]]; then
    log "$name finished with exit $rc (thresholds or script error — continuing)"
  fi
}

log "waiting for API at $BASE_URL..."
wait_ready

run_k6 bootstrap loadtest/bootstrap.js
run_k6 feed loadtest/feed.js
run_k6 like loadtest/like.js
run_k6 view loadtest/view.js
run_k6 stats loadtest/stats.js

log "writing loadtest/RESULTS.md"
ROOT="$ROOT" BASE_URL="$BASE_URL" "$ROOT/hack/write-loadtest-results.sh"

log "done — see loadtest/RESULTS.md and loadtest/results/"
