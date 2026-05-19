-- 0001_init.up.sql (ClickHouse)
-- Schema for the analytical view-events store.
--
-- golang-migrate's ClickHouse driver may leave the session on `default` even when
-- the DSN names another database — qualify everything under `vidmerce` so the
-- API (CLICKHOUSE_DB=vidmerce) and migrations stay aligned.

CREATE DATABASE IF NOT EXISTS vidmerce;

CREATE TABLE IF NOT EXISTS vidmerce.video_views (
    event_time DateTime64(3, 'UTC'),
    video_id   UUID,
    viewer_id  Nullable(UUID),
    ip_hash    FixedString(32),
    country    LowCardinality(String),
    ua_hash    FixedString(16),
    is_unique  UInt8
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(event_time)
ORDER BY (video_id, event_time)
TTL toDateTime(event_time) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS vidmerce.video_views_daily (
    day          Date,
    video_id     UUID,
    views        UInt64,
    unique_views UInt64
)
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (day, video_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS vidmerce.video_views_daily_mv
TO vidmerce.video_views_daily AS
SELECT
    toDate(event_time)    AS day,
    video_id              AS video_id,
    count()               AS views,
    sumIf(1, is_unique=1) AS unique_views
FROM vidmerce.video_views
GROUP BY day, video_id;
