package like

import (
	"context"
	"log/slog"
	"time"

	"github.com/mhd7966/vidmerce/internal/platform/metrics"
)

// Reconciler periodically walks a sample of video_stats rows and verifies
// that likes_count == COUNT(*) over the likes table. Drift should be zero
// under correct code; the reconciler exists as a tripwire (and a self-heal)
// in case a bug ever causes the worker and the source-of-truth to diverge.
type Reconciler struct {
	repo     Repository
	log      *slog.Logger
	interval time.Duration
	sample   int
}

// ReconcilerConfig parameterises the reconciler loop. Zero fields fall back
// to sensible defaults.
type ReconcilerConfig struct {
	Interval   time.Duration
	SampleSize int
}

// NewReconciler builds a reconciler.
func NewReconciler(repo Repository, log *slog.Logger, cfg ReconcilerConfig) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.SampleSize <= 0 {
		cfg.SampleSize = 200
	}
	return &Reconciler{repo: repo, log: log, interval: cfg.Interval, sample: cfg.SampleSize}
}

// Run loops every `interval` until ctx is cancelled. The first pass runs
// immediately on start so a freshly deployed system gets verified once.
func (r *Reconciler) Run(ctx context.Context) error {
	r.log.Info("like reconciler started",
		slog.Duration("interval", r.interval),
		slog.Int("sample", r.sample),
	)
	t := time.NewTicker(r.interval)
	defer t.Stop()

	// Initial pass.
	if err := r.Once(ctx); err != nil && ctx.Err() == nil {
		r.log.Error("reconciler initial pass failed", slog.Any("error", err))
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.Once(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("reconciler pass failed", slog.Any("error", err))
			}
		}
	}
}

// Once runs a single reconciliation pass. Exposed so tests don't have to
// drive the ticker.
func (r *Reconciler) Once(ctx context.Context) error {
	ids, err := r.repo.SampleVideoIDs(ctx, r.sample)
	if err != nil {
		return err
	}
	var checked, drifted, corrected int
	for _, id := range ids {
		stored, err := r.repo.Count(ctx, id)
		if err != nil {
			r.log.Warn("reconciler count failed",
				slog.String("video_id", id.String()),
				slog.Any("error", err))
			continue
		}
		exact, err := r.repo.CountFromEdges(ctx, id)
		if err != nil {
			r.log.Warn("reconciler edges count failed",
				slog.String("video_id", id.String()),
				slog.Any("error", err))
			continue
		}
		checked++
		if stored == exact {
			continue
		}
		drifted++
		n, err := r.repo.ReconcileCounter(ctx, id, exact)
		if err != nil {
			r.log.Error("reconciler write failed",
				slog.String("video_id", id.String()),
				slog.Int64("stored", stored),
				slog.Int64("exact", exact),
				slog.Any("error", err))
			continue
		}
		corrected += int(n)
		// Drift fires an *error* log even after we fix it: under correct code,
		// this branch should never run.
		r.log.Error("like-counter drift detected and corrected",
			slog.String("video_id", id.String()),
			slog.Int64("stored", stored),
			slog.Int64("exact", exact),
		)
	}
	metrics.LikeReconcilerCheckedTotal.Add(float64(checked))
	if drifted > 0 {
		metrics.LikeReconcilerDriftTotal.Add(float64(drifted))
	}
	r.log.Info("reconciler pass complete",
		slog.Int("checked", checked),
		slog.Int("drifted", drifted),
		slog.Int("corrected", corrected),
	)
	return nil
}
