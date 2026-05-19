// Package app is the composition root for the Vidmerce API. The Application
// struct owns every long-lived resource — config, logger, datastores, router,
// HTTP server — and is built once at process start.
//
// Handlers in feature packages receive the dependencies they need via
// constructor injection (NewHandler(log, repo, cache, ...)). The router build
// step below is the single place where those constructors are called, which
// keeps the dependency graph visible and grep-able in one file.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mhd7966/vidmerce/internal/auth"
	"github.com/mhd7966/vidmerce/internal/feed"
	"github.com/mhd7966/vidmerce/internal/health"
	"github.com/mhd7966/vidmerce/internal/like"
	"github.com/mhd7966/vidmerce/internal/platform/bloom"
	"github.com/mhd7966/vidmerce/internal/platform/cache"
	"github.com/mhd7966/vidmerce/internal/platform/clickhouse"
	"github.com/mhd7966/vidmerce/internal/platform/config"
	"github.com/mhd7966/vidmerce/internal/platform/db"
	"github.com/mhd7966/vidmerce/internal/platform/httpx"
	platformjwt "github.com/mhd7966/vidmerce/internal/platform/jwt"
	"github.com/mhd7966/vidmerce/internal/platform/logger"
	"github.com/mhd7966/vidmerce/internal/platform/metrics"
	"github.com/mhd7966/vidmerce/internal/platform/ratelimit"
	"github.com/mhd7966/vidmerce/internal/platform/redis"
	"github.com/mhd7966/vidmerce/internal/platform/swagger"
	"github.com/mhd7966/vidmerce/internal/product"
	"github.com/mhd7966/vidmerce/internal/stats"
	"github.com/mhd7966/vidmerce/internal/video"
	"github.com/mhd7966/vidmerce/internal/view"
)

// Application is the wired-up runtime. It is intentionally a plain struct
// with exported fields so tests can substitute fakes (e.g. a stub redis.Client
// or a test HTTP server) without going through the New() constructor.
type Application struct {
	Config config.Config
	Log    *slog.Logger

	Postgres   *db.Pool
	Redis      *redis.Client
	ClickHouse clickhouse.Conn

	// Cross-cutting services.
	JWT         *platformjwt.Service
	RateLimiter *ratelimit.LeakyBucket

	// Feature services. Each feature package owns a Service/Handler pair built
	// here at startup and consumed by buildRouter().
	AuthService    *auth.Service
	VideoService   *video.Service
	ProductService *product.Service
	FeedFetcher    feed.Fetcher
	LikeService    *like.Service
	ViewService    *view.Service
	StatsService   *stats.Service

	Router     *gin.Engine
	HTTPServer *http.Server
}

