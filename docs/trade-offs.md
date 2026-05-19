# Trade-offs

Every decision here can be justified two ways. This document picks one and
calls out what the alternatives buy you and what they cost.

## 1. Gin vs Fiber (web framework)

**Chose Gin.**

| Aspect             | Gin                                | Fiber                                       |
|--------------------|------------------------------------|---------------------------------------------|
| Throughput         | High (uses net/http under the hood)| Higher (uses fasthttp)                       |
| Middleware ecosystem | Very large                       | Smaller; many net/http middlewares don't drop in |
| Standard library compat | Yes                           | No — fasthttp's Request type is incompatible with net/http |
| Idiom              | Closest to "Go standard"           | Diverges                                    |

I initially leaned Fiber for raw throughput. After looking at what we plan
to wire in (OpenTelemetry, JWT libs, testcontainers HTTP utilities, gRPC
gateway later) it became clear that fasthttp's incompatibility with
`net/http` would force us to either swap libraries we already chose or
write thin shims for each. The benchmark gap (~30% on a single endpoint, in
synthetic tests) doesn't translate to real workloads where TLS termination,
JSON marshaling, and database I/O dominate the latency budget.

## 2. Why three datastores (Postgres + Redis + ClickHouse)

**Chose all three.** Each is doing what it's best at:

- **Postgres** owns strong-consistency data (users, videos, products, like
  edges, exact like count). Reading "did this user like this video?" or
  "what's the title of video X?" needs a single point lookup that returns a
  fresh, durable answer. That's exactly Postgres's wheelhouse.
- **ClickHouse** owns the firehose of view events. A row-store would be
  forced to pick between paying for write-throughput (heavy disks, no
  indexes) or pre-aggregating in the application layer (custom code,
  guaranteed bugs). ClickHouse's SummingMergeTree + materialized view does
  the aggregation for us, on-disk, in microseconds per query.
- **Redis** is the in-memory tier — caches, streams (transport),
  leaky-bucket state (rate limit), and locks. Putting any of those into
  Postgres would either turn pg into the bottleneck or require us to
  reimplement Redis features (TTL eviction, pub/sub, atomic Lua) in SQL.

**The rejected alternative** is "Postgres for everything". It works at
small scale, but each of the three workloads grows on a different
exponent: views go up with content × audience, like edges go up with users
× videos, lookups go up with traffic. They will not all fit in one
Postgres instance for long, and splitting them later means a migration
under load. We pay the operational cost up front; in return we know
exactly where each piece scales.

## 3. Async likes with exact counts

**Chose async Redis-first + Postgres-eventual with exact counter.**

The two ends of the spectrum:

| Approach          | Latency       | Consistency           | Failure surface              |
|-------------------|---------------|------------------------|------------------------------|
| Sync to Postgres  | High (≥10ms)  | Strong                 | If PG goes down, likes break.|
| Pure Redis (no PG) | Sub-ms       | Eventual + no exact count | If Redis goes down, you lose state. |
| **Hybrid (chosen)** | Sub-ms      | Eventual everywhere except `video_stats.likes_count` which is exact in Postgres after worker apply. | Both PG and Redis must survive *together* for steady-state correctness; the reconciler heals the rest. |

The hybrid is more complex than either extreme. We accept that cost because:

- Like load is the most predictably spiky thing in a social app
  (celebrity-posts-something, the world claps simultaneously). Synchronous
  Postgres writes here cap concurrency at the connection pool size.
- The Redis side is a single Lua call so the *user-visible* response is
  always sub-ms; even if Postgres is having a bad minute, the like still
  registers.
- Exact counts in Postgres preserve the "trustworthy aggregate" property
  that creators, payouts, and downstream analytics rely on. Approximate
  counters (HyperLogLog, count-min sketches) are great for analytics-grade
  numbers; they're wrong for likes where a creator can manually check the
  list of liked-by users.
- The reconciler is a 60-line job that runs once an hour. The cost of
  having it is essentially zero; the cost of not having it is that any
  future bug in the worker silently corrupts the platform's flagship metric.

## 4. Async views to ClickHouse

**Chose XADD on accept; worker batches into ClickHouse.**

ClickHouse INSERT performance is approximately linear in *INSERT statement
count*, not in *row count*. A 10000-row INSERT is roughly the same cost as
a 1-row INSERT. So the only viable shape is **batched** ingestion.

