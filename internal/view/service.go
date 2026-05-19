package view

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/mhd7966/vidmerce/internal/platform/metrics"
)

// Service is the API-side view layer. It owns the filter chain and the
// stream emission; ClickHouse writes happen exclusively on the worker side.
type Service struct {
	rdb       *goredis.Client
	chain     *Chain
	durations DurationStore
	unique    *UniqueMarker
	log       *slog.Logger
	clock     func() time.Time
}

// NewService wires the service. `chain` is the spam-detection pipeline.
// `durations` is Redis-only on the hot path; `unique` marks first-in-window.
func NewService(
	rdb *goredis.Client,
	chain *Chain,
	durations DurationStore,
	unique *UniqueMarker,
	log *slog.Logger,
) *Service {
	return &Service{
		rdb:       rdb,
		chain:     chain,
		durations: durations,
		unique:    unique,
		log:       log,
		clock:     time.Now,
	}
}

// Track runs the filter chain. On accept, it marks uniqueness, XADDs onto
// stream:views, and returns whether the view counted as unique.
func (s *Service) Track(ctx context.Context, in Input) (Result, error) {
	if sec, ok := s.durations.Get(ctx, in.VideoID); ok {
		in.DurationSec = sec
	}

	ok, rejectedBy := s.chain.Apply(ctx, in)
	if !ok {
		metrics.RecordViewTrack("rejected")
		metrics.RecordViewFilterReject(rejectedBy)
		return Result{Accepted: false, RejectedBy: rejectedBy}, nil
	}

	isUnique := true
	if s.unique != nil {
		marked, err := s.unique.TryMark(ctx, in)
		if err != nil {
			s.log.Warn("unique view marker failed; counting as non-unique",
				slog.Any("error", err),
				slog.String("video_id", in.VideoID.String()),
			)
			isUnique = false
		} else {
			isUnique = marked
		}
	}

	ts := s.clock().UnixMilli()
	uFlag := "0"
	if isUnique {
		uFlag = "1"
	}

	args := []any{
		"vid", in.VideoID.String(),
		"ip", in.IPHash,
		"ua", in.UAHash,
		"c", in.Country,
		"u", uFlag,
		"t", strconv.FormatInt(ts, 10),
	}
	if in.ViewerID != nil {
		args = append(args, "uid", in.ViewerID.String())
	}

	if err := s.rdb.XAdd(ctx, &goredis.XAddArgs{
		Stream: streamKey,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: args,
	}).Err(); err != nil {
		metrics.RecordViewTrack("error")
		return Result{}, fmt.Errorf("xadd view: %w", err)
	}
	metrics.RecordViewTrack("accepted")
	return Result{Accepted: true, IsUnique: isUnique}, nil
}
