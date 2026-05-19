package product

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/bloom"
	"github.com/mhd7966/vidmerce/internal/platform/cache"
	"github.com/mhd7966/vidmerce/internal/video"
)

// Cache is the per-id cache contract (cache key e.g. "product:{id}").
type Cache interface {
	Get(ctx context.Context, id string) (Product, bool, error)
	Set(ctx context.Context, id string, p Product) error
	Del(ctx context.Context, id string) error
}

// Compile-time check.
var _ Cache = (*cache.JSONCache[Product])(nil)

// VideoOwnerAsserter is the slice of the video service we depend on. Defining
// the interface here (consumer side) means we can mock ownership checks in
// tests without spinning up a full video service.
type VideoOwnerAsserter interface {
	AssertOwner(ctx context.Context, videoID, userID uuid.UUID) error
}

// Service is the business layer for products.
type Service struct {
	repo         Repository
	cache        Cache       // by-id cache: "product:{id}"
	byVideoCache Cache       // by-video-id cache: "video:{id}:product"
	videos       VideoOwnerAsserter
	log          *slog.Logger
	videoBloom   bloom.Filter // optional; one product per video_id
}

// NewService wires the service. Both caches are optional (pass nil to disable).
func NewService(
	repo Repository,
	byIDCache, byVideoCache Cache,
	videos VideoOwnerAsserter,
	log *slog.Logger,
	videoBloom bloom.Filter,
) *Service {
	return &Service{
		repo:         repo,
		cache:        byIDCache,
		byVideoCache: byVideoCache,
		videos:       videos,
		log:          log,
		videoBloom:   videoBloom,
	}
}

// Create persists a new product attached to a video the caller owns.
//
// Ordering:
//  1. Validate inputs.
//  2. AssertOwner — short-circuits with 403/404 before any write.
//  3. Insert. Unique-constraint failure → ErrVideoAlreadyTaken (409).
//  4. Invalidate the by-video cache key (in case a previous miss had cached
//     "not found"; with current code that doesn't happen, but the invalidation
//     is cheap insurance against future regressions).
func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Product, error) {
	if err := in.Validate(); err != nil {
		return Product{}, err
	}
	if err := s.videos.AssertOwner(ctx, in.VideoID, userID); err != nil {
		return Product{}, err
	}
	vidKey := in.VideoID.String()
	if dup, err := s.videoMaybeTaken(ctx, vidKey); err != nil {
		return Product{}, err
	} else if dup {
		return Product{}, ErrVideoAlreadyTaken
	}
	p, err := s.repo.Create(ctx, in)
	if err != nil {
		if errors.Is(err, ErrVideoAlreadyTaken) {
			s.recordVideo(ctx, vidKey)
		}
		return Product{}, err
	}
	s.recordVideo(ctx, vidKey)
	if s.cache != nil {
		if err := s.cache.Set(ctx, p.ID.String(), p); err != nil {
			s.log.Warn("product cache prewarm failed", slog.Any("error", err))
		}
	}
	if s.byVideoCache != nil {
		if err := s.byVideoCache.Set(ctx, p.VideoID.String(), p); err != nil {
			s.log.Warn("video-product cache prewarm failed", slog.Any("error", err))
		}
	}
	return p, nil
}

// GetByID reads a product by its own id, consulting the cache first.
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (Product, error) {
	if s.cache != nil {
		if p, hit, err := s.cache.Get(ctx, id.String()); err != nil {
			s.log.Warn("product cache get failed", slog.Any("error", err))
		} else if hit {
			return p, nil
		}
	}
	p, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return Product{}, err
	}
	if s.cache != nil {
		if err := s.cache.Set(ctx, p.ID.String(), p); err != nil {
			s.log.Warn("product cache set failed", slog.Any("error", err))
		}
	}
	return p, nil
}

// GetByVideoID reads the product attached to a video. We use a separate cache
// namespace so invalidations are precise (changing the video doesn't bust
// product caches and vice versa).
func (s *Service) GetByVideoID(ctx context.Context, videoID uuid.UUID) (Product, error) {
	if s.byVideoCache != nil {
		if p, hit, err := s.byVideoCache.Get(ctx, videoID.String()); err != nil {
			s.log.Warn("video-product cache get failed", slog.Any("error", err))
		} else if hit {
			return p, nil
		}
	}
	p, err := s.repo.FindByVideoID(ctx, videoID)
	if err != nil {
		return Product{}, err
	}
	if s.byVideoCache != nil {
		if err := s.byVideoCache.Set(ctx, videoID.String(), p); err != nil {
			s.log.Warn("video-product cache set failed", slog.Any("error", err))
		}
	}
	return p, nil
}

func (s *Service) videoMaybeTaken(ctx context.Context, videoID string) (bool, error) {
	if s.videoBloom == nil {
		return false, nil
	}
	maybe, err := s.videoBloom.MayContain(ctx, videoID)
	if err != nil {
		s.log.Warn("product video bloom check failed; falling back to db", slog.Any("error", err))
		return false, nil
	}
	return maybe, nil
}

func (s *Service) recordVideo(ctx context.Context, videoID string) {
	if s.videoBloom == nil {
		return
	}
	if err := s.videoBloom.Add(ctx, videoID); err != nil {
		s.log.Warn("product video bloom add failed", slog.Any("error", err))
	}
}

// IsNotFound is a small helper so callers don't import this package's errors.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrProductNotFound) || errors.Is(err, video.ErrVideoNotFound)
}
