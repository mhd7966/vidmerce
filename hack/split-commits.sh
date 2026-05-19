#!/usr/bin/env bash
# One-time helper: split the initial codebase into logical commits.
set -euo pipefail
cd "$(dirname "$0")/.."

export GIT_AUTHOR_NAME="Mahdieh Naeimi"
export GIT_AUTHOR_EMAIL="47852330+mhd7966@users.noreply.github.com"
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"

commit() {
  local msg="$1"
  shift
  git add "$@"
  git commit -m "$msg"
}

commit "chore: add project scaffold and local dev tooling

Add go.mod, Makefile, docker-compose, env template, and hack scripts
for migrations, demo port cleanup, and ClickHouse repair." \
  .gitignore .go-version .env.example go.mod go.sum Makefile docker-compose.yml hack/

commit "feat(platform): add config, logging, and datastore clients

Wire Postgres, Redis, ClickHouse, HTTP helpers, JWT, rate limiting,
metrics, cache, bloom filters, and health probes." \
  internal/platform/config/ internal/platform/logger/ internal/platform/db/ \
  internal/platform/redis/ internal/platform/clickhouse/ internal/platform/httpx/ \
  internal/platform/jwt/ internal/platform/ratelimit/ internal/platform/metrics/ \
  internal/platform/cache/ internal/platform/bloom/ internal/health/

commit "feat(migrate): add Postgres and ClickHouse schemas

OLTP tables for users, videos, products, likes, and video_stats;
ClickHouse view events with daily SummingMergeTree rollup." \
  migrations/

commit "feat(auth): add registration, login, and JWT refresh

Email/password auth with bcrypt, Redis refresh rotation, and rate-limited login." \
  internal/auth/

commit "feat(catalog): add video and product CRUD with caching

Create/read videos and one-product-per-video with Redis read-through cache." \
  internal/video/ internal/product/

commit "feat(feed): add pull and push feed modes

Keyset pagination from Postgres or Redis ZSET fan-out, toggled via FEED_MODE." \
  internal/feed/

commit "feat(likes): add async likes with exact Postgres counts

Redis hot path, stream worker, and periodic reconciler for drift detection." \
  internal/like/

commit "feat(views): add duration-aware view tracking pipeline

Filter chain, unique window, Redis streams, and ClickHouse batch worker." \
  internal/view/

commit "feat(stats): add cached analytics endpoint

Stampede-protected GET /videos/:id/stats joining ClickHouse views and PG likes." \
  internal/stats/

commit "feat(api): wire HTTP server, worker, and OpenAPI docs

Composition root, cmd/api, cmd/worker, embedded Swagger UI, and Grafana/Prometheus stack." \
  internal/platform/app/ internal/platform/swagger/ cmd/ deploy/

commit "test: add unit, integration, and k6 load tests

Race-safe unit tests, testcontainers e2e, and k6 scenarios for feed/like/view/stats." \
  tests/ loadtest/

commit "docs: add architecture, API reference, and README

Full documentation package, observability notes, and demo screenshots." \
  docs/ images/ README.md

echo "Done: $(git log --oneline origin/main..HEAD | wc -l | tr -d ' ') commits on top of origin/main"
