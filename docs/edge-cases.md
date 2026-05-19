# Edge cases

A catalogue of the non-happy-path conditions we explicitly handle and what
happens in each. Organised by feature.

## Auth

### 1. User registers an email that's already taken

`pgUserRepo.Create` translates the Postgres unique-violation
(`SQLSTATE 23505`) into `ErrEmailTaken`. The handler maps to `409 Conflict`
with `code: "conflict"`. **Crucially we do *not* leak whether the email
exists during *login*** — only during register.

### 2. Login with non-existent user

`Login` looks up the user. On `ErrUserNotFound` it **still runs a dummy
bcrypt compare** against a fake hash before returning `ErrInvalidCredentials`.
This equalises the response-time between "user doesn't exist" and "password
is wrong", defeating user-enumeration via timing.

### 3. Stale / replayed refresh token

Refresh tokens are stored in Redis with TTL. On `POST /auth/refresh`:

- Token not in Redis → `ErrInvalidRefresh` → 401.
- Token in Redis → rotate (issue new pair, `DEL` old token).

If a stolen refresh token is used before its rightful owner notices,
rotation defeats it: whichever party uses the token second gets a 401, and
the other can detect the breach (logging shows the unexpected access from a
new device).

### 4. JWT clock skew across replicas

Access tokens carry `iat` and `exp` in UTC. We do not add custom skew
tolerance — the upstream `golang-jwt/jwt` library performs a small built-in
tolerance check. In practice deploying NTP everywhere fixes this; for
cross-region deployments where clock drift is a real concern the right fix
is shorter access TTLs, not bigger skew windows.

## Videos & products

### 5. Creating a product for someone else's video

`product.Service.Create` calls `video.Service.AssertOwner` first. The
caller's user ID is compared to `videos.user_id`; mismatch → `ErrForbidden`
→ 403. Video missing → `ErrVideoNotFound` → 404. Test coverage in
`internal/product/service_test.go`.

### 6. Creating two products for one video

`products.video_id` has a `UNIQUE` constraint. The repository translates
the unique-violation into `ErrVideoAlreadyTaken` → 409. We don't allow
multiple products per video because the spec says
`GET /videos/:id/product` returns *the* product.

### 7. Cache stampede on a hot video

