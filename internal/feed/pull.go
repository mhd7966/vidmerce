package feed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/video"
)

// PullSource is the slice of the video repository the pull fetcher needs.
// Defining the interface here (consumer side) makes the fetcher mockable in
// unit tests without touching pgx.
type PullSource interface {
	FeedPage(ctx context.Context, createdAt time.Time, id uuid.UUID, limit int) ([]video.Video, error)
}

// PullFetcher answers feed reads by running a keyset query against Postgres.
//
// Algorithm: fetch limit+1 rows; if we got limit+1, the last one becomes the
// next-page cursor and is *not* returned to the client. Avoids a separate
// COUNT(*) and keeps the per-request work proportional to page size.
type PullFetcher struct {
	src PullSource
}

// NewPullFetcher wires the fetcher.
func NewPullFetcher(src PullSource) *PullFetcher { return &PullFetcher{src: src} }

// Fetch implements Fetcher.
func (p *PullFetcher) Fetch(ctx context.Context, cursor Cursor, limit int) (Page, error) {
	if limit <= 0 {
		return Page{}, fmt.Errorf("limit must be positive")
	}
	rows, err := p.src.FeedPage(ctx, cursor.CreatedAt, cursor.ID, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("pull feed query: %w", err)
	}

	var nextCursor string
	if len(rows) > limit {
		// The (limit+1)-th item determines the next cursor and is stripped
		// from the response so the client never sees a "preview" of the next
		// page in the current one.
		last := rows[limit-1]
		nextCursor = Cursor{CreatedAt: last.CreatedAt, ID: last.ID}.Encode()
		rows = rows[:limit]
	}
	return Page{Items: rows, NextCursor: nextCursor}, nil
}