We considered:

- **Sync write per request.** Catastrophic. 1 view = 1 INSERT = ClickHouse
  pinned at boot-time merge pressure.
- **In-process buffer + periodic flush.** Works for one replica; loses
  buffered events on a crash, and N replicas means N independent flushes
  with no coordination.
- **Kafka.** Best at very high scale, but introduces a 5th piece of
  infrastructure. Overkill for this exercise; documented as the upgrade
  path in `scalability.md`.

Redis Streams gives us at-least-once delivery, replayability, consumer
groups, and a single moving part — same Redis we already run.

## 5. Spam filter as a chain (not middleware)

**Chose `view.Chain` with ordered `view.Filter` interfaces.**

A middleware-per-rule approach would have worked but mixes HTTP concerns
with business concerns. The chain abstraction:

1. Lives entirely inside the `view` package — can be wired the same way in
   an internal RPC, a CLI replay tool, or a backfill job.
2. Short-circuits on first reject and returns a `<filter>:<reason>` tag
   that flows directly to logs, metrics, and the response body.
3. Is unit-testable without any HTTP machinery.

A future iteration where a filter is *expensive* (ML model on a sidecar)
moves it from the API edge to the worker side without changing its
interface. That portability is the design goal.

## 6. Feed: pull vs push

**Implemented both, togglable via `FEED_MODE`.**

| Mode | Pros                                            | Cons                                       |
|------|-------------------------------------------------|--------------------------------------------|
| Pull | Trivial to implement; correct out of the box; great for "global recency" feeds with under ~10M videos. | Linear cost per query in (page_size × index depth); concurrent reads contend on the same hot rows. |
| Push | O(1) per-feed read latency; trivial to personalise per user later. | Write amplification (every video create fans out to followers); cache invalidation; stale-on-deploy footgun. |

In practice, you almost always end up with push for "for-you" feeds and
pull for "latest in category" pages. We implemented both *now* so the
team can run the side-by-side load test and pick the right one when the
follow graph data is available.

## 7. Cursor-based pagination (not offset)

**Chose keyset.** `OFFSET N` in Postgres is O(N) — bad for pages 100+ of
any popular feed. Keyset uses the index directly: ordering by
`(created_at, id) DESC` and returning a cursor that is the last seen
`(created_at, id)` lets the next request resume in O(log N). Same shape
regardless of how many pages have already been seen.

The cursor is `base64(JSON{c: created_at, i: id})`. JSON inside base64
because:

- Self-describing: a developer reading the URL can decode it in their head.
- Forward-compatible: adding fields (e.g. `personalised_seed`) doesn't
  break old clients.
- Cheap: serialising two scalars is fewer bytes than the worst-case URL
  length.

## 8. JWT (HS256) access + opaque refresh

**Chose stateless access + stateful refresh.**

- **Access tokens** are HS256 JWTs with a short TTL (15m default). They are
  *not* stored server-side, which means token revocation requires waiting
  out the TTL. That's an explicit trade-off: it bounds revocation latency
  in exchange for not paying a Redis hit on every authenticated request.
- **Refresh tokens** are opaque UUIDs stored in Redis under
  `refresh:{token_id}` with TTL = `JWT_REFRESH_TTL`. Logging out is a
  single `DEL`. Refresh rotation issues a new pair and `DEL`s the old one
  — the access token's short TTL bounds the window where a stolen refresh
  token can still be exploited.

RS256 was considered. Worth it when token verifiers and signers are run by
*different* teams (e.g. an auth service + N consuming services); not worth
the operational cost (key rotation, JWKS, asymmetric algorithm in every
runtime) at this scale.

## 9. Bcrypt cost = 12

Cost 10 is the most common default; cost 14 is the OWASP recommendation as
of recent guidance. We picked 12 — the midpoint — so login latency stays
under 200ms on modest hardware while still putting offline brute-force
attacks well into "infeasible" territory. Configurable via `BCRYPT_COST`.

## 10. Leaky bucket > token bucket for spam control

**Chose leaky bucket.**

