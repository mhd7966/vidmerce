package bloom

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	KeyEmails        = "bloom:emails"
	KeyProductVideos = "bloom:product_videos"
)

// WarmupEmails loads all user emails from Postgres into the Bloom filter.
func WarmupEmails(ctx context.Context, pool *pgxpool.Pool, f *RedisFilter) error {
	if err := f.Init(ctx); err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `SELECT email FROM users`)
	if err != nil {
		return fmt.Errorf("select emails: %w", err)
	}
	defer rows.Close()

	const batch = 5000
	buf := make([]string, 0, batch)
	var total int
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return fmt.Errorf("scan email: %w", err)
		}
		buf = append(buf, email)
		if len(buf) >= batch {
			if err := f.AddBatch(ctx, buf); err != nil {
				return err
			}
			total += len(buf)
			buf = buf[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(buf) > 0 {
		if err := f.AddBatch(ctx, buf); err != nil {
			return err
		}
		total += len(buf)
	}
	return nil
}

// WarmupProductVideos loads all product video_id values into the Bloom filter.
func WarmupProductVideos(ctx context.Context, pool *pgxpool.Pool, f *RedisFilter) error {
	if err := f.Init(ctx); err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `SELECT video_id::text FROM products`)
	if err != nil {
		return fmt.Errorf("select video_ids: %w", err)
	}
	defer rows.Close()

	const batch = 5000
	buf := make([]string, 0, batch)
	for rows.Next() {
		var vid string
		if err := rows.Scan(&vid); err != nil {
			return fmt.Errorf("scan video_id: %w", err)
		}
		buf = append(buf, vid)
		if len(buf) >= batch {
			if err := f.AddBatch(ctx, buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(buf) > 0 {
		return f.AddBatch(ctx, buf)
	}
	return nil
}
