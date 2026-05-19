#!/usr/bin/env bash
# Free :9091 for the worker metrics server without killing Docker proxies.
set -euo pipefail

PORT="${1:-9091}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PIDFILE="$ROOT/.demo/worker.pid"

if [[ -f "$PIDFILE" ]]; then
  pid="$(cat "$PIDFILE")"
  if kill -0 "$pid" 2>/dev/null; then
    echo "Stopping previous demo worker (pid $pid)"
    kill "$pid" 2>/dev/null || true
    sleep 1
  fi
  rm -f "$PIDFILE"
fi

for pid in $(lsof -ti :"$PORT" 2>/dev/null || true); do
  cmd="$(ps -p "$pid" -o command= 2>/dev/null || true)"
  case "$cmd" in
    *docker*|*com.docker*|*Docker*|*vpnkit*|*com.docke*)
      echo "Leaving :$PORT pid=$pid (Docker proxy)"
      continue
      ;;
    *vidmerce*|*cmd/worker*|*"go run"*|*go-build*worker*|*/worker*)
      echo "Stopping :$PORT pid=$pid"
      kill "$pid" 2>/dev/null || true
      ;;
    *)
      # METRICS_WORKER_PORT is vidmerce-only in this repo; stale go-build bins land here.
      echo "Stopping :$PORT pid=$pid (stale listener)"
      kill "$pid" 2>/dev/null || true
      ;;
  esac
done
