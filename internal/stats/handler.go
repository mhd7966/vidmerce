package stats

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
)

// Handler exposes GET /videos/:id/stats. Public route — anyone can read
// engagement metrics for a video. Adding auth would only matter for *private*
// videos, which the spec doesn't include.
type Handler struct{ svc *Service }

// NewHandler builds the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Get returns the cached or freshly-computed stats for a video.
//
// Status code mapping:
//
//	200 — stats returned (engagement_rate may be 0 if no audience yet).
//	400 — :id is not a valid UUID.
//	404 — video does not exist.
//	500 — both backends (or one) failed unexpectedly; treat as transient.
func (h *Handler) Get(c *gin.Context) {
	vid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid video id")
		return
	}
	v, err := h.svc.Get(c.Request.Context(), vid)
	if err != nil {
		switch {
		case errors.Is(err, ErrVideoNotFound):
			httpx.Error(c, http.StatusNotFound, httpx.CodeNotFound, "video not found")
		default:
			httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "stats unavailable")
		}
		return
	}
	httpx.OK(c, v)
}
