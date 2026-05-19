// Package product owns the products table in Postgres and exposes the HTTP
// surface for creating and reading products. A product is 1:1 with a video
// (enforced by a UNIQUE constraint on video_id at the schema level), so the
// API is intentionally narrow:
//
//	POST /products             create a product attached to a video the caller owns
//	GET  /products/:id         read a product by its own id
//	GET  /videos/:id/product   read the product attached to a video (or 404)
package product

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Domain errors.
var (
	ErrProductNotFound   = errors.New("product not found")
	ErrVideoAlreadyTaken = errors.New("a product already exists for this video")
)

// ValidationError surfaces a user-visible reason a request was rejected.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string        { return e.Msg }
func (e *ValidationError) Is(target error) bool { _, ok := target.(*ValidationError); return ok }

// Product is the canonical product record. Price is stored as integer cents
// + ISO currency code to avoid binary-float pitfalls.
type Product struct {
	ID         uuid.UUID `json:"id"`
	VideoID    uuid.UUID `json:"video_id"`
	Name       string    `json:"name"`
	PriceCents int64     `json:"price_cents"`
	Currency   string    `json:"currency"`
	ImageURL   string    `json:"image_url"`
	CreatedAt  time.Time `json:"created_at"`
}

// CreateInput is the service-layer create payload.
type CreateInput struct {
	VideoID    uuid.UUID
	Name       string
	PriceCents int64
	Currency   string
	ImageURL   string
}

// Validate runs cheap, format-only checks before we touch the database.
func (in CreateInput) Validate() error {
	if in.VideoID == uuid.Nil {
		return &ValidationError{Msg: "video_id is required"}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return &ValidationError{Msg: "name is required"}
	}
	if len(name) > 200 {
		return &ValidationError{Msg: "name must be at most 200 characters"}
	}
	if in.PriceCents < 0 {
		return &ValidationError{Msg: "price_cents must be non-negative"}
	}
	if in.PriceCents > 1_000_000_000 { // $10M sanity ceiling
		return &ValidationError{Msg: "price_cents exceeds maximum"}
	}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(cur) != 3 {
		return &ValidationError{Msg: "currency must be a 3-letter ISO 4217 code"}
	}
	if strings.TrimSpace(in.ImageURL) == "" {
		return &ValidationError{Msg: "image_url is required"}
	}
	u, err := url.Parse(in.ImageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &ValidationError{Msg: "image_url must be a valid http(s) URL"}
	}
	if len(in.ImageURL) > 2048 {
		return &ValidationError{Msg: "image_url must be at most 2048 characters"}
	}
	return nil
}
