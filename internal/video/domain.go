// Package video owns the videos table in Postgres and exposes the HTTP
// surface for creating and reading videos. Caching for GET /videos/:id lives
// here too, but the cache is a write-around accessor — the database is always
// the source of truth.
//
// What this package does *not* do:
//   - Persist or fetch products. That's internal/product.
//   - Compute view or like counts. Those live in their own packages and the
//     analytics endpoint joins them.
//   - Render the feed. Feed-mode logic is internal/feed.
package video

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Domain errors.
var (
	ErrVideoNotFound = errors.New("video not found")
	ErrForbidden     = errors.New("forbidden")
)

// ValidationError carries a user-visible message describing what was wrong
// with the request. The handler maps it to 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string                  { return e.Msg }
func (e *ValidationError) Is(target error) bool           { _, ok := target.(*ValidationError); return ok }

// Video is the canonical video record. The video_url is an external URL
// (typically a presigned S3 / CDN link); the platform does not host bytes.
type Video struct {
	ID           uuid.UUID `json:"id"`
	UserID       uuid.UUID `json:"user_id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	VideoURL     string    `json:"video_url"`
	DurationSec  int       `json:"duration_sec"`
	CreatedAt    time.Time `json:"created_at"`
}

// CreateInput is what callers pass to Service.Create. We keep it as a small
// value type rather than reusing the wire DTO so the service layer is free of
// JSON tags / Gin binding tags.
type CreateInput struct {
	Title        string
	Description  string
	VideoURL     string
	DurationSec  int
}

// Validate runs cheap, format-only checks. It is *not* a substitute for the
// database constraints that ultimately enforce these invariants.
func (in CreateInput) Validate() error {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return &ValidationError{Msg: "title is required"}
	}
	if len(title) > 200 {
		return &ValidationError{Msg: "title must be at most 200 characters"}
	}
	if len(in.Description) > 5000 {
		return &ValidationError{Msg: "description must be at most 5000 characters"}
	}
	if strings.TrimSpace(in.VideoURL) == "" {
		return &ValidationError{Msg: "video_url is required"}
	}
	u, err := url.Parse(in.VideoURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &ValidationError{Msg: "video_url must be a valid http(s) URL"}
	}
	if len(in.VideoURL) > 2048 {
		return &ValidationError{Msg: "video_url must be at most 2048 characters"}
	}
	if in.DurationSec <= 0 {
		return &ValidationError{Msg: "duration_sec is required"}
	}
	if in.DurationSec > 3600 {
		return &ValidationError{Msg: "duration_sec must be at most 3600"}
	}
	return nil
}
