#!/usr/bin/env bash
# Build loadtest/RESULTS.md from k6 --summary-export JSON files.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RESULTS="$ROOT/loadtest/results"
OUT="$ROOT/loadtest/RESULTS.md"
BASE_URL="${BASE_URL:-http://localhost:8080}"

export ROOT
export BASE_URL
python3 <<'PY'
import json
import os
from datetime import datetime, timezone

root = os.environ["ROOT"]
results = os.path.join(root, "loadtest/results")
out = os.path.join(root, "loadtest/RESULTS.md")
base = os.environ.get("BASE_URL", "http://localhost:8080")
scenarios = ["bootstrap", "feed", "like", "view", "stats"]

def load(name):
    path = os.path.join(results, f"{name}-summary.json")
    if not os.path.isfile(path):
        return None
    with open(path) as f:
        return json.load(f)

def metric_row(data, key):
    m = (data or {}).get("metrics", {}).get(key, {})
    if not m:
        return "—"
    if "p(95)" in m:
        unit = "ms" if m["p(95)"] > 10 else "ms"
        return f"p95={m['p(95)']:.2f}{unit} avg={m.get('avg', 0):.2f}{unit}"
    if "value" in m and key.startswith("http_req_failed"):
        return f"{m['value'] * 100:.2f}%"
    if "count" in m:
        return str(m["count"])
    return "—"

lines = [
    "# k6 load test results",
    "",
    f"Generated: {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M:%S UTC')}",
    f"API base: `{base}`",
    "",
    "Raw logs and JSON summaries: [`loadtest/results/`](results/).",
    "",
    "Re-run:",
    "",
    "```bash",
    "make up && make migrate-all",
    "make run-api    # terminal 1",
    "make run-worker # terminal 2",
    "bash hack/run-loadtest.sh",
    "```",
    "",
    "## Summary",
    "",
    "| Scenario | http_reqs | http_req_failed | http_req_duration | checks |",
    "|----------|-----------|-----------------|-------------------|--------|",
]

for name in scenarios:
    data = load(name)
    if not data:
        lines.append(f"| {name} | *not run* | | | |")
        continue
    metrics = data.get("metrics", {})
    reqs = metrics.get("http_reqs", {}).get("count", "—")
    failed = metric_row(data, "http_req_failed")
    dur = metric_row(data, "http_req_duration")
    checks = metrics.get("checks", {})
    checks_s = f"{checks.get('passes', 0)}/{checks.get('fails', 0)} pass/fail" if checks else "—"
    lines.append(f"| {name} | {reqs} | {failed} | {dur} | {checks_s} |")

lines.extend([
    "",
    "## Per-scenario logs",
    "",
])
for name in scenarios:
    lines.append(f"- [`{name}.log`](results/{name}.log)")
    lines.append(f"- [`{name}-summary.json`](results/{name}-summary.json)")

with open(out, "w") as f:
    f.write("\n".join(lines) + "\n")
print("wrote", out)
PY
