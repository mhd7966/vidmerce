-- Video length drives view thresholds and per-video rate limits on the hot path
-- (duration is cached in Redis at create time; /view never reads Postgres).
ALTER TABLE videos
    ADD COLUMN IF NOT EXISTS duration_sec INT NOT NULL DEFAULT 30
        CHECK (duration_sec > 0 AND duration_sec <= 3600);
