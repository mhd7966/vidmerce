-- 0001_init.up.sql
-- Initial schema for the Vidmerce platform.
--
-- Stores covered here:
--   users           : account credentials and identity
--   videos          : video metadata (the URL itself is external, e.g. S3)
--   products        : 1:1 product attached to a video (commerce layer)
--   likes           : user<->video like edges (source of truth for likes)
--   video_stats     : denormalised counters used by /stats and hot reads
--   follows         : prepared for personalised push-feed in a follow-up step
--
-- Conventions:
--   - UUID primary keys (gen_random_uuid()) so we don't leak ordering or scale.
--   - All timestamps are TIMESTAMPTZ with NOW() defaults.
--   - Foreign keys cascade on delete so deleting a user cleans up their data.
--   - Counters never live alone in Postgres without the source-of-truth table
--     that backs them: video_stats.likes_count is derivable from likes, and is
--     maintained by the like worker inside the same transaction as the edge
--     insert/delete (see Step 6).

-- pgcrypto provides gen_random_uuid(); citext gives us case-insensitive
-- email comparison without LOWER() everywhere. Both extensions are idempotent.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

-- =============================================================================
-- users
-- =============================================================================
CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         CITEXT      NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- =============================================================================
-- videos
-- =============================================================================
CREATE TABLE videos (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    video_url   TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Keyset-pagination index for the global feed: (created_at DESC, id DESC).
-- The compound order ensures cursor stability when two videos share a timestamp.
CREATE INDEX videos_feed_idx ON videos (created_at DESC, id DESC);

-- Per-author listing.
CREATE INDEX videos_user_created_idx ON videos (user_id, created_at DESC);

-- =============================================================================
-- products
-- =============================================================================
-- A product is 1:1 with a video (the spec uses singular `GET /videos/:id/product`).
-- We enforce the 1:1 with a UNIQUE constraint on video_id. Prices are stored as
-- integer cents to avoid binary-float pitfalls.
CREATE TABLE products (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    video_id    UUID        NOT NULL UNIQUE REFERENCES videos(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    price_cents BIGINT      NOT NULL CHECK (price_cents >= 0),
    currency    CHAR(3)     NOT NULL DEFAULT 'USD',
    image_url   TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- =============================================================================
-- likes
-- =============================================================================
-- The composite primary key (user_id, video_id) makes "no duplicate likes"
-- a database-enforced invariant, not just an application convention. The like
-- worker uses INSERT ... ON CONFLICT DO NOTHING so retries are no-ops.
CREATE TABLE likes (
    user_id    UUID        NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    video_id   UUID        NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, video_id)
);

-- "How many likes does this video have?" — index by video for COUNT(*) and
-- reconciliation queries.
CREATE INDEX likes_video_idx ON likes (video_id);

-- =============================================================================
-- video_stats
-- =============================================================================
-- Denormalised counters. likes_count is exact (maintained by the like worker
-- in the same TX as the edge change); views_count is approximate at the second
-- granularity (snapshotted from the ClickHouse view-events store).
CREATE TABLE video_stats (
    video_id     UUID        PRIMARY KEY REFERENCES videos(id) ON DELETE CASCADE,
    likes_count  BIGINT      NOT NULL DEFAULT 0 CHECK (likes_count >= 0),
    views_count  BIGINT      NOT NULL DEFAULT 0 CHECK (views_count >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Every video gets a stats row at creation time, so every increment/decrement
-- is a plain UPDATE (no upsert) and reads never have to deal with NULLs.
CREATE OR REPLACE FUNCTION videos_stats_init() RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO video_stats (video_id) VALUES (NEW.id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER videos_stats_init_trg
AFTER INSERT ON videos
FOR EACH ROW EXECUTE FUNCTION videos_stats_init();

-- =============================================================================
-- follows
-- =============================================================================
-- Prepared for the personalised push-feed variant. The current `FEED_MODE=push`
-- uses a global ZSET; if/when we enable per-user fan-out, this table is the
-- source of "who do I follow?".
CREATE TABLE follows (
    follower_id UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    followee_id UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (follower_id, followee_id),
    CHECK (follower_id <> followee_id)
);

CREATE INDEX follows_followee_idx ON follows (followee_id);
