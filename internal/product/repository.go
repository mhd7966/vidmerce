package product

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mhd7966/vidmerce/internal/platform/db"
)

// Repository abstracts product storage. Consumer-side interface so the
// service is unit-testable with an in-memory fake.
type Repository interface {
	Create(ctx context.Context, in CreateInput) (Product, error)
	FindByID(ctx context.Context, id uuid.UUID) (Product, error)
	FindByVideoID(ctx context.Context, videoID uuid.UUID) (Product, error)
}

// pgRepo is the Postgres implementation.
type pgRepo struct{ pool *db.Pool }

// NewPostgresRepository constructs a repository bound to the given pgx pool.
func NewPostgresRepository(pool *db.Pool) Repository { return &pgRepo{pool: pool} }

const pgUniqueViolation = "23505"

func (r *pgRepo) Create(ctx context.Context, in CreateInput) (Product, error) {
	const q = `
		INSERT INTO products (video_id, name, price_cents, currency, image_url)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, video_id, name, price_cents, currency, image_url, created_at
	`
	var p Product
	err := r.pool.QueryRow(ctx, q,
		in.VideoID,
		strings.TrimSpace(in.Name),
		in.PriceCents,
		strings.ToUpper(strings.TrimSpace(in.Currency)),
		strings.TrimSpace(in.ImageURL),
	).Scan(&p.ID, &p.VideoID, &p.Name, &p.PriceCents, &p.Currency, &p.ImageURL, &p.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return Product{}, ErrVideoAlreadyTaken
		}
		return Product{}, fmt.Errorf("insert product: %w", err)
	}
	return p, nil
}

func (r *pgRepo) FindByID(ctx context.Context, id uuid.UUID) (Product, error) {
	const q = `
		SELECT id, video_id, name, price_cents, currency, image_url, created_at
		FROM products
		WHERE id = $1
	`
	var p Product
	err := r.pool.QueryRow(ctx, q, id).
		Scan(&p.ID, &p.VideoID, &p.Name, &p.PriceCents, &p.Currency, &p.ImageURL, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ErrProductNotFound
	}
	if err != nil {
		return Product{}, fmt.Errorf("select product: %w", err)
	}
	return p, nil
}

func (r *pgRepo) FindByVideoID(ctx context.Context, videoID uuid.UUID) (Product, error) {
	const q = `
		SELECT id, video_id, name, price_cents, currency, image_url, created_at
		FROM products
		WHERE video_id = $1
	`
	var p Product
	err := r.pool.QueryRow(ctx, q, videoID).
		Scan(&p.ID, &p.VideoID, &p.Name, &p.PriceCents, &p.Currency, &p.ImageURL, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ErrProductNotFound
	}
	if err != nil {
		return Product{}, fmt.Errorf("select product by video: %w", err)
	}
	return p, nil
}
