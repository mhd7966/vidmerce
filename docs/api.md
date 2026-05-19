# API reference

Interactive docs: **[Swagger UI](http://localhost:8080/swagger/)** (when the API is
running). OpenAPI spec: `GET /swagger/openapi.yaml` (embedded from
`internal/platform/swagger/openapi.yaml`).

All endpoints return JSON wrapped in the standard envelope:

```json
{
  "code": "ok | validation_failed | unauthenticated | forbidden | not_found | conflict | rate_limited | internal | unavailable",
  "message": "human-readable string, empty on success",
  "data": "{ ... }, omitted when there is no payload",
  "meta": "{ ... }, present for paginated responses"
}
```

Status code maps 1:1 to `code`. Errors carry `message`; success leaves it
empty.

Authentication where required is `Authorization: Bearer <access_token>`.

## Auth

### `POST /auth/register`

Request:

```json
{ "email": "user@example.com", "password": "at-least-eight-chars" }
```

Responses:

- `201 ok`: returns `{ user: { id, email, created_at }, tokens: { access_token, refresh_token } }`.
- `400 validation_failed`: email invalid or password too short.
- `409 conflict`: email already registered.

### `POST /auth/login`

Request: same as register.

Responses:

- `200 ok`: same payload as register.
- `400 validation_failed`.
- `401 unauthenticated`: `invalid credentials` (uniform message — does not
  distinguish "user not found" from "bad password" to defeat
  enumeration).
- `429 rate_limited`: per-IP leaky bucket (10 attempts / 10 per min).

### `POST /auth/refresh`

Request:

```json
{ "refresh_token": "..." }
```

Responses:

- `200 ok`: new `{ access_token, refresh_token }`. The old refresh is
  revoked atomically.
- `401 unauthenticated`: token unknown, already used (rotated), or
  expired.

### `POST /auth/logout`

Request:

```json
{ "refresh_token": "..." }
```

Responses:

- `204 ok` (no body): token revoked.

## Videos

### `POST /videos` (auth required)

Request:

```json
{
  "title": "...",
  "description": "...",
  "video_url": "https://...",
  "duration_sec": 30
}
```

`duration_sec` (1–3600) is stored in Postgres and cached in Redis at
create time for view thresholds; `POST /view` never reads Postgres.

Responses:

- `201 ok`: returns the created video including `id`, `user_id`,
  `created_at`.
- `400 validation_failed`: missing field, title > 200 chars, bad URL.
- `401 unauthenticated`.

### `GET /videos/:id`

Public. Response: `200 ok` with the video, or `404 not_found`.

Cached for 60s; cache is pre-warmed on create.

## Products

### `POST /products` (auth required)

Caller must own the linked `video_id`.

Request:

```json
{
  "name": "...",
  "price_cents": 1999,
  "image_url": "https://...",
  "video_id": "uuid"
}
```

Responses:

- `201 ok`: returns the created product.
- `400 validation_failed`.
- `401 unauthenticated`.
- `403 forbidden`: caller does not own the video.
- `404 not_found`: video does not exist.
- `409 conflict`: a product already exists for this video.

### `GET /products/:id`

Public. `200 ok` or `404 not_found`.

### `GET /videos/:id/product`

Public. Returns the product linked to the video. `200 ok` or `404 not_found`.

## Feed

### `GET /feed`

Public. Cursor-based pagination.

Query:

- `cursor` (optional): opaque base64 token returned in previous response.
  Omit for first page.
- `limit` (optional): items per page. Default `FEED_PAGE_DEFAULT` (20),
  capped at `FEED_PAGE_MAX` (50).

Response (`200 ok`):

```json
{
  "code": "ok",
  "data": [
    { "id": "...", "user_id": "...", "title": "...", "description": "...", "video_url": "...", "created_at": "..." },
    ...
  ],
  "meta": {
    "pagination": {
      "next_cursor": "opaque-string-or-empty"
    }
  }
}
```

`next_cursor` is empty when there are no more pages. The same endpoint
serves pull and push modes; clients cannot tell which is active.

## Likes (auth required)

### `POST /videos/:id/like`
### `POST /videos/:id/unlike`

Idempotent: liking an already-liked video (or unliking an already-unliked
one) is a no-op.

Response (`202 ok`):

```json
{ "code": "ok", "data": { "liked": true, "count": 142 } }
```

Returns 202 (not 200) to surface that the Postgres persistence happens
asynchronously. `count` is from Redis (eventual); the exact Postgres
counter follows within tens of milliseconds.

Errors:

- `400 validation_failed`: bad UUID in path.
- `401 unauthenticated`.
- `429 rate_limited`: per-(user, video) leaky bucket.
- `500 internal`: Redis unreachable.

## Views

### `POST /videos/:id/view`

Optional auth. Counts **total views** (replays allowed) and marks
**unique views** once per `VIEW_UNIQUE_TTL` window per (viewer, video).
Watch threshold: client `watch_ms` ≥ one third of video length (from Redis
cache). Rate cap: ~`60/duration_sec` views per minute per (viewer, video);
videos shorter than `VIEW_MIN_DURATION_SEC` (5s) skip the rate cap only.

Request body (optional):

```json
{ "watch_ms": 12345, "country": "US" }
```

Response (`202 ok`, **always 202 regardless of accept/reject**):

```json
{ "code": "ok", "data": { "accepted": true, "is_unique": true, "rejected_by": "" } }
```

`rejected_by` is a `<filter>:<reason>` tag — e.g. `watch_threshold:below_threshold`
or `duration_rate:rate_limited`. Replays within the unique window are still
**accepted** (`accepted: true`, `is_unique: false`). The HTTP status is the
same in both cases so a spammer can't probe filter rules by watching
response codes.

## Analytics

### `GET /videos/:id/stats`

Public. Cached for `STATS_CACHE_TTL` (default 30s) with stampede protection
(see [`architecture.md`](architecture.md)).

Response:

```json
{
  "code": "ok",
  "data": {
    "video_id": "uuid",
    "views": 12345,
    "unique_views": 9876,
    "likes": 421,
    "engagement_rate": 0.0426,
    "computed_at": "2026-05-18T01:25:00Z"
  }
}
```

`engagement_rate = likes / unique_views`, with `unique_views == 0`
returning `0.0`.

Errors:

- `400 validation_failed`: bad UUID.
- `404 not_found`: video doesn't exist.
- `500 internal`: both backends failed.

## Operational

### `GET /health`

Liveness. Returns `200 ok` if the process is up. Does not contact backends.

### `GET /ready`

Readiness. Pings Postgres, Redis, and ClickHouse in parallel. Returns:

- `200 ok` with `{ status: "ready", dependencies: { postgres: "ok", redis: "ok", clickhouse: "ok" } }` if all are reachable.
- `503 unavailable` with the same shape but `status: "degraded"` and the
  failing dependency's value set to the error message.

## Request IDs

Every response carries `X-Request-ID`. The middleware generates one if the
client did not send one. All structured logs for that request include the
same ID — use it to correlate when filing bug reports.

## Rate-limit headers

Endpoints behind the leaky-bucket middleware return `Retry-After` (seconds)
on `429`. Clients should respect it.
