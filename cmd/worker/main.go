// Command worker is the background processor for the Vidmerce platform. It
// owns the Redis Stream consumers (likes, views) and the periodic reconcilers
// that keep denormalised counters honest.
//
// The worker is intentionally separate from cmd/api so each can scale
// independently: API replicas scale with read traffic, worker replicas with
// write throughput. They share configuration and connect to the same
// Postgres / Redis / ClickHouse backends.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mhd7966/vidmerce/internal/like"
	chplatform "github.com/mhd7966/vidmerce/internal/platform/clickhouse"
	"github.com/mhd7966/vidmerce/internal/platform/config"
	"github.com/mhd7966/vidmerce/internal/platform/db"
	"github.com/mhd7966/vidmerce/internal/platform/logger"
	"github.com/mhd7966/vidmerce/internal/platform/metrics"
	"github.com/mhd7966/vidmerce/internal/platform/redis"
	"github.com/mhd7966/vidmerce/internal/view"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(".env")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build only the dependencies the worker actually needs. Worker mode is
	// HTTP-free; we still need all three datastores because:
	//   - Postgres: like worker writes here
	//   - Redis: source of all streams
	//   - ClickHouse: view worker batch sink
	pg, err := db.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pg.Close()

	rdb, err := redis.NewClient(ctx, cfg.Redis)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer func() { _ = rdb.Close() }()

	chConn, err := chplatform.NewConn(ctx, cfg.ClickHouse)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer func() { _ = chConn.Close() }()

	// --- Like pipeline -------------------------------------------------------
	likeRepo := like.NewPostgresRepository(pg)
	likeWorker := like.NewWorker(rdb, likeRepo, log, like.WorkerConfig{
		Group:        cfg.Worker.ConsumerGroup,
		Consumer:     cfg.Worker.Name,
		BatchSize:    cfg.Worker.BatchSize,
		BlockTimeout: cfg.Worker.BatchTimeout,
	})
	likeReconciler := like.NewReconciler(likeRepo, log, like.ReconcilerConfig{
		Interval:   cfg.Like.ReconcilerInterval,
		SampleSize: cfg.Like.ReconcilerSampleSize,
	})

	// --- View pipeline -------------------------------------------------------
	viewSink := view.NewClickHouseSink(chConn, log)
	viewWorker := view.NewWorker(rdb, viewSink, log, view.WorkerConfig{
		Group:         cfg.Worker.ConsumerGroup,
		Consumer:      cfg.Worker.Name,
		FlushSize:     cfg.Worker.BatchSize,
		FlushInterval: cfg.Worker.BatchTimeout,
	})

	if cfg.Metrics.Enabled {
		go metrics.RunRedisCollector(ctx, rdb, cfg.Worker.ConsumerGroup, cfg.Metrics.RedisPollInterval, log)
	}

	// One stop channel; if any goroutine exits unexpectedly we tear everything
	// down rather than running half-broken.
	var wg sync.WaitGroup
	startGoroutine := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("worker goroutine exited", slog.String("name", name), slog.Any("error", err))
				cancel()
			}
		}()
	}
	startGoroutine("like_worker", likeWorker.Run)
	startGoroutine("like_reconciler", likeReconciler.Run)
	startGoroutine("view_worker", viewWorker.Run)
	if cfg.Metrics.Enabled {
		// Metrics listen failure must not stop stream consumers (demo often has :9091 taken).
		metricsSrv := metrics.NewServer(fmt.Sprintf(":%d", cfg.Metrics.WorkerPort), log)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := metricsSrv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("worker metrics server stopped", slog.Any("error", err))
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
		log.Info("worker context cancelled; draining")
	case sig := <-quit:
		log.Info("worker shutdown signal", slog.String("signal", sig.String()))
		cancel()
	}

	// Give consumers up to 10s to finish in-flight work before forcing exit.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		log.Info("worker stopped cleanly")
	case <-time.After(10 * time.Second):
		log.Warn("worker shutdown timeout; forcing exit")
	}
	return nil
}
