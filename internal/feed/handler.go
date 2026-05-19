package feed

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
)

// Handler renders the feed HTTP surface. One handler, one route:
//
//	GET /feed?cursor=<opaque>&limit=<int>
//
// The pull-vs-push decision is invisible at this level: the handler talks to
// a Fetcher interface and never knows (or cares) which implementation is wired.
type Handler struct {
	fetcher Fetcher

	pageDefault int
	pageMax     int
}

// NewHandler builds the handler. Page-size defaults and the upper bound are
// passed in from config so they're tunable per-environment without code change.
func NewHandler(f Fetcher, pageDefault, pageMax int) *Handler {
	if pageDefault <= 0 {
		pageDefault = 20
	}
	if pageMax <= 0 || pageMax < pageDefault {
		pageMax = 50
	}
	return &Handler{fetcher: f, pageDefault: pageDefault, pageMax: pageMax}
}

// Get handles GET /feed. Public.
func (h *Handler) Get(c *gin.Context) {
	cursorStr := c.Query("cursor")
	cursor, err := DecodeCursor(cursorStr)
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid cursor")
		return
	}

	limit := h.pageDefault
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed,
				"limit must be a positive integer")
			return
		}
		if n > h.pageMax {
			n = h.pageMax
		}
		limit = n
	}

	page, err := h.fetcher.Fetch(c.Request.Context(), cursor, limit)
	switch {
	case errors.Is(err, ErrInvalidCursor):
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid cursor")
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "feed query failed")
		return
	}

	httpx.OKMeta(c, page.Items, httpx.Pagination{
		NextCursor: page.NextCursor,
		Limit:      limit,
		Count:      len(page.Items),
	})
}
