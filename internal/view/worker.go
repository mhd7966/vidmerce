package view

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	chplatform "github.com/mhd7966/vidmerce/internal/platform/clickhouse"
	"github.com/mhd7966/vidmerce/internal/platform/metrics"
)

// Sink is the interface the worker uses to write a batch of view events into
// the analytics store. Defined here so the worker can be unit-tested against
// an in-memory fake without standing up a ClickHouse container.
type Sink interface {
	Insert(ctx context.Context, batch []Event) error
}

// Worker drains stream:views and flushes events to the Sink in batches.
//
// Why batching: ClickHouse INSERT performance is approximately linear in
// *number of INSERT statements*, not number of rows. A 10k-row INSERT costs
// almost the same as a 1-row INSERT, so streaming row-by-row would be a
// pathological workload. We batch by both size (FlushSize) and time
// (FlushInterval) so latency stays bounded even under low traffic.
//
// Crash recovery: we XACK only after the ClickHouse INSERT succeeds. A
// process crash mid-batch leaves the messages in the PEL (Pending Entry List)
// and they'll be redelivered to whichever consumer reclaims them. Because
// INSERT INTO video_views VALUES (...) is idempotent in terms of *time-series
// shape* (and ClickHouse aggregates by (video_id, day) anyway), a duplicate
// row only marginally inflates the raw event count by the number of in-flight
// messages at crash time — typically <1k, negligible vs daily view volumes.
type Worker struct {
	rdb           *goredis.Client
	sink          Sink
	log           *slog.Logger
	group         string
	consumer      string
	flushSize     int
	flushInterval time.Duration
	readBatch     int64
}

// WorkerConfig holds parameters for the worker loop. Zero fields fall back to
// sensible defaults.
type WorkerConfig struct {
	Group         string
	Consumer      string
	FlushSize     int           // flush when N events accumulated
	FlushInterval time.Duration // flush when this much time has passed since the last flush
	ReadBatch     int           // XREADGROUP COUNT per call
}

// NewWorker constructs a view worker.
func NewWorker(rdb *goredis.Client, sink Sink, log *slog.Logger, cfg WorkerConfig) *Worker {
	if cfg.Group == "" {
		cfg.Group = "vidmerce-workers"
	}
	if cfg.Consumer == "" {
		cfg.Consumer = "worker-1"
	}
	if cfg.FlushSize <= 0 {
		cfg.FlushSize = 500
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.ReadBatch <= 0 {
		cfg.ReadBatch = 200
	}
	return &Worker{
		rdb:           rdb,
		sink:          sink,
		log:           log,
		group:         cfg.Group,
		consumer:      cfg.Consumer,
		flushSize:     cfg.FlushSize,
		flushInterval: cfg.FlushInterval,
		readBatch:     int64(cfg.ReadBatch),
	}
}

// Run starts the consume loop and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.ensureGroup(ctx); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	w.log.Info("view worker started",
		slog.String("stream", streamKey),
		slog.String("group", w.group),
		slog.String("consumer", w.consumer),
		slog.Int("flush_size", w.flushSize),
		slog.Duration("flush_interval", w.flushInterval),
	)

	// Block on XREADGROUP for at most this long; on timeout we'll drop out
	// and flush any partial batch even if we didn't hit flushSize.
	const blockTimeout = 200 * time.Millisecond

	var (
		batch     = make([]Event, 0, w.flushSize)
		batchIDs  = make([]string, 0, w.flushSize)
		lastFlush = time.Now()
	)

	for {
		if ctx.Err() != nil {
			// Best-effort final flush before exit. Anything still in PEL will be
			// reclaimed on next startup.
			w.flush(context.Background(), &batch, &batchIDs)
			return nil
		}

		entries, err := w.rdb.XReadGroup(ctx, &goredis.XReadGroupArgs{
			Group:    w.group,
			Consumer: w.consumer,
			Streams:  []string{streamKey, ">"},
			Count:    w.readBatch,
			Block:    blockTimeout,
			NoAck:    false,
		}).Result()
		if err != nil {
			if errors.Is(err, goredis.Nil) || errors.Is(err, context.DeadlineExceeded) {
				// no new messages — fall through to the time-based flush check
			} else if errors.Is(err, context.Canceled) {
				w.flush(context.Background(), &batch, &batchIDs)
				return nil
			} else {
				w.log.Error("xreadgroup failed; backing off",
					slog.Any("error", err))
				time.Sleep(time.Second)
				continue
			}
		}

		for _, s := range entries {
			for _, m := range s.Messages {
				ev, ok := parseEvent(m)
				if !ok {
					w.log.Warn("dropping malformed view stream entry",
						slog.String("id", m.ID),
						slog.Any("values", m.Values))
					_ = w.rdb.XAck(ctx, streamKey, w.group, m.ID).Err()
					continue
				}
				batch = append(batch, ev)
				batchIDs = append(batchIDs, m.ID)
			}
		}

		// Flush triggers: size hit, or interval elapsed and the batch is non-empty.
		if len(batch) >= w.flushSize || (len(batch) > 0 && time.Since(lastFlush) >= w.flushInterval) {
			w.flush(ctx, &batch, &batchIDs)
			lastFlush = time.Now()
		}
	}
}

