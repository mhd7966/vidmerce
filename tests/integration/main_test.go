//go:build integration

// Package integration holds cross-package end-to-end tests that spin up real
// Postgres, Redis, and ClickHouse containers via testcontainers-go, run all
// migrations, and exercise the full async pipeline (API → Redis stream →
// worker → source-of-truth store).
//
// They are gated by the `integration` build tag and are *not* part of
// `make test-unit`. Run with `make test-integration`. The first run takes
// ~30s because Docker pulls all three images; subsequent runs reuse the
// local image cache.
package integration

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	chmod "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	pgmod "github.com/testcontainers/testcontainers-go/modules/postgres"
	rdmod "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

// suite holds the resources shared by every test in the package. We
// initialise it once in TestMain to amortise the slow container startup over
// the whole package's runtime.
type suite struct {
	pg     *pgmod.PostgresContainer
	rd     *rdmod.RedisContainer
	ch     *chmod.ClickHouseContainer
	pgDSN  string
	rdAddr string
	chHost string
	chPort string
}

var s *suite

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var err error
	s, err = newSuite(ctx)
	if err != nil {
		log.Printf("integration suite bring-up failed: %v", err)
		// Tear down any partially-started containers before we exit.
		if s != nil {
			s.shutdown(context.Background())
		}
		os.Exit(1)
	}

	code := m.Run()
	s.shutdown(context.Background())
	os.Exit(code)
}

func newSuite(ctx context.Context) (*suite, error) {
	st := &suite{}

	// --- Postgres --------------------------------------------------------
	pg, err := pgmod.Run(ctx,
		"postgres:16-alpine",
		pgmod.WithDatabase("vidmerce"),
		pgmod.WithUsername("vidmerce"),
		pgmod.WithPassword("vidmerce"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return st, fmt.Errorf("start postgres: %w", err)
	}
	st.pg = pg
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return st, fmt.Errorf("pg conn string: %w", err)
	}
	st.pgDSN = dsn

	// --- Redis -----------------------------------------------------------
	rd, err := rdmod.Run(ctx, "redis:7-alpine")
	if err != nil {
		return st, fmt.Errorf("start redis: %w", err)
	}
	st.rd = rd
	rdURL, err := rd.ConnectionString(ctx)
	if err != nil {
		return st, fmt.Errorf("rd conn string: %w", err)
	}
	parsed, err := goredis.ParseURL(rdURL)
	if err != nil {
		return st, fmt.Errorf("parse redis url: %w", err)
	}
	st.rdAddr = parsed.Addr

	// --- ClickHouse ------------------------------------------------------
	ch, err := chmod.Run(ctx,
		"clickhouse/clickhouse-server:24.3-alpine",
		chmod.WithUsername("default"),
		chmod.WithPassword(""),
		chmod.WithDatabase("vidmerce"),
	)
	if err != nil {
		return st, fmt.Errorf("start clickhouse: %w", err)
	}
	st.ch = ch
	host, err := ch.Host(ctx)
	if err != nil {
		return st, fmt.Errorf("ch host: %w", err)
	}
	port, err := ch.MappedPort(ctx, "9000/tcp")
	if err != nil {
		return st, fmt.Errorf("ch port: %w", err)
	}
	st.chHost = host
	st.chPort = port.Port()

	// --- Migrations ------------------------------------------------------
	if err := st.runMigrations(ctx); err != nil {
		return st, fmt.Errorf("run migrations: %w", err)
	}
	return st, nil
}

// runMigrations applies the on-disk SQL migration files against the freshly
// started containers. We use direct Exec (no golang-migrate dependency) to
// keep the test surface small; each migration is a single multi-statement
// SQL file that both drivers handle natively.
func (st *suite) runMigrations(ctx context.Context) error {
	rootDir := repoRoot()

	// Postgres.
	pgSQL, err := os.ReadFile(filepath.Join(rootDir, "migrations", "postgres", "0001_init.up.sql"))
	if err != nil {
		return fmt.Errorf("read pg migration: %w", err)
	}
	pool, err := pgxpool.New(ctx, st.pgDSN)
	if err != nil {
		return fmt.Errorf("pg pool: %w", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, string(pgSQL)); err != nil {
		return fmt.Errorf("apply pg migration: %w", err)
	}
	pgSQL2, err := os.ReadFile(filepath.Join(rootDir, "migrations", "postgres", "0002_video_duration.up.sql"))
	if err != nil {
		return fmt.Errorf("read pg migration 0002: %w", err)
	}
	if _, err := pool.Exec(ctx, string(pgSQL2)); err != nil {
		return fmt.Errorf("apply pg migration 0002: %w", err)
	}

	// ClickHouse. The native driver does not support multi-statement, so we
	// split the migration file on ";" at top-level. Our migrations contain
	// no string literals with embedded semicolons, so this is safe.
	chConn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{st.chHost + ":" + st.chPort},
		Auth: clickhouse.Auth{Database: "vidmerce", Username: "default"},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("clickhouse open: %w", err)
	}
	defer func() { _ = chConn.Close() }()
	if err := chConn.Ping(ctx); err != nil {
		return fmt.Errorf("clickhouse ping: %w", err)
	}
	chSQL, err := os.ReadFile(filepath.Join(rootDir, "migrations", "clickhouse", "0001_init.up.sql"))
	if err != nil {
		return fmt.Errorf("read ch migration: %w", err)
	}
	for _, stmt := range splitSQL(string(chSQL)) {
		if err := chConn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply ch stmt %q: %w", stmtSnippet(stmt), err)
		}
	}
	return nil
}

// shutdown stops every container in best-effort fashion. We deliberately use
// a fresh context (rather than the per-test one which may already be done)
// so the cleanup doesn't fail because the test's deadline elapsed.
func (st *suite) shutdown(ctx context.Context) {
	if st.pg != nil {
		_ = st.pg.Terminate(ctx)
	}
	if st.rd != nil {
		_ = st.rd.Terminate(ctx)
	}
	if st.ch != nil {
		_ = st.ch.Terminate(ctx)
	}
}

// repoRoot resolves the path of the project root from the location of this
// source file. Doing it this way means the tests run correctly regardless of
// the working directory `go test` was invoked from.
func repoRoot() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// splitSQL splits a multi-statement SQL file on `;` and drops blank/comment
// lines. Naive (does not understand string literals or dollar-quoted
// strings), but correct for the migrations in this repository.
func splitSQL(src string) []string {
	var out []string
	start := 0
	for i, r := range src {
		if r != ';' {
			continue
		}
		stmt := trimSQL(src[start:i])
		if stmt != "" {
			out = append(out, stmt)
		}
		start = i + 1
	}
	if tail := trimSQL(src[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

func trimSQL(s string) string {
	// Strip leading whitespace and `--` line comments — ClickHouse rejects
	// a no-op like `;` or `-- comment` as a statement by itself.
	for {
		s = trimLeadingWhitespace(s)
		if len(s) < 2 || s[0] != '-' || s[1] != '-' {
			break
		}
		end := indexByte(s, '\n')
		if end < 0 {
			return ""
		}
		s = s[end+1:]
	}
	return trimTrailingWhitespace(s)
}

func trimLeadingWhitespace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return s[i:]
}

func trimTrailingWhitespace(s string) string {
	i := len(s)
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\n' || s[i-1] == '\r') {
		i--
	}
	return s[:i]
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func stmtSnippet(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
