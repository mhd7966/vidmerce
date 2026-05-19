package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	chplatform "github.com/mhd7966/vidmerce/internal/platform/clickhouse"
)

// ViewsCounter abstracts the read of (total_views, unique_views) for a video.
// Defined on the consumer side so the service can be unit-tested with a
// trivial in-memory fake — no ClickHouse container required.
type ViewsCounter interface {
	Counts(ctx context.Context, videoID uuid.UUID) (views, uniqueViews int64, err error)
}

// ClickHouseViewsRepo reads the pre-aggregated daily roll-up table populated
// by the materialized view from Step 7. Reading the rollup is much cheaper
// than scanning raw events:
//
//	video_views_daily is partitioned by month, ORDER BY (day, video_id) — a
//	single video's rows over the 90-day retention window touch a small slice
//	of one or two partitions, typically <1ms per query.
//
// We sum across all retained days; if/when the product surfaces "last 7
// days" or similar windows, the same table answers those queries with an
// extra WHERE on `day`.
type ClickHouseViewsRepo struct{ conn chplatform.Conn }

// NewClickHouseViewsRepo builds the repo against a ClickHouse connection.
func NewClickHouseViewsRepo(conn chplatform.Conn) *ClickHouseViewsRepo {
	return &ClickHouseViewsRepo{conn: conn}
}

// countsQuery uses sumIf to avoid a second roundtrip for the unique count.
// COALESCE-style fallback isn't needed: ClickHouse aggregate over zero rows
// returns 0 for sum() which is exactly what we want for "video with no
// views". Postgres-style "no row found" semantics don't apply here.
const countsQuery = `
SELECT
    toInt64(sum(views))        AS views,
    toInt64(sum(unique_views)) AS unique_views
FROM video_views_daily
WHERE video_id = ?
`

func (r *ClickHouseViewsRepo) Counts(ctx context.Context, videoID uuid.UUID) (int64, int64, error) {
	var views, unique int64
	if err := r.conn.QueryRow(ctx, countsQuery, videoID).Scan(&views, &unique); err != nil {
		return 0, 0, fmt.Errorf("clickhouse views count: %w", err)
	}
	return views, unique, nil
}
