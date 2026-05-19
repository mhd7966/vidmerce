#!/usr/bin/env bash
# One-time repair: golang-migrate may have created view tables in `default` while
# the API reads CLICKHOUSE_DB=vidmerce. Idempotent — safe on fresh installs.
set -euo pipefail

if ! docker inspect vidmerce-clickhouse >/dev/null 2>&1; then
  echo "repair-clickhouse-db: vidmerce-clickhouse container not running" >&2
  exit 1
fi

ch() { docker exec vidmerce-clickhouse clickhouse-client -q "$1"; }

has_default=$(ch "SELECT count() FROM system.tables WHERE database='default' AND name='video_views'")
has_vidmerce=$(ch "SELECT count() FROM system.tables WHERE database='vidmerce' AND name='video_views'")

if [[ "$has_default" == "0" && "$has_vidmerce" != "0" ]]; then
  echo "repair-clickhouse-db: vidmerce schema OK"
  exit 0
fi

if [[ "$has_default" == "0" && "$has_vidmerce" == "0" ]]; then
  echo "repair-clickhouse-db: no view tables yet (run migrations first)" >&2
  exit 1
fi

echo "repair-clickhouse-db: moving tables from default → vidmerce"

ch "CREATE DATABASE IF NOT EXISTS vidmerce"

ch "CREATE TABLE IF NOT EXISTS vidmerce.video_views (
    event_time DateTime64(3, 'UTC'),
    video_id   UUID,
    viewer_id  Nullable(UUID),
    ip_hash    FixedString(32),
    country    LowCardinality(String),
    ua_hash    FixedString(16),
    is_unique  UInt8
) ENGINE = MergeTree
PARTITION BY toYYYYMM(event_time)
ORDER BY (video_id, event_time)
TTL toDateTime(event_time) + INTERVAL 90 DAY"

ch "CREATE TABLE IF NOT EXISTS vidmerce.video_views_daily (
    day Date, video_id UUID, views UInt64, unique_views UInt64
) ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (day, video_id)"

ch "INSERT INTO vidmerce.video_views SELECT * FROM default.video_views"
ch "INSERT INTO vidmerce.video_views_daily SELECT * FROM default.video_views_daily"

ch "DROP VIEW IF EXISTS default.video_views_daily_mv"
ch "DROP TABLE IF EXISTS default.video_views_daily"
ch "DROP TABLE IF EXISTS default.video_views"

ch "DROP VIEW IF EXISTS vidmerce.video_views_daily_mv"
ch "CREATE MATERIALIZED VIEW vidmerce.video_views_daily_mv
TO vidmerce.video_views_daily AS
SELECT
    toDate(event_time) AS day,
    video_id,
    count() AS views,
    sumIf(1, is_unique=1) AS unique_views
FROM vidmerce.video_views
GROUP BY day, video_id"

echo "repair-clickhouse-db: done"