func (w *Worker) ensureGroup(ctx context.Context) error {
	err := w.rdb.XGroupCreateMkStream(ctx, streamKey, w.group, "$").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// flush writes the in-memory batch to ClickHouse and, on success, XACKs every
// entry. On error the batch is preserved (not cleared) so the next loop
// iteration retries. The PEL ensures redelivery even across crashes.
func (w *Worker) flush(ctx context.Context, batchPtr *[]Event, idsPtr *[]string) {
	batch := *batchPtr
	ids := *idsPtr
	if len(batch) == 0 {
		return
	}

	start := time.Now()
	if err := w.sink.Insert(ctx, batch); err != nil {
		metrics.RecordViewWorkerFlush(len(batch), time.Since(start), err)
		w.log.Error("view sink insert failed; will retry",
			slog.Int("size", len(batch)),
			slog.Any("error", err))
		return
	}
	metrics.RecordViewWorkerFlush(len(batch), time.Since(start), nil)
	// ACK in one round-trip if the client supports it (go-redis XAck takes a
	// variadic id list, so a single call is enough).
	if err := w.rdb.XAck(ctx, streamKey, w.group, ids...).Err(); err != nil {
		w.log.Warn("xack failed after successful insert",
			slog.Int("size", len(batch)),
			slog.Any("error", err))
	}

	w.log.Debug("view batch flushed",
		slog.Int("size", len(batch)),
		slog.Duration("duration", time.Since(start)),
	)
	*batchPtr = batch[:0]
	*idsPtr = ids[:0]
}

// parseEvent extracts a typed Event from a Redis Stream message. Returns
// ok=false on any field we can't parse so the worker can drop the message
// instead of retrying forever.
func parseEvent(m goredis.XMessage) (Event, bool) {
	vidStr, _ := m.Values["vid"].(string)
	vid, err := uuid.Parse(vidStr)
	if err != nil {
		return Event{}, false
	}

	ev := Event{StreamID: m.ID, VideoID: vid}

	if uidStr, ok := m.Values["uid"].(string); ok && uidStr != "" {
		if uid, err := uuid.Parse(uidStr); err == nil {
			ev.ViewerID = &uid
		}
	}

	ipHash, _ := m.Values["ip"].(string)
	uaHash, _ := m.Values["ua"].(string)
	country, _ := m.Values["c"].(string)
	uStr, _ := m.Values["u"].(string)
	tStr, _ := m.Values["t"].(string)

	if len(ipHash) != 32 || len(uaHash) != 16 {
		return Event{}, false
	}
	ev.IPHash = ipHash
	ev.UAHash = uaHash
	ev.Country = country
	ev.IsUnique = uStr == "1"
	if t, err := strconv.ParseInt(tStr, 10, 64); err == nil {
		ev.EventTime = t
	} else {
		return Event{}, false
	}
	return ev, true
}

// --- ClickHouse sink ---------------------------------------------------------

// ClickHouseSink is the production Sink. It uses the ClickHouse Go driver's
// PrepareBatch API, which streams rows over the native protocol and is the
// recommended path for high-throughput inserts.
type ClickHouseSink struct {
	conn chplatform.Conn
	log  *slog.Logger
}

// NewClickHouseSink builds the sink.
func NewClickHouseSink(conn chplatform.Conn, log *slog.Logger) *ClickHouseSink {
	return &ClickHouseSink{conn: conn, log: log}
}

// Insert writes the batch using PrepareBatch + Append per row + Send. If any
// row fails, the whole batch errors out — the worker treats this as "didn't
// land, don't ACK". ClickHouse INSERTs are typically all-or-nothing at this
// API level so partial-batch corruption isn't a concern.
func (s *ClickHouseSink) Insert(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	const q = `INSERT INTO video_views (event_time, video_id, viewer_id, ip_hash, country, ua_hash, is_unique)`
	batch, err := s.conn.PrepareBatch(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, e := range events {
		eventTime := time.UnixMilli(e.EventTime).UTC()
		var viewerID *uuid.UUID
		if e.ViewerID != nil {
			viewerID = e.ViewerID
		}
		var unique uint8
		if e.IsUnique {
			unique = 1
		}
		country := e.Country
		if country == "" {
			country = "ZZ" // ISO-3166 user-assigned "unknown"
		}
		if err := batch.Append(eventTime, e.VideoID, viewerID, e.IPHash, country, e.UAHash, unique); err != nil {
			return fmt.Errorf("append row: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send batch: %w", err)
	}
	return nil
}
