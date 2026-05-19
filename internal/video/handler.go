package video

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
	platformjwt "github.com/mhd7966/vidmerce/internal/platform/jwt"
)

// Handler exposes the HTTP surface for video resources. Constructed once at
// app start via NewHandler and mounted in the composition root.
type Handler struct{ svc *Service }

// NewHandler wires the handler with its service.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// createRequest is the wire DTO; the service consumes the domain CreateInput.
type createRequest struct {
	Title        string `json:"title"         binding:"required,min=1,max=200"`
	Description  string `json:"description"   binding:"max=5000"`
	VideoURL     string `json:"video_url"     binding:"required,url,max=2048"`
	DurationSec  int    `json:"duration_sec"  binding:"required,min=1,max=3600"`
}

// Create handles POST /videos. Auth-required (mounted under the auth group in
// the composition root); the user id comes from the verified JWT.
func (h *Handler) Create(c *gin.Context) {
	uid := platformjwt.UserIDFrom(c)

	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrorDetail(c, http.StatusBadRequest, httpx.CodeValidationFailed,
			"invalid request body", gin.H{"error": err.Error()})
		return
	}

	v, err := h.svc.Create(c.Request.Context(), uid, CreateInput{
		Title:        req.Title,
		Description:  req.Description,
		VideoURL:     req.VideoURL,
		DurationSec:  req.DurationSec,
	})
	var vErr *ValidationError
	switch {
	case errors.As(err, &vErr):
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, vErr.Msg)
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "create video failed")
		return
	}
	httpx.Created(c, v)
}

// Get handles GET /videos/:id. Public.
func (h *Handler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid video id")
		return
	}
	v, err := h.svc.Get(c.Request.Context(), id)
	switch {
	case errors.Is(err, ErrVideoNotFound):
		httpx.Error(c, http.StatusNotFound, httpx.CodeNotFound, "video not found")
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "get video failed")
		return
	}
	httpx.OK(c, v)
}
