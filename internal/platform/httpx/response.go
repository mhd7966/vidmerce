// Package httpx response envelope. Every JSON response the API emits — success
// or failure — wraps its payload in this single shape so clients have one
// parser to write and one set of fields to inspect. Field semantics:
//
//	code    : stable machine-readable identifier (e.g. "ok", "validation_failed")
//	message : optional human-readable explanation
//	data    : the actual payload (resource, list of resources, etc.)
//	meta    : auxiliary information (pagination cursors, request IDs, retry hints, ...)
//
// All four fields use `omitempty` so the on-wire shape stays compact when a
// field is not relevant (e.g. error responses typically have no `data`).
package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CodeOK is the canonical success code. Endpoint-specific success codes
// (e.g. "accepted") may also be used when the distinction is useful for clients.
const (
	CodeOK                = "ok"
	CodeAccepted          = "accepted"
	CodeCreated           = "created"
	CodeValidationFailed  = "validation_failed"
	CodeUnauthenticated   = "unauthenticated"
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	CodeConflict          = "conflict"
	CodeRateLimited       = "rate_limited"
	CodeInternal          = "internal_error"
	CodeServiceUnready    = "service_unready"
)

// Envelope is the universal response shape. It is the only type that handlers
// should serialise to the wire directly.
type Envelope struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
	Meta    any    `json:"meta,omitempty"`
}

// Pagination is the canonical shape for cursor pagination metadata. It is
// always nested inside `meta` so that other meta fields can coexist.
type Pagination struct {
	NextCursor string `json:"next_cursor,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Count      int    `json:"count,omitempty"`
}

// OK writes a 200 response with no meta block.
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Code: CodeOK, Data: data})
}

// OKMeta writes a 200 response with a meta block (typically pagination).
func OKMeta(c *gin.Context, data, meta any) {
	c.JSON(http.StatusOK, Envelope{Code: CodeOK, Data: data, Meta: meta})
}

// Created writes a 201 response (use after a successful resource creation).
func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, Envelope{Code: CodeCreated, Data: data})
}

// Accepted writes a 202 response (use when work has been queued asynchronously,
// e.g. likes and views).
func Accepted(c *gin.Context, data any) {
	c.JSON(http.StatusAccepted, Envelope{Code: CodeAccepted, Data: data})
}

// Error writes the canonical error envelope and aborts the handler chain.
// `code` is a stable machine code (see Code* constants); `message` is a short
// human explanation safe to surface to end users.
func Error(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, Envelope{Code: code, Message: message})
}

// ErrorDetail is Error with an additional structured payload attached under
// `meta` (per-field validation errors, retry hints, etc.). Putting detail in
// `meta` keeps the envelope shape uniform between success and failure.
func ErrorDetail(c *gin.Context, status int, code, message string, detail any) {
	c.AbortWithStatusJSON(status, Envelope{Code: code, Message: message, Meta: detail})
}
