# Load tests

[k6](https://k6.io) scripts that exercise the hot endpoints under
representative traffic profiles. They are deliberately self-contained — no
extra runtime, no metrics scraper required. Pipe the output into Grafana /
InfluxDB / Prometheus if you want longer-lived dashboards.

## Layout

| File             | Scenario                                         | Endpoint(s)                              |
|------------------|--------------------------------------------------|------------------------------------------|
| `lib.js`         | Shared helpers (auth, request wrappers)          | —                                        |
| `bootstrap.js`   | Seeds the feed with N videos (run once)          | `POST /auth/*`, `POST /videos`           |
| `feed.js`        | Ramp-up read load, pull vs push toggle           | `GET /feed`                              |
| `like.js`        | Like/unlike burst with rate-limit boundary       | `POST /videos/:id/like`, `/unlike`       |
| `view.js`        | Mixed traffic: legit / no-watch / spammer        | `POST /videos/:id/view`                  |
| `stats.js`       | 200 VUs on one video — stampede protection       | `GET /videos/:id/stats`                  |

## Running

Run the stack first:

```bash
make up
make migrate-all
make run-api &
make run-worker &
```

Then:

```bash
# One-time seed
k6 run loadtest/bootstrap.js

# Scenarios
make load-feed   # GET /feed
make load-like   # POST /videos/:id/like
make load-view   # POST /videos/:id/view
make load-stats  # GET /videos/:id/stats stampede

# Compare feed modes
FEED_MODE=pull make run-api & sleep 2 && k6 run loadtest/feed.js
FEED_MODE=push make run-api & sleep 2 && k6 run loadtest/feed.js
```

## What to look for

- **`feed.js`**: p95 should drop noticeably in push mode (Redis ZSET hits)
  vs pull mode (Postgres keyset scan).
- **`like.js`**: error rate climbs as VUs concentrate on the same videos —
  expected; the leaky bucket is doing its job. Aggregate throughput should
  stay flat as VUs scale up.
- **`view.js`**: `view_accepted` and `view_rejected` custom counters report
  the filter chain outcome ratio. With the default mix (60/25/15) and
  default thresholds, expect roughly `accepted ≈ 60%`; replays count as
  accepted with `is_unique=false` when within `VIEW_UNIQUE_TTL`.
- **`stats.js`**: with 200 VUs hammering one video, p95 should sit near
  cache-hit latency (one Redis GET). Observe `system.query_log` on the
  ClickHouse side — you should see ≈ `(test_duration / STATS_CACHE_TTL)`
  queries, not 200 × `requests_per_VU`.
