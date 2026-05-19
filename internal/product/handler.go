package product

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
	platformjwt "github.com/mhd7966/vidmerce/internal/platform/jwt"
	"github.com/mhd7966/vidmerce/internal/video"
)

// Handler renders the HTTP surface of the product service.
type Handler struct{ svc *Service }

// NewHandler wires the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// createRequest is the wire DTO.
type createRequest struct {
	VideoID    uuid.UUID `json:"video_id"    binding:"required"`
	Name       string    `json:"name"        binding:"required,min=1,max=200"`
	PriceCents int64     `json:"price_cents" binding:"gte=0"`
	Currency   string    `json:"currency"    binding:"required,len=3"`
	ImageURL   string    `json:"image_url"   binding:"required,url,max=2048"`
}

// Create handles POST /products. Auth required; the caller must own the video.
func (h *Handler) Create(c *gin.Context) {
	uid := platformjwt.UserIDFrom(c)

	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrorDetail(c, http.StatusBadRequest, httpx.CodeValidationFailed,
			"invalid request body", gin.H{"error": err.Error()})
		return
	}

	p, err := h.svc.Create(c.Request.Context(), uid, CreateInput{
		VideoID:    req.VideoID,
		Name:       req.Name,
		PriceCents: req.PriceCents,
		Currency:   req.Currency,
		ImageURL:   req.ImageURL,
	})
	var vErr *ValidationError
	switch {
	case errors.As(err, &vErr):
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, vErr.Msg)
		return
	case errors.Is(err, video.ErrVideoNotFound):
		httpx.Error(c, http.StatusNotFound, httpx.CodeNotFound, "video not found")
		return
	case errors.Is(err, video.ErrForbidden):
		httpx.Error(c, http.StatusForbidden, httpx.CodeForbidden,
			"you do not own the linked video")
		return
	case errors.Is(err, ErrVideoAlreadyTaken):
		httpx.Error(c, http.StatusConflict, httpx.CodeConflict,
			"a product already exists for this video")
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "create product failed")
		return
	}
	httpx.Created(c, p)
}

// Get handles GET /products/:id. Public.
func (h *Handler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid product id")
		return
	}
	p, err := h.svc.GetByID(c.Request.Context(), id)
	switch {
	case errors.Is(err, ErrProductNotFound):
		httpx.Error(c, http.StatusNotFound, httpx.CodeNotFound, "product not found")
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "get product failed")
		return
	}
	httpx.OK(c, p)
}

// GetByVideoID handles GET /videos/:id/product. Public.
func (h *Handler) GetByVideoID(c *gin.Context) {
	videoID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid video id")
		return
	}
	p, err := h.svc.GetByVideoID(c.Request.Context(), videoID)
	switch {
	case errors.Is(err, ErrProductNotFound):
		httpx.Error(c, http.StatusNotFound, httpx.CodeNotFound, "no product attached to this video")
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "get product failed")
		return
	}
	httpx.OK(c, p)
}