Token bucket allows full-burst at the moment the bucket refills — exactly
the pattern bots use ("hammer until you get through, then back off and
hammer again"). Leaky bucket smooths the burst: the same `capacity` /
`leak_rate` parameters cap *steady-state* rate without ever permitting
a sudden burst.

Single implementation, three call sites:

- Login (per IP, fail-closed): brute force defense.
- View (per subject × video, fail-open): spam suppression. Policy is derived
  from cached `duration_sec` (~`60/duration` views/min); videos under 5s skip
  the bucket.
- Like (per user × video, fail-open): toggle-spam defense.

One Lua script in `internal/platform/ratelimit/leakybucket.go`; login and
like use fixed `Policy` values; view uses `view.RatePolicy(duration)`.

## 11. Stampede protection in three layers

**Cache + singleflight + distributed lock.** Each layer addresses a
different failure mode:

| Layer                   | Stops…                                                       |
|-------------------------|---------------------------------------------------------------|
| Redis cache + TTL       | The 99% case where the same video is requested over and over. |
| In-process singleflight | A burst of N concurrent requests **within one replica**.      |
| Distributed Redis lock  | A burst of N concurrent requests **across replicas**.         |

Skipping any layer leaves a failure mode unaddressed. Doing just the lock
(no singleflight) means a thundering herd inside one process still
hammers Redis for the lock attempt itself. Doing just the cache means a
cold cache can compute N times across replicas. Doing just singleflight
fails the moment you scale past one API replica.

A simpler design — "fetch under a mutex, period" — was rejected because
its blast radius is unbounded: if a holder dies, every other request waits
for the TTL.

## 12. Worker partitioning (current and planned)

**Now: one consumer per stream.** Redis Streams + a consumer group with one
consumer preserves per-stream order, which is what we need for likes (so
a `like → unlike` from the same user, in that order, doesn't end up
applied as `unlike → like`).

**Planned: partition by `hash(user_id)`** when single-consumer throughput
becomes the bottleneck. Documented in `scalability.md`. Won't change the
filter / repository interfaces.

## 13. Testing strategy

**Layered, like the code.**

| Layer        | Tool             | What it covers                                 |
|--------------|------------------|------------------------------------------------|
| Unit         | `go test`        | Business logic with in-memory fakes.           |
| Integration  | testcontainers   | Real Postgres/Redis/ClickHouse, full pipelines.|
| Load         | k6               | Profile under representative concurrency.      |

Each layer catches a *different* class of bug:

- Unit catches logic errors (validation, error wrapping, state machine
  transitions).
- Integration catches schema-vs-code drift, atomicity violations, and
  *worker timing* (PEL behaviour, ACK ordering).
- Load catches the dragons that only appear under contention: leaky bucket
  vs concurrency, stampede vs cache warmth, batch flush vs throughput.

Rejecting any one layer leaves a class of bugs that the others can't
practically reach.

## 11. Postgres UNIQUE + Redis Bloom for uniqueness

**Chose both.** Postgres `UNIQUE` on `users.email` and `products.video_id`
remains authoritative — it catches races and Bloom false negatives. Redis
Bloom (`bloom:emails`, `bloom:product_videos`) is a shared, probabilistic
front filter: `BF.EXISTS` lets us return `409` before bcrypt (register) or
before `INSERT` (product) when the member is *probably* already present.

**Trade-off:** a configurable false-positive rate (~1% by default) means
some legitimate creates get rejected without touching Postgres; clients can
retry or we could add an optional DB confirm path. **Benefit:** duplicate
registration storms no longer burn CPU on bcrypt or unique-index probes.

We rejected "app-only pre-check (`SELECT` before `INSERT`)" — it doesn't
serialize concurrent requests. We rejected "Bloom only" — false negatives
would allow duplicates unless the DB still enforces uniqueness.

## 12. Total views vs unique views (Reels-style counting)

**Chose separate metrics with different rules.**

| Metric | Rule |
|--------|------|
| **Total views** | Every accepted beacon after watch ≥⅓ duration and under the physics rate cap (`60/duration_sec` per minute). Replays count. |
| **Unique views** | First view per `(viewer, video)` in `VIEW_UNIQUE_TTL`; replays write `is_unique=0` but still increment totals. |

We rejected **dedup-at-edge that drops replays** — that made total and unique
identical and blocked legitimate re-watches. We rejected **Postgres on every
`/view`** for duration — `video:{id}:dur` in Redis is set once at create.

**Trade-off:** client `watch_ms` is forgeable; the rate cap bounds impossible
view rates per video length. **Benefit:** a 10s reel can show 6 total views
and 1 unique view in the same minute, matching product expectations.