// New constructs an Application by wiring config, logger, datastores, and the
// HTTP router/server. Returns an error if any resource fails to start, after
// rolling back anything that had already been opened (no half-initialised
// state escapes from this function).
func New(ctx context.Context, cfg config.Config) (*Application, error) {
	log := logger.New(cfg.Log.Level, cfg.Log.Format)

	if cfg.AppEnv == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	a := &Application{Config: cfg, Log: log}

	pg, err := db.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	a.Postgres = pg

	rdb, err := redis.NewClient(ctx, cfg.Redis)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	a.Redis = rdb

	ch, err := clickhouse.NewConn(ctx, cfg.ClickHouse)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	a.ClickHouse = ch

	// Cross-cutting services built once and shared.
	a.JWT = platformjwt.NewService(cfg.JWT.Secret, cfg.JWT.AccessTTL, "vidmerce")
	a.RateLimiter = ratelimit.New(a.Redis)

	// Feature services. Repositories are constructed here (the composition
	// root) and passed in. The service has no idea whether it's talking to
	// Postgres, an in-memory fake, or a mock.
	userRepo := auth.NewPostgresUserRepository(a.Postgres)
	refreshStore := auth.NewRedisRefreshStore(a.Redis)

	var emailBloom, productVideoBloom bloom.Filter
	if cfg.Bloom.Enabled {
		emailBF := bloom.NewRedisFilter(a.Redis, bloom.KeyEmails,
			cfg.Bloom.ErrorRate, cfg.Bloom.EmailCapacity)
		if err := bloom.WarmupEmails(ctx, a.Postgres, emailBF); err != nil {
			a.Close()
			return nil, fmt.Errorf("bloom warmup emails: %w", err)
		}
		emailBloom = emailBF

		productBF := bloom.NewRedisFilter(a.Redis, bloom.KeyProductVideos,
			cfg.Bloom.ErrorRate, cfg.Bloom.ProductCapacity)
		if err := bloom.WarmupProductVideos(ctx, a.Postgres, productBF); err != nil {
			a.Close()
			return nil, fmt.Errorf("bloom warmup product videos: %w", err)
		}
		productVideoBloom = productBF
		log.Info("bloom filters warmed",
			slog.String("emails_key", bloom.KeyEmails),
			slog.String("products_key", bloom.KeyProductVideos),
		)
	}

	a.AuthService = auth.NewService(userRepo, refreshStore, a.JWT, a.Log,
		cfg.BcryptCost, cfg.JWT.RefreshTTL, emailBloom)

	// Video service and its read-through cache. Same TTL on all per-resource
	// caches for now; can be tuned per-endpoint later.
	const resourceCacheTTL = 60 * time.Second
	videoRepo := video.NewPostgresRepository(a.Postgres)
	videoCache := cache.New[video.Video](a.Redis,
		func(id string) string { return "video:" + id }, resourceCacheTTL,
	)

	// Feed fetcher is decided here, at the composition root, based on config.
	// In push mode we also need to register a post-create hook on the video
	// service so newly created videos fan out into the global ZSET. Because
	// of that ordering dependency we construct the fetcher *before* the video
	// service, then plug it in via the WithOnCreate option.
	var feedOpts []video.Option
	switch cfg.Feed.Mode {
	case config.FeedModePush:
		pushF := feed.NewPushFetcher(a.Redis, videoRepo, cfg.Feed.PushZSetCap, a.Log)
		if err := pushF.Warmup(ctx); err != nil {
			a.Close()
			return nil, fmt.Errorf("feed warmup: %w", err)
		}
		a.FeedFetcher = pushF
		feedOpts = append(feedOpts, video.WithOnCreate(pushF.Publish))
	default: // pull
		a.FeedFetcher = feed.NewPullFetcher(videoRepo)
	}
	viewDurStore := view.NewRedisDurationStore(a.Redis, cfg.View.DurationCacheTTL)
	a.VideoService = video.NewService(videoRepo, videoCache, viewDurStore, a.Log, feedOpts...)

	// Product service. It depends on the video service for the ownership
	// check on POST /products — the dependency direction is product -> video,
	// not the other way around, so no cyclic import is possible.
	productRepo := product.NewPostgresRepository(a.Postgres)
	productByIDCache := cache.New[product.Product](a.Redis,
		func(id string) string { return "product:" + id }, resourceCacheTTL,
	)
	productByVideoCache := cache.New[product.Product](a.Redis,
		func(id string) string { return "video:" + id + ":product" }, resourceCacheTTL,
	)
	a.ProductService = product.NewService(
		productRepo, productByIDCache, productByVideoCache, a.VideoService, a.Log,
		productVideoBloom,
	)

	// Like service. The API side only needs the Redis ops; the worker (built
	// in cmd/worker) is the only consumer of the Postgres repo from the
	// stream:likes side. We still pass a repo here so the count cache can
	// repopulate from the source of truth on miss.
	likeRepo := like.NewPostgresRepository(a.Postgres)
	a.LikeService = like.NewService(a.Redis, likeRepo, a.Log)

	// View pipeline: watch ≥⅓ duration, then duration-based rate cap (60/T per
	// min). Duration comes from Redis only on POST /view — no Postgres.
	viewPolicy := view.ViewPolicyConfig{
		MinDurationSec:    cfg.View.MinDurationSec,
		UnknownMinWatchMs: cfg.View.UnknownMinWatchMs,
	}
	viewChain := view.NewChain(a.Log,
		view.NewWatchThresholdFilter(viewPolicy),
		view.NewDurationRateFilter(a.RateLimiter, viewPolicy, a.Log, true /* failOpen */),
	)
	viewUnique := view.NewUniqueMarker(a.Redis, cfg.View.UniqueTTL)
	a.ViewService = view.NewService(a.Redis, viewChain, viewDurStore, viewUnique, a.Log)

	// Stats service. The two read repos (CH views, PG likes) are constructed
	// here at the composition root; the service depends on small interfaces
	// (ViewsCounter, LikesCounter, VideoExister) that those concrete types
	// satisfy implicitly — keeps the stats package decoupled from CH/PG.
	statsViewsRepo := stats.NewClickHouseViewsRepo(a.ClickHouse)
	a.StatsService = stats.NewService(
		statsViewsRepo,
		likeRepo, // already constructed for the like service above
		a.VideoService,
		a.Redis,
		a.Log,
		stats.Config{
			CacheTTL:  cfg.Stats.CacheTTL,
			LockTTL:   cfg.Stats.LockTTL,
			LockRetry: cfg.Stats.LockRetry,
		},
	)

	a.Router = a.buildRouter()
	a.HTTPServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTP.Port),
		Handler:      a.Router,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
	}
	return a, nil
}

