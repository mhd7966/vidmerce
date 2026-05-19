package view

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
	platformjwt "github.com/mhd7966/vidmerce/internal/platform/jwt"
)

// Handler exposes POST /videos/:id/view. The route uses OptionalAuth — views
// are accepted for both anonymous and logged-in viewers — but logged-in
// viewers get a per-user dedup key, which is more accurate than IP-based
// dedup on shared networks.
type Handler struct{ svc *Service }

// NewHandler builds the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// trackBody is the optional JSON body. Currently only watch_ms is meaningful;
// extra fields are tolerated so clients can add them without a deploy.
type trackBody struct {
	WatchMs int    `json:"watch_ms"`
	Country string `json:"country"` // optional client hint; trusted only as a label
}

// Track handles POST /videos/:id/view. Always returns 202 — see Service.Track
// for why we don't surface "rejected" as a different status code.
func (h *Handler) Track(c *gin.Context) {
	vid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid video id")
		return
	}

	// Body is optional. We intentionally ignore parse errors instead of 400-ing
	// because beacons are typically fire-and-forget and may not carry a body.
	var body trackBody
	_ = c.ShouldBindJSON(&body)

	in := Input{
		VideoID: vid,
		IPHash:  HashIP(c.ClientIP()),
		UAHash:  HashUA(c.Request.UserAgent()),
		Country: body.Country,
		WatchMs: body.WatchMs,
	}
	if uid, ok := platformjwt.OptionalUserIDFrom(c); ok {
		uidCopy := uid
		in.ViewerID = &uidCopy
	}

	res, err := h.svc.Track(c.Request.Context(), in)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "track failed")
		return
	}
	// 202 with a body that signals whether the view was counted. Clients SHOULD
	// not branch on this; it's exposed primarily for debugging and metrics.
	httpx.Accepted(c, map[string]any{
		"accepted":    res.Accepted,
		"is_unique":   res.IsUnique,
		"rejected_by": res.RejectedBy,
	})
}
