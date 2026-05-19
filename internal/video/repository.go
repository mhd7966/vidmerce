package video

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mhd7966/vidmerce/internal/platform/db"
)

// Repository abstracts video storage. Defined consumer-side so the service is
// unit-testable with an in-memory fake.
type Repository interface {
	Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Video, error)
	FindByID(ctx context.Context, id uuid.UUID) (Video, error)
	// FindByIDs returns the videos whose ids are in `ids`, in *no particular
	// order*. Missing ids are silently dropped. Used by the push-feed hydrator
	// to materialise a ZSET-derived page into full Video records in one round
	// trip instead of N.
	FindByIDs(ctx context.Context, ids []uuid.UUID) ([]Video, error)
	// FeedPage returns up to `limit` videos older than the (createdAt, id)
	// cursor pair, ordered newest-first. If both cursor components are zero
	// values it starts from the head of the feed. Used by the pull-feed
	// fetcher.
	FeedPage(ctx context.Context, createdAt time.Time, id uuid.UUID, limit int) ([]Video, error)
}

// pgRepo is the Postgres implementation.
type pgRepo struct{ pool *db.Pool }

// NewPostgresRepository constructs a repository bound to a pgx pool.
func NewPostgresRepository(pool *db.Pool) Repository { return &pgRepo{pool: pool} }

func (r *pgRepo) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Video, error) {
	const q = `
		INSERT INTO videos (user_id, title, description, video_url, duration_sec)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, user_id, title, description, video_url, duration_sec, created_at
	`
	var v Video
	err := r.pool.QueryRow(ctx, q,
		userID,
		strings.TrimSpace(in.Title),
		in.Description,
		strings.TrimSpace(in.VideoURL),
		in.DurationSec,
	).Scan(&v.ID, &v.UserID, &v.Title, &v.Description, &v.VideoURL, &v.DurationSec, &v.CreatedAt)
	if err != nil {
		return Video{}, fmt.Errorf("insert video: %w", err)
	}
	return v, nil
}

func (r *pgRepo) FindByID(ctx context.Context, id uuid.UUID) (Video, error) {
	const q = `
		SELECT id, user_id, title, description, video_url, duration_sec, created_at
		FROM videos
		WHERE id = $1
	`
	var v Video
	err := r.pool.QueryRow(ctx, q, id).
		Scan(&v.ID, &v.UserID, &v.Title, &v.Description, &v.VideoURL, &v.DurationSec, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Video{}, ErrVideoNotFound
	}
	if err != nil {
		return Video{}, fmt.Errorf("select video: %w", err)
	}
	return v, nil
}

func (r *pgRepo) FindByIDs(ctx context.Context, ids []uuid.UUID) ([]Video, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const q = `
		SELECT id, user_id, title, description, video_url, duration_sec, created_at
		FROM videos
		WHERE id = ANY($1)
	`
	rows, err := r.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("select videos by ids: %w", err)
	}
	defer rows.Close()

	out := make([]Video, 0, len(ids))
	for rows.Next() {
		var v Video
		if err := rows.Scan(&v.ID, &v.UserID, &v.Title, &v.Description, &v.VideoURL, &v.DurationSec, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan video: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FeedPage uses the keyset compound `(created_at, id)` so cursors are stable
// across ties. The supporting index is `videos_feed_idx` (Step 2 migration).
//
// When createdAt.IsZero() && id == uuid.Nil we treat the cursor as the start
// of feed and skip the WHERE clause entirely; the planner uses the same index
// for both code paths.
func (r *pgRepo) FeedPage(ctx context.Context, createdAt time.Time, id uuid.UUID, limit int) ([]Video, error) {
	if limit <= 0 {
		return nil, nil
	}
	var (
		rows pgx.Rows
		err  error
	)
	if createdAt.IsZero() && id == uuid.Nil {
		const q = `
			SELECT id, user_id, title, description, video_url, duration_sec, created_at
			FROM videos
			ORDER BY created_at DESC, id DESC
			LIMIT $1
		`
		rows, err = r.pool.Query(ctx, q, limit)
	} else {
		const q = `
			SELECT id, user_id, title, description, video_url, duration_sec, created_at
			FROM videos
			WHERE (created_at, id) < ($1, $2)
			ORDER BY created_at DESC, id DESC
			LIMIT $3
		`
		rows, err = r.pool.Query(ctx, q, createdAt, id, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("feed page query: %w", err)
	}
	defer rows.Close()

	out := make([]Video, 0, limit)
	for rows.Next() {
		var v Video
		if err := rows.Scan(&v.ID, &v.UserID, &v.Title, &v.Description, &v.VideoURL, &v.DurationSec, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan feed row: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
