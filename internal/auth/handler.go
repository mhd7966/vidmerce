package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
)

// Handler exposes the HTTP surface of the auth service. It is built once at
// app start via NewHandler and mounted onto the router in the composition root.
type Handler struct {
	svc *Service
}

// NewHandler wires the handler. The service carries all dependencies it needs.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// ---- DTOs ----

type registerRequest struct {
	Email    string `json:"email" binding:"required,email,max=254"`
	Password string `json:"password" binding:"required,min=8,max=72"`
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email,max=254"`
	Password string `json:"password" binding:"required,min=1,max=72"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// ---- handlers ----

// Register handles POST /auth/register. Returns 201 with the public user view.
func (h *Handler) Register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrorDetail(c, http.StatusBadRequest, httpx.CodeValidationFailed,
			"invalid request body", gin.H{"error": err.Error()})
		return
	}
	u, err := h.svc.Register(c.Request.Context(), req.Email, req.Password)
	var vErr *ValidationError
	switch {
	case errors.Is(err, ErrEmailTaken):
		httpx.Error(c, http.StatusConflict, httpx.CodeConflict, "email already registered")
		return
	case errors.As(err, &vErr):
		httpx.Error(c, http.StatusBadRequest, httpx.CodeValidationFailed, vErr.Msg)
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "registration failed")
		return
	}
	httpx.Created(c, u)
}

// Login handles POST /auth/login. Returns 200 with a token pair on success.
func (h *Handler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrorDetail(c, http.StatusBadRequest, httpx.CodeValidationFailed,
			"invalid request body", gin.H{"error": err.Error()})
		return
	}
	tp, err := h.svc.Login(c.Request.Context(), req.Email, req.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		httpx.Error(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "invalid email or password")
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "login failed")
		return
	}
	httpx.OK(c, tp)
}

// Refresh handles POST /auth/refresh. The old refresh token is rotated out;
// the response carries a new access + refresh pair.
func (h *Handler) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrorDetail(c, http.StatusBadRequest, httpx.CodeValidationFailed,
			"invalid request body", gin.H{"error": err.Error()})
		return
	}
	tp, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	switch {
	case errors.Is(err, ErrInvalidRefresh):
		httpx.Error(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "invalid or expired refresh token")
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "refresh failed")
		return
	}
	httpx.OK(c, tp)
}

// Logout handles POST /auth/logout. Revokes the presented refresh token.
// Idempotent — repeated calls return 200.
func (h *Handler) Logout(c *gin.Context) {
	var req logoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrorDetail(c, http.StatusBadRequest, httpx.CodeValidationFailed,
			"invalid request body", gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		httpx.Error(c, http.StatusInternalServerError, httpx.CodeInternal, "logout failed")
		return
	}
	httpx.OK(c, gin.H{"revoked": true})
}
