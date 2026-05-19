#!/usr/bin/env bash
# Free :8080 for make demo without killing Docker port-forward proxies.
# Blind `kill $(lsof -ti :8080)` can take down com.docker.* and break :5432/:6379.

set -euo pipefail

PORT="${1:-8080}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PIDFILE="$ROOT/.demo/api.pid"

if [[ -f "$PIDFILE" ]]; then
  pid="$(cat "$PIDFILE")"
  if kill -0 "$pid" 2>/dev/null; then
    echo "Stopping previous demo API (pid $pid)"
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
    *vidmerce*|*cmd/api*|*"go run"*|*/api )
      echo "Stopping :$PORT pid=$pid"
      kill "$pid" 2>/dev/null || true
      ;;
    *)
      echo "Port :$PORT still in use by pid=$pid — stop it manually or set HTTP_PORT"
      echo "  $cmd"
      exit 1
      ;;
  esac
done
