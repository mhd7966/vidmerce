package like

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/db"
)

// Repository is the persistence contract used by the worker. Defined here
// (consumer side) so the worker can be unit-tested with an in-memory fake.
type Repository interface {
	// Apply inserts (op=OpLike) or deletes (op=OpUnlike) the edge AND updates
	// video_stats.likes_count in the same transaction. The returned Result
	// reflects what the database actually did — duplicate events are no-ops
	// and Changed will be false.
	Apply(ctx context.Context, userID, videoID uuid.UUID, op Op) (Result, error)

	// Count returns the *exact* current likes_count from video_stats. Used by
	// the service on cache miss and by the reconciler.
	Count(ctx context.Context, videoID uuid.UUID) (int64, error)

	// CountFromEdges returns COUNT(*) over the likes table for the video.
	// Used only by the reconciler — never on the hot path. Slow on large
	// videos but the reconciler operates on samples.
	CountFromEdges(ctx context.Context, videoID uuid.UUID) (int64, error)

	// ReconcileCounter UPDATEs video_stats.likes_count to the given exact
	// value. Returns the number of rows modified (0 if the stored value was
	// already correct).
	ReconcileCounter(ctx context.Context, videoID uuid.UUID, exact int64) (int64, error)

	// SampleVideoIDs returns up to `limit` video ids ordered by least-recently
	// reconciled. The reconciler walks this slice to find drift.
	SampleVideoIDs(ctx context.Context, limit int) ([]uuid.UUID, error)
}

// pgRepo is the Postgres implementation.
type pgRepo struct{ pool *db.Pool }

// NewPostgresRepository wires the repository against a pgx pool.
func NewPostgresRepository(pool *db.Pool) Repository { return &pgRepo{pool: pool} }

// applyLikeSQL is one round-trip:
//
//	INSERT into likes ON CONFLICT DO NOTHING → returns 0 or 1 inserted rows.
//	IF a row was inserted → UPDATE video_stats.likes_count = likes_count + 1.
//	SELECT the resulting count to return to the caller.
//
// Wrapping these in a CTE means the whole thing is a single statement and
// pgx treats it as a single autocommitted transaction — atomic and crash-
// safe even though it's not wrapped in an explicit BEGIN/COMMIT.
const applyLikeSQL = `
WITH ins AS (
    INSERT INTO likes (user_id, video_id)
    VALUES ($1, $2)
    ON CONFLICT (user_id, video_id) DO NOTHING
    RETURNING 1
),
upd AS (
    UPDATE video_stats
       SET likes_count = likes_count + 1,
           updated_at  = NOW()
     WHERE video_id = $2
       AND EXISTS (SELECT 1 FROM ins)
    RETURNING likes_count
)
SELECT
    EXISTS (SELECT 1 FROM ins)                                AS changed,
    COALESCE(
        (SELECT likes_count FROM upd),
        (SELECT likes_count FROM video_stats WHERE video_id = $2),
        0
    )                                                          AS count
`

const applyUnlikeSQL = `
WITH del AS (
    DELETE FROM likes
     WHERE user_id = $1 AND video_id = $2
    RETURNING 1
),
upd AS (
    UPDATE video_stats
       SET likes_count = GREATEST(likes_count - 1, 0),
           updated_at  = NOW()
     WHERE video_id = $2
       AND EXISTS (SELECT 1 FROM del)
    RETURNING likes_count
)
SELECT
    EXISTS (SELECT 1 FROM del)                                AS changed,
    COALESCE(
        (SELECT likes_count FROM upd),
        (SELECT likes_count FROM video_stats WHERE video_id = $2),
        0
    )                                                          AS count
`

func (r *pgRepo) Apply(ctx context.Context, userID, videoID uuid.UUID, op Op) (Result, error) {
	var q string
	switch op {
	case OpLike:
		q = applyLikeSQL
	case OpUnlike:
		q = applyUnlikeSQL
	default:
		return Result{}, fmt.Errorf("unknown op %q", op)
	}
	var (
		changed bool
		count   int64
	)
	err := r.pool.QueryRow(ctx, q, userID, videoID).Scan(&changed, &count)
	if err != nil {
		return Result{}, fmt.Errorf("apply %s: %w", op, err)
	}
	return Result{Changed: changed, NewCount: count, WasNoop: !changed}, nil
}

func (r *pgRepo) Count(ctx context.Context, videoID uuid.UUID) (int64, error) {
	const q = `SELECT likes_count FROM video_stats WHERE video_id = $1`
	var c int64
	if err := r.pool.QueryRow(ctx, q, videoID).Scan(&c); err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return c, nil
}

func (r *pgRepo) CountFromEdges(ctx context.Context, videoID uuid.UUID) (int64, error) {
	const q = `SELECT COUNT(*) FROM likes WHERE video_id = $1`
	var c int64
	if err := r.pool.QueryRow(ctx, q, videoID).Scan(&c); err != nil {
		return 0, fmt.Errorf("count from edges: %w", err)
	}
	return c, nil
}

func (r *pgRepo) ReconcileCounter(ctx context.Context, videoID uuid.UUID, exact int64) (int64, error) {
	const q = `
		UPDATE video_stats
		   SET likes_count = $2, updated_at = NOW()
		 WHERE video_id = $1
		   AND likes_count <> $2
	`
	tag, err := r.pool.Exec(ctx, q, videoID, exact)
	if err != nil {
		return 0, fmt.Errorf("reconcile: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *pgRepo) SampleVideoIDs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		return nil, nil
	}
	// updated_at gives us a rough "least-recently reconciled" ordering because
	// the worker bumps it on every successful Apply and the reconciler bumps
	// it whenever it rewrites a row. Stable enough for sampling.
	const q = `
		SELECT video_id
		FROM video_stats
		ORDER BY updated_at ASC NULLS FIRST
		LIMIT $1
	`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("sample: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