// buildRouter is the single place where handler constructors are called and
// routes are mounted. New feature packages added in later steps wire their
// handlers here.
func (a *Application) buildRouter() *gin.Engine {
	r := gin.New()
	if a.Config.Metrics.Enabled {
		r.Use(metrics.Prometheus())
	}
	r.Use(
		httpx.RequestID(),
		httpx.AccessLog(a.Log),
		httpx.Recover(a.Log),
	)

	if a.Config.Metrics.Enabled {
		r.GET(a.Config.Metrics.Path, gin.WrapH(promhttp.Handler()))
	}

	// Health / readiness. The handler receives only what it needs — config and
	// a map of Pinger-shaped dependencies — not the full Application. Each
	// datastore exposes a slightly different Ping signature, so we adapt them
	// to the uniform Pinger contract right here, where the wiring lives.
	healthH := health.NewHandler(a.Config, map[string]health.Pinger{
		"postgres":   health.PingerFunc(a.Postgres.Ping),
		"redis":      health.PingerFunc(func(ctx context.Context) error { return a.Redis.Ping(ctx).Err() }),
		"clickhouse": health.PingerFunc(a.ClickHouse.Ping),
	})
	r.GET("/health", healthH.Health)
	r.GET("/ready", healthH.Ready)

	swagger.Register(r)

	// ---- Auth ---------------------------------------------------------------
	// Login carries a per-IP leaky-bucket rate limit so a brute-force attacker
	// can't grind through password attempts from one machine. We fail closed
	// (return 503 when Redis is down) because letting unlimited login attempts
	// through is worse than briefly refusing the route.
	loginRL := ratelimit.Middleware(ratelimit.MiddlewareConfig{
		Bucket: a.RateLimiter,
		Policy: ratelimit.Policy{
			Capacity:      10, // burst of 10 attempts
			LeakPerSecond: 1.0 / 6.0, // ~10 attempts per minute steady state
			TTL:           time.Hour,
		},
		KeyFunc:     func(c *gin.Context) string { return "bucket:login:" + c.ClientIP() },
		Logger:      a.Log,
		FailOpen:    false,
		MetricLabel: "login",
	})

	authH := auth.NewHandler(a.AuthService)
	authGroup := r.Group("/auth")
	{
		authGroup.POST("/register", authH.Register)
		authGroup.POST("/login", loginRL, authH.Login)
		authGroup.POST("/refresh", authH.Refresh)
		authGroup.POST("/logout", authH.Logout)
	}

	// ---- Public + protected resource routes --------------------------------
	// The protected group sits behind the auth middleware so anything mounted
	// on it can rely on platformjwt.UserIDFrom(c) returning a valid uuid.
	videoH := video.NewHandler(a.VideoService)
	productH := product.NewHandler(a.ProductService)

	// Public reads.
	r.GET("/videos/:id", videoH.Get)
	r.GET("/videos/:id/product", productH.GetByVideoID)
	r.GET("/products/:id", productH.Get)

	// Feed (public).
	feedH := feed.NewHandler(a.FeedFetcher, a.Config.Feed.PageDefault, a.Config.Feed.PageMax)
	r.GET("/feed", feedH.Get)

	// Per-(user,video) leaky-bucket for like toggles. Generous capacity (a
	// human can plausibly tap-untap several times in a panic) but a low leak
	// rate so bots that hammer the endpoint get a 429. Fail-open: a Redis
	// outage shouldn't block users from liking videos.
	likeRL := ratelimit.Middleware(ratelimit.MiddlewareConfig{
		Bucket: a.RateLimiter,
		Policy: ratelimit.Policy{
			Capacity:      a.Config.Like.BucketCapacity,
			LeakPerSecond: float64(a.Config.Like.BucketLeakPerMin) / 60.0,
			TTL:           time.Hour,
		},
		KeyFunc: func(c *gin.Context) string {
			// We can safely call UserIDFrom here because RequireAuth runs
			// earlier in the chain (mounted at the group level below).
			return "bucket:like:" + platformjwt.UserIDFrom(c).String() + ":" + c.Param("id")
		},
		Logger:      a.Log,
		FailOpen:    true,
		MetricLabel: "like",
	})

	// Protected writes.
	likeH := like.NewHandler(a.LikeService)
	protected := r.Group("", platformjwt.RequireAuth(a.JWT))
	{
		protected.POST("/videos", videoH.Create)
		protected.POST("/products", productH.Create)
		protected.POST("/videos/:id/like", likeRL, likeH.Like)
		protected.POST("/videos/:id/unlike", likeRL, likeH.Unlike)
	}

	// View tracking: optional auth (anonymous viewers are allowed; logged-in
	// viewers get a per-user dedup key rather than IP-based, which is more
	// accurate on shared networks). The spam filter chain runs inside the
	// service, not as middleware, because it's stateful and needs the parsed
	// body — but the route itself is cheap so we don't add a rate-limit
	// middleware on top.
	viewH := view.NewHandler(a.ViewService)
	r.POST("/videos/:id/view", platformjwt.OptionalAuth(a.JWT), viewH.Track)

	// Analytics: public read-only with three-layer stampede protection
	// (Redis cache → in-process singleflight → distributed lock). See the
	// stats package documentation for the failure-mode matrix.
	statsH := stats.NewHandler(a.StatsService)
	r.GET("/videos/:id/stats", statsH.Get)

	return r
}

