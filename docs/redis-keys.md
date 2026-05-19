# Redis Key Catalog

Redis is the single in-memory tier in Vidmerce. Centralising the key catalog
here keeps the namespace orderly, lets us reason about TTLs and memory cost,
and makes it easy to add a new key without colliding with an existing one.

Conventions:

- Lower-case, colon-separated namespaces: `area:subject:id[:variant]`.
- Templated parts use `{placeholder}`; everything else is literal.
- TTLs are documented per key — Redis is **not** durable storage for any of
  these. Postgres or ClickHouse is the source of truth in every case.

## Caches

| Key                                | Type   | TTL  | Purpose |
|---|---|---|---|
| `video:{id}`                       | string | 60s  | Cached JSON of `GET /videos/:id`. Invalidated on update. |
| `product:{id}`                     | string | 60s  | Cached JSON of `GET /products/:id`. |
| `video:{id}:product`               | string | 60s  | Cached JSON of `GET /videos/:id/product`. |
| `stats:{video_id}`                 | string | 30s  | Cached analytics payload for `GET /videos/:id/stats`. Stampede-protected. |

## Counters & sets (likes)

| Key                                | Type   | TTL  | Purpose |
|---|---|---|---|
| `video:{id}:likes`                 | int    | none | Hot like counter. Eventual consistency vs Postgres; refreshed on cache miss. |
| `user:{id}:liked:videos`           | set    | none | Set of video IDs the user has liked. Used for "did I like this?" on feed reads. |

## Counters & probabilistic sets (views)

| Key                                | Type   | TTL  | Purpose |
|---|---|---|---|
| `video:{id}:dur`                   | string | `VIEW_DURATION_CACHE_TTL` (default 168h) | Video length in seconds for view rules. Written at `POST /videos` create; read on `POST /view` (no Postgres on hot path). |
| `view:unique:{subject}:{video_id}` | string | `VIEW_UNIQUE_TTL` (default 10m) | First view in window sets `is_unique=1` in ClickHouse; replays still count as total views with `is_unique=0`. |

## Rate-limiting / leaky-bucket state

| Key                                | Type   | TTL  | Purpose |
|---|---|---|---|
| `bucket:login:{ip}`                | hash   | 1h   | Leaky-bucket state (level + last_leak) for login attempts per IP. |
| `bucket:view:{subject}:{video_id}` | hash   | 1h   | Duration-aware leaky bucket per (subject, video). Capacity/leak ≈ `60/duration_sec` per minute (skipped when duration &lt; `VIEW_MIN_DURATION_SEC`). Same `subject` as `view:unique:*`. |
| `bucket:like:{user_id}:{video_id}` | hash   | 1h   | Leaky-bucket state for like/unlike toggles per (user, video). |

## Streams (async pipelines)

| Key                                | Type   | Retention | Purpose |
|---|---|---|---|
| `stream:likes`                     | stream | XADD MAXLEN ~100k | Like / unlike events to be persisted to Postgres by the worker. |
| `stream:views`                     | stream | XADD MAXLEN ~1M   | View events to be filtered (spam pipeline) and inserted into ClickHouse. |
| `stream:videos.created`            | stream | XADD MAXLEN ~10k  | Video-creation fan-out events, used by the push-feed worker to update ZSETs. |

Consumer groups for each stream are named `vidmerce-workers` (configurable
via `WORKER_CONSUMER_GROUP`). Consumer names are `worker-{N}` per pod.

## Uniqueness (Bloom filters — RedisBloom)

Requires **Redis Stack** (`BF.*` commands). Postgres `UNIQUE` constraints remain
the source of truth; blooms only reject obvious duplicates early.

| Key                     | Type  | TTL  | Purpose |
|---|---|---|---|
| `bloom:emails`          | bloom | none | Probabilistic set of normalised user emails. `BF.EXISTS` before bcrypt on register; `BF.ADD` after successful insert or `23505`. Warmed from `users.email` at API startup. |
| `bloom:product_videos`  | bloom | none | Probabilistic set of `video_id` values that already have a product. Checked before `INSERT` on `POST /products`; warmed from `products.video_id`. |

Configured via `BLOOM_*` env vars (`BLOOM_ERROR_RATE`, capacities). A tiny
false-positive rate means an occasional `409` without hitting Postgres; false
negatives are impossible — races still hit `UNIQUE` and then `BF.ADD`.

## Auth

| Key                                | Type   | TTL                  | Purpose |
|---|---|---|---|
| `refresh:{token_id}`               | string | `JWT_REFRESH_TTL`    | Issued refresh tokens. Deleted on `POST /auth/logout` to revoke. |
| `refresh:user:{user_id}`           | set    | aligned with above   | Per-user index of active refresh tokens (so "log out all sessions" can iterate). |

## Push feed

| Key                                | Type        | TTL  | Cap | Purpose |
|---|---|---|---|---|
| `feed:global`                      | sorted set  | none | `FEED_PUSH_ZSET_CAP` (1000) | Global recent-videos cache for `FEED_MODE=push`. Score = `created_at_unix`. |
| `feed:user:{id}` *(future)*        | sorted set  | 7d   | 1000 | Personalised fan-out for users with a follow list. Documented; not yet implemented. |

## Distributed locks

| Key                                | Type   | TTL  | Purpose |
|---|---|---|---|
| `lock:stats:{video_id}`            | string | 5s   | Stampede-protection for `GET /videos/:id/stats` recomputation. |
| `lock:reconcile:likes`             | string | 5m   | Singleton lock for the periodic like-counter reconciler. |

## Notes on TTL choices

- **Counters (`*:likes`, `*:views`)** have no TTL because they are read on
  every feed and stats request; expiring them would create spiky DB load.
  They are reconciled against Postgres/ClickHouse periodically.
- **Caches (`video:*`, `product:*`, `stats:*`)** have short TTLs because the
  underlying data can change frequently and we prefer eventually-correct over
  permanently-stale.
- **Unique keys (`view:unique:*`)** TTL == `VIEW_UNIQUE_TTL` (unique-view window).
- **Duration keys (`video:*:dur`)** TTL == `VIEW_DURATION_CACHE_TTL`; hot path never reads Postgres.
- **Buckets** carry a TTL larger than the practical idle time so memory is
  reclaimed for inactive (viewer, video) pairs but active ones stay warm.
