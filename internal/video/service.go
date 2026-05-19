package video

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/cache"
	"github.com/mhd7966/vidmerce/internal/view"
)

// Cache is the read-through cache contract the service depends on. The
// concrete *cache.JSONCache[Video] in the platform package satisfies this
// shape; tests can substitute a fake.
type Cache interface {
	Get(ctx context.Context, id string) (Video, bool, error)
	Set(ctx context.Context, id string, v Video) error
	Del(ctx context.Context, id string) error
}

// Compile-time check: the concrete cache type fits the interface.
var _ Cache = (*cache.JSONCache[Video])(nil)

// Service is the business layer. It owns validation, ordering of repository
// vs cache operations, and structured logging. It does not touch HTTP types.
type Service struct {
	repo      Repository
	cache     Cache
	durations view.DurationStore // Redis cache for view hot path; optional
	log       *slog.Logger
	onCreate  func(ctx context.Context, v Video) // optional event hook
}

// Option lets callers extend the service without changing the constructor
// signature. We use it for cross-cutting concerns that are wired at
// composition time (e.g. push-feed fan-out) and that we don't want to bake
// into the package's mandatory contract.
type Option func(*Service)

// WithOnCreate registers a callback fired after a successful Create. Errors
// from the hook are logged but never surfaced to the API caller — the create
// already succeeded; a downstream notification failure is a separate concern.
//
// In practice this is used to publish a "video created" event into the
// feed:global ZSET when FEED_MODE=push.
func WithOnCreate(fn func(ctx context.Context, v Video)) Option {
	return func(s *Service) { s.onCreate = fn }
}

// NewService wires the service. Pass nil for cache to disable caching (useful
// in tests). Apply Options for behaviours that aren't part of the core contract.
// NewService wires the service. Pass nil for durations to skip Redis duration
// caching (tests); production passes view.NewRedisDurationStore.
func NewService(repo Repository, c Cache, durations view.DurationStore, log *slog.Logger, opts ...Option) *Service {
	s := &Service{repo: repo, cache: c, durations: durations, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create persists a new video owned by userID and returns it.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Video, error) {
	if err := in.Validate(); err != nil {
		return Video{}, err
	}
	v, err := s.repo.Create(ctx, userID, in)
	if err != nil {
		return Video{}, err
	}
	// Pre-warm the cache: the very next read is almost always for the just-
	// created resource (the client follows POST with a navigation to it).
	if s.cache != nil {
		if err := s.cache.Set(ctx, v.ID.String(), v); err != nil {
			s.log.Warn("video cache prewarm failed", slog.Any("error", err))
		}
	}
	if s.durations != nil {
		if err := s.durations.Set(ctx, v.ID, v.DurationSec); err != nil {
			s.log.Warn("video duration cache failed", slog.Any("error", err))
		}
	}
	// Fire the optional post-create hook (e.g. push-feed fan-out). We deliberately
	// run it in-line so failures are observable in logs; in a higher-volume
	// system this would move onto an event stream.
	if s.onCreate != nil {
		s.onCreate(ctx, v)
	}
	return v, nil
}

// Get returns a video by id, consulting the cache first.
//
// Cache-miss policy: we never let a cache failure block the request — a Redis
// outage degrades latency, not correctness. The DB is the source of truth.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Video, error) {
	if s.cache != nil {
		if v, hit, err := s.cache.Get(ctx, id.String()); err != nil {
			s.log.Warn("video cache get failed", slog.Any("error", err))
		} else if hit {
			return v, nil
		}
	}
	v, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return Video{}, err
	}
	if s.cache != nil {
		if err := s.cache.Set(ctx, v.ID.String(), v); err != nil {
			s.log.Warn("video cache set failed", slog.Any("error", err))
		}
	}
	return v, nil
}

// Exists reports whether a video with the given id exists. Wraps Get so the
// (cached) lookup is reused, but flattens the not-found case into ok=false
// rather than an error — convenient for callers (like the stats endpoint)
// that need a 404-or-continue branch without parsing error sentinels.
func (s *Service) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	if _, err := s.Get(ctx, id); err != nil {
		if errors.Is(err, ErrVideoNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// AssertOwner returns ErrVideoNotFound if the video doesn't exist or
// ErrForbidden if it exists but is owned by someone else. Used by the product
// service to gate POST /products on the caller owning the linked video.
func (s *Service) AssertOwner(ctx context.Context, videoID, userID uuid.UUID) error {
	v, err := s.Get(ctx, videoID)
	if err != nil {
		return err
	}
	if v.UserID != userID {
		return ErrForbidden
	}
	return nil
}

// IsNotFound is a small helper so other packages don't have to import this
// package's errors directly.
func IsNotFound(err error) bool { return errors.Is(err, ErrVideoNotFound) }
