-- 0001_init.down.sql
-- Reverse of 0001_init.up.sql. Dropped in dependency-safe order.

DROP TABLE IF EXISTS follows;

DROP TRIGGER IF EXISTS videos_stats_init_trg ON videos;
DROP FUNCTION IF EXISTS videos_stats_init();
DROP TABLE IF EXISTS video_stats;

DROP TABLE IF EXISTS likes;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS videos;
DROP TABLE IF EXISTS users;

-- We intentionally do NOT drop the citext / pgcrypto extensions: other
-- databases on the same cluster may depend on them.
