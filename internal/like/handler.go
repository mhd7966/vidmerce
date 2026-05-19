package like

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
	platformjwt "github.com/mhd7966/vidmerce/internal/platform/jwt"
)

// Handler exposes the like HTTP surface. Both endpoints are write-paths and
// must sit behind the auth middleware in the composition root.
type Handler struct{ svc *Service }

// NewHandler builds the handler. `svc` is the like service.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Like handles POST /videos/:id/like.
//
// We return 202 Accepted (not 200) to surface the truth that the persistence
// to Postgres happens asynchronously. The body carries the immediate Redis
// state so the client can render the like as taken without waiting.
func (h *Handler) Like(c *gin.Context) {
	uid := platformjwt.UserIDFrom(c)
	vid, ok := parseVideoID(c)
	if !ok {
		return
	}
	st, err := h.svc.Like(c.Request.Context(), uid, vid)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "like failed")
		return
	}
	httpx.Accepted(c, st)
}

// Unlike handles POST /videos/:id/unlike.
func (h *Handler) Unlike(c *gin.Context) {
	uid := platformjwt.UserIDFrom(c)
	vid, ok := parseVideoID(c)
	if !ok {
		return
	}
	st, err := h.svc.Unlike(c.Request.Context(), uid, vid)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "unlike failed")
		return
	}
	httpx.Accepted(c, st)
}

// parseVideoID extracts and validates the :id route param. Writes a 400 and
// returns ok=false if the param is malformed.
func parseVideoID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid video id")
		return uuid.Nil, false
	}
	return id, true
}