See [`stats.md`](architecture.md#stats-three-layer-stampede-defense)
section. For `GET /videos/:id` and `/products/:id` the simpler cache-aside
pattern is sufficient because cache reads are 1 round-trip and cache TTL
is short (60s). For `GET /stats` we add singleflight + a distributed lock.

## Feed

### 8. Tampered cursor

The cursor is unauthenticated base64-JSON — clients can edit it. The
decoder validates: bad base64 → 400, bad JSON → 400, bad uuid in `id` → 400.
A *valid* cursor that points to an arbitrary timestamp is just a query
parameter; no privilege escalation possible because the feed is public.

### 9. Cursor for a deleted video

The cursor is `(created_at, id)`. If the video at `id` has been deleted,
the query still works — keyset pagination compares tuples, not foreign
keys. The deleted video simply doesn't appear in the result. The next
cursor advances normally.

### 10. Push mode: ZSET drift across redeploys

`PushFetcher.Warmup` is called on API startup: it reads recent videos from
Postgres and populates `feed:global`. So a cold Redis (or a wiped cache)
self-heals on next boot. If a video is added to Postgres while the API is
down, it lands in the ZSET on the next `POST /videos` via the
`WithOnCreate` hook.

### 11. Push mode: feed update during deploy

We do not see-saw between modes at runtime; mode is a startup env var.
During a rolling deploy that *changes* the mode, both implementations are
running simultaneously briefly. Both are read-only on the same Postgres
table; the push side will see slightly stale ZSET data until it catches
up. Acceptable for a feed.

## Likes

### 12. Same user likes the same video twice

The Lua script checks `SISMEMBER user:{uid}:liked:videos vid` first. Already
in the set → returns `(liked=1, count=current, status=noop)` without
incrementing the counter or XADDing. The endpoint returns 202 either way so
the client behaviour is identical.

### 13. User unlikes a video they never liked

Symmetric: SISMEMBER returns 0, the script returns
`(liked=0, count=current, status=noop)`. No DECR. No XADD. Total round-trip
~1ms.

### 14. Like / unlike race within milliseconds

The Lua script is atomic. Two requests from the same user on the same
video — one like and one unlike — execute in *some* serial order. The
final state reflects whichever was second on the Redis side. The stream
preserves that order (`monotonic ID`); the worker applies in order; the
final Postgres state matches Redis.

### 15. Worker crash mid-batch

Each event is XACK'd only after the apply succeeds. A crash mid-event
leaves it in the PEL. On restart, `XREADGROUP > GROUP …` re-delivers
pending entries to whichever consumer reclaims them. Because the SQL is
idempotent (`ON CONFLICT DO NOTHING`), re-applying is a no-op.

### 16. Worker can't reach Postgres for an extended time

Each failed apply does *not* XACK; the message stays pending. The worker
backs off (1s sleep on the read loop) so it isn't busy-looping. When
Postgres comes back, the worker drains the backlog. There's no
data-loss path here as long as Redis itself stays up.

### 17. Counter drift in Postgres (impossible, but…)

The reconciler runs hourly: it samples `video_stats` rows ordered by
`updated_at`, recomputes `COUNT(*)` from `likes`, and rewrites the counter
if they don't match. Drift detection logs at ERROR — under correct code it
should never log; if it does we have a real bug.

### 18. Like operations during Redis outage

The Lua script call fails → the handler returns 500. The leaky-bucket
middleware is fail-open, so the rate limiter doesn't make things worse,
but the like itself doesn't register. This is consistent with how
production systems behave: Redis is a hard dependency for the like hot
path, by design.

## Views

### 19. Same viewer watches the same video twice (replay)

Both beacons can be **accepted** if each passes the watch threshold and
duration-based rate cap. The first in `VIEW_UNIQUE_TTL` sets
`is_unique=1` on the stream row; the second sets `is_unique=0`. ClickHouse
`views` increments twice; `unique_views` increments once. The HTTP body
includes `accepted` and `is_unique` (still always **202**).

### 20. Six full replays of a 10s reel in one minute

With `duration_sec=10`, the rate cap is **6/min** (`60/10`). Six honest
watches with `watch_ms ≥ 3334` fit the physics cap. A seventh in the same
minute gets `rejected_by=duration_rate:rate_limited` unless enough time
has leaked from the bucket.

### 21. Logged-in user views their own video repeatedly

Same rules as any viewer — we don't special-case creator == viewer.
Creator replays still count toward **total** views; **unique** views are
still capped by the unique window.

### 22. Anonymous viewer behind shared NAT

The subject key is `a:{ip_hash}:{ua_hash}`. Two people behind the same NAT
with the same browser share unique-view and rate-limit buckets. Mitigation:
log in (`u:{user_id}`). This is an accepted trade-off for anonymous traffic.

### 23. `watch_ms` is client-supplied — can it be forged?

Yes. `WatchThresholdFilter` requires `watch_ms ≥ duration_sec/3` when
duration is known (from Redis). Bots with `watch_ms=0` are rejected;
sophisticated bots can lie. We combine that with the duration rate cap and
optional future filters. Trust in any single client field is zero; trust in
the combination is non-zero.

### 24. Video duration missing from Redis (`video:{id}:dur`)

Duration is written at **`POST /videos` create**. If the key is missing
(cache eviction, pre-migration video), the service uses
`VIEW_UNKNOWN_MIN_WATCH_MS` (default 1000ms) and **skips** the
duration-based rate bucket (fail-open). No Postgres fallback on the hot
path — backfill duration keys offline if needed.

### 25. ClickHouse batch insert fails

The worker keeps the batch in memory and re-tries on the next loop
iteration. If ClickHouse stays down, the stream backs up. When the stream
hits its `MAXLEN ~ 5_000_000` cap, Redis starts evicting the oldest
entries. That's data loss — but losing the oldest 5M views during a long
outage is dramatically better than crashing the API. The right alert is
"stream:views length > N" with N tuned to fire well before MAXLEN.

### 26. Filter chain has an infrastructure failure

`DurationRateFilter` is fail-open — if Redis Lua fails, views are allowed
(brief over-counting beats dropping legitimate engagement). The unique
marker failing counts the view as non-unique but still accepts it. Login's
leaky bucket is fail-closed: brute-force protection is *more* important
than availability of the login endpoint.

## Stats

### 27. Stats for a brand-new video (no views, no likes)

The ClickHouse `sum()` over zero rows returns 0 (not NULL). The Postgres
`video_stats` row is created by the AFTER INSERT trigger on `videos`, so
`likes_count` is 0 too. `engagement_rate` is 0 via the divide-by-zero
guard. The response is `{ views: 0, unique_views: 0, likes: 0,
engagement_rate: 0 }` — clean, no nulls.

### 28. Stats for a non-existent video

`Service.Get` calls `video.Service.Exists` (cached) before computing. False
→ `ErrVideoNotFound` → 404. Backends are not contacted.

### 29. Distributed lock holder dies before unlocking

The lock has `STATS_LOCK_TTL` (default 5s). After it expires, the key is
gone and the next request acquires the lock cleanly. The
`STATS_LOCK_RETRY` (75ms) wait means lock-losers fall through to a private
compute well before the lock TTL elapses — bounded latency under failure.

### 30. Lock holder takes longer than `STATS_LOCK_TTL`

The lock auto-expires. Another replica acquires the lock and starts a
parallel compute. When the original holder finishes and calls
`releaseLock`, the Lua `compare-and-delete` prevents it from deleting the
successor's lock (it compares the per-call UUID token). The cache simply
gets written twice — last writer wins, content is the same.

### 31. ClickHouse query times out under load

The error propagates through `errgroup`; the parallel Postgres query is
cancelled via the shared context. `compute` returns the wrapped error;
`Service.Get` returns it to the handler, which maps to 500. We don't
serve a half-computed value because a stats response showing 0 views
when the truth is non-zero is *worse* than a 5xx.

## Rate-limiting

### 32. Failing-open vs failing-closed by endpoint

Each call site picks the right policy:

| Bucket             | Policy       | Rationale                                          |
|--------------------|--------------|----------------------------------------------------|
| `bucket:login:*`   | fail-closed  | Unrestricted login is worse than briefly refusing it. |
| `bucket:view:*`    | fail-open    | Brief over-counting beats refusing legitimate views.  |
| `bucket:like:*`    | fail-open    | Same as view.                                         |

Documented in `internal/platform/app/app.go` next to each middleware mount.

## Operations

### 33. Migration partially applied (dirty state)

`golang-migrate` flags partially-applied migrations as "dirty". `make
migrate-status` surfaces this. `make migrate-force VERSION=N` clears the
flag after manual recovery. Documented in the Makefile target's `## help`.

### 34. Graceful shutdown

API: `SIGTERM` → stops accepting new connections, drains in-flight requests
for `HTTP_SHUTDOWN_TIMEOUT` (15s), then closes all stores.

Worker: `SIGTERM` → cancels the context the consumer loops are reading. The
view worker also flushes any partially-accumulated batch (best effort) on
the way out. Anything that doesn't drain in 10s is force-exited; the PEL
ensures redelivery.

### 35. Boot ordering on a fresh deploy

`config.Load` validates required values. `app.New` pings Postgres, Redis,
and ClickHouse before mounting the router — so a misconfigured backend
fails the deploy at startup, never under traffic. In Kubernetes this means
the readiness probe stays red and the LB doesn't send traffic.
