package like

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/mhd7966/vidmerce/internal/platform/metrics"
)

// Worker drains stream:likes and persists each event to Postgres via the
// repository. It is the only writer to the likes table on the steady-state
// hot path; the reconciler is the only other writer to video_stats and it
// only ever fixes drift, not real events.
//
// Delivery model: at-least-once. Combined with `INSERT ... ON CONFLICT DO
// NOTHING` and `DELETE WHERE EXISTS`, replays are no-ops. Worker order: we
// run with one consumer per stream so events for the same (user, video) are
// processed in publish order. Horizontal scaling beyond one consumer requires
// stream partitioning by hash(user_id); documented in scalability.md.
type Worker struct {
	rdb          *goredis.Client
	repo         Repository
	log          *slog.Logger
	group        string
	consumer     string
	batchSize    int64
	blockTimeout time.Duration
}

// WorkerConfig holds parameters for the worker loop. Defaults are applied in
// NewWorker for any zero field, so callers can pass just what they want to
// override.
type WorkerConfig struct {
	Group        string
	Consumer     string
	BatchSize    int
	BlockTimeout time.Duration
}

// NewWorker constructs a worker. Call Run to start the consume loop.
func NewWorker(rdb *goredis.Client, repo Repository, log *slog.Logger, cfg WorkerConfig) *Worker {
	if cfg.Group == "" {
		cfg.Group = "vidmerce-workers"
	}
	if cfg.Consumer == "" {
		cfg.Consumer = "worker-1"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.BlockTimeout <= 0 {
		cfg.BlockTimeout = 5 * time.Second
	}
	return &Worker{
		rdb:          rdb,
		repo:         repo,
		log:          log,
		group:        cfg.Group,
		consumer:     cfg.Consumer,
		batchSize:    int64(cfg.BatchSize),
		blockTimeout: cfg.BlockTimeout,
	}
}

// Run starts the consume loop and blocks until ctx is cancelled. It is safe
// to call Run from multiple goroutines per process (each one becomes a
// separate consumer in the group) — but as noted in the type doc, that
// breaks per-(user, video) ordering and should only be done when the events
// are independent.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.ensureGroup(ctx); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	w.log.Info("like worker started",
		slog.String("stream", streamKey),
		slog.String("group", w.group),
		slog.String("consumer", w.consumer),
	)

	for {
		if ctx.Err() != nil {
			return nil
		}
		entries, err := w.read(ctx)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil
		case err != nil:
			w.log.Error("xreadgroup failed; backing off",
				slog.Any("error", err))
			time.Sleep(time.Second)
			continue
		}
		for _, e := range entries {
			w.process(ctx, e)
		}
	}
}

// ensureGroup creates the consumer group if it does not exist. BUSYGROUP is
// not an error — it just means another instance got here first.
func (w *Worker) ensureGroup(ctx context.Context) error {
	err := w.rdb.XGroupCreateMkStream(ctx, streamKey, w.group, "$").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

func (w *Worker) read(ctx context.Context) ([]goredis.XMessage, error) {
	streams, err := w.rdb.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group:    w.group,
		Consumer: w.consumer,
		Streams:  []string{streamKey, ">"},
		Count:    w.batchSize,
		Block:    w.blockTimeout,
		NoAck:    false,
	}).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	if len(streams) == 0 {
		return nil, nil
	}
	return streams[0].Messages, nil
}

func (w *Worker) process(ctx context.Context, m goredis.XMessage) {
	ev, ok := parseEvent(m)
	if !ok {
		// Malformed entry — ACK so we don't loop on it; log so it's visible.
		w.log.Warn("dropping malformed stream entry",
			slog.String("id", m.ID),
			slog.Any("values", m.Values),
		)
		_ = w.rdb.XAck(ctx, streamKey, w.group, m.ID).Err()
		return
	}

	start := time.Now()
	res, err := w.repo.Apply(ctx, ev.UserID, ev.VideoID, ev.Op)
	if err != nil {
		// Do NOT XACK on apply failure; the entry stays pending and will be
		// redelivered to this consumer on the next pass. Crash-loop protection
		// is handled by the supervisor (k8s back-off), not here.
		w.log.Error("like apply failed; will retry",
			slog.String("id", m.ID),
			slog.Any("error", err),
		)
		return
	}
	metrics.RecordLikeWorkerApply(string(ev.Op), res.Changed, time.Since(start))
	w.log.Debug("like applied",
		slog.String("id", m.ID),
		slog.String("op", string(ev.Op)),
		slog.Bool("changed", res.Changed),
		slog.Int64("count", res.NewCount),
	)
	_ = w.rdb.XAck(ctx, streamKey, w.group, m.ID).Err()
}

// parseEvent extracts an Event from a stream message. Returns false for any
// missing / unparseable field so the worker can drop the message instead of
// retrying forever.
func parseEvent(m goredis.XMessage) (Event, bool) {
	uidStr, _ := m.Values["uid"].(string)
	vidStr, _ := m.Values["vid"].(string)
	opStr, _ := m.Values["op"].(string)
	uid, err1 := uuid.Parse(uidStr)
	vid, err2 := uuid.Parse(vidStr)
	if err1 != nil || err2 != nil || (opStr != string(OpLike) && opStr != string(OpUnlike)) {
		return Event{}, false
	}
	return Event{UserID: uid, VideoID: vid, Op: Op(opStr), StreamID: m.ID}, true
}