// Run starts the HTTP server and blocks until either the server returns an
// error or an OS signal (SIGINT / SIGTERM) is received. On shutdown it drains
// in-flight requests up to cfg.HTTP.ShutdownTimeout and then closes resources.
func (a *Application) Run(ctx context.Context) error {
	if a.Config.Metrics.Enabled && a.Redis != nil {
		go metrics.RunRedisCollector(ctx, a.Redis, a.Config.Worker.ConsumerGroup, a.Config.Metrics.RedisPollInterval, a.Log)
	}

	serverErr := make(chan error, 1)
	go func() {
		a.Log.Info("http server starting",
			slog.String("addr", a.HTTPServer.Addr),
			slog.String("feed_mode", string(a.Config.Feed.Mode)),
		)
		if err := a.HTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	case sig := <-quit:
		a.Log.Info("shutdown signal received", slog.String("signal", sig.String()))
	case <-ctx.Done():
		a.Log.Info("context cancelled, shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.Config.HTTP.ShutdownTimeout)
	defer cancel()
	if err := a.HTTPServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	a.Log.Info("server stopped cleanly")
	return nil
}

// Close releases all datastore handles. Safe to call multiple times; safe to
// call partway through a failed New() — each field is nil-checked.
func (a *Application) Close() {
	if a == nil {
		return
	}
	if a.Postgres != nil {
		a.Postgres.Close()
	}
	if a.Redis != nil {
		_ = a.Redis.Close()
	}
	if a.ClickHouse != nil {
		_ = a.ClickHouse.Close()
	}
}
