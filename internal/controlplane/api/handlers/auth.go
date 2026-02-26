package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/internal/controlplane/api/middleware"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// AuthHandler handles authentication-related API endpoints.
type AuthHandler struct {
	store      store.UserStore
	jwtService *auth.JWTService
}

// NewAuthHandler creates a new AuthHandler.
func NewAuthHandler(s store.UserStore, jwtService *auth.JWTService) *AuthHandler {
	return &AuthHandler{
		store:      s,
		jwtService: jwtService,
	}
}

// LoginRequest is the request body for POST /api/v1/auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse is the response body for POST /api/v1/auth/login.
type LoginResponse struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	TokenType    string       `json:"token_type"`
	ExpiresIn    int64        `json:"expires_in"`
	ExpiresAt    time.Time    `json:"expires_at"`
	User         UserResponse `json:"user"`
}

// UserResponse is a sanitized user representation for API responses.
type UserResponse struct {
	ID                 string   `json:"id"`
	Username           string   `json:"username"`
	DisplayName        string   `json:"display_name,omitempty"`
	Email              string   `json:"email,omitempty"`
	Role               string   `json:"role"`
	UID                *uint32  `json:"uid,omitempty"`
	Groups             []string `json:"groups,omitempty"`
	Enabled            bool     `json:"enabled"`
	MustChangePassword bool     `json:"must_change_password"`
}

// RefreshRequest is the request body for POST /api/v1/auth/refresh.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Login handles POST /api/v1/auth/login.
// Authenticates user credentials and returns a JWT token pair.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Username == "" || req.Password == "" {
		BadRequest(w, "Username and password are required")
		return
	}

	// Validate credentials
	user, err := h.store.ValidateCredentials(r.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, models.ErrInvalidCredentials) || errors.Is(err, models.ErrUserNotFound) {
			Unauthorized(w, "Invalid username or password")
			return
		}
		if errors.Is(err, models.ErrUserDisabled) {
			Forbidden(w, "User account is disabled")
			return
		}
		InternalServerError(w, "Authentication failed")
		return
	}

	// Generate token pair
	tokenPair, err := h.jwtService.GenerateTokenPair(user)
	if err != nil {
		InternalServerError(w, "Failed to generate token")
		return
	}

	// Update last login time (non-critical, log error for debugging)
	if err := h.store.UpdateLastLogin(r.Context(), user.Username, time.Now()); err != nil {
		logger.WarnCtx(r.Context(), "failed to update last login time", "username", user.Username, "error", err)
	}

	// Build response
	response := LoginResponse{
		AccessToken:  tokenPair.AccessToken,
		RefreshToken: tokenPair.RefreshToken,
		TokenType:    tokenPair.TokenType,
		ExpiresIn:    tokenPair.ExpiresIn,
		ExpiresAt:    tokenPair.ExpiresAt,
		User:         userToResponse(user),
	}

	WriteJSONOK(w, response)
}

// Refresh handles POST /api/v1/auth/refresh.
// Returns a new token pair using a valid refresh token.
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.RefreshToken == "" {
		BadRequest(w, "Refresh token is required")
		return
	}

	// Validate the refresh token
	claims, err := h.jwtService.ValidateRefreshToken(req.RefreshToken)
	if err != nil {
		if errors.Is(err, auth.ErrExpiredToken) {
			Unauthorized(w, "Refresh token has expired")
			return
		}
		Unauthorized(w, "Invalid refresh token")
		return
	}

	// Fetch fresh user data
	user, err := h.store.GetUser(r.Context(), claims.Username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			Unauthorized(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to fetch user")
		return
	}

	// Check if user is still enabled
	if !user.Enabled {
		Forbidden(w, "User account is disabled")
		return
	}

	// Generate new token pair
	tokenPair, err := h.jwtService.GenerateTokenPair(user)
	if err != nil {
		InternalServerError(w, "Failed to generate token")
		return
	}

	// Build response
	response := LoginResponse{
		AccessToken:  tokenPair.AccessToken,
		RefreshToken: tokenPair.RefreshToken,
		TokenType:    tokenPair.TokenType,
		ExpiresIn:    tokenPair.ExpiresIn,
		ExpiresAt:    tokenPair.ExpiresAt,
		User:         userToResponse(user),
	}

	WriteJSONOK(w, response)
}

// Me handles GET /api/v1/auth/me.
// Returns the current authenticated user's information.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	claims := middleware.GetClaimsFromContext(r.Context())
	if claims == nil {
		Unauthorized(w, "Authentication required")
		return
	}

	// Fetch fresh user data
	user, err := h.store.GetUser(r.Context(), claims.Username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			Unauthorized(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to fetch user")
		return
	}

	WriteJSONOK(w, userToResponse(user))
}

// userToResponse converts a User to a UserResponse for API output.
func userToResponse(user *models.User) UserResponse {
	return UserResponse{
		ID:                 user.ID,
		Username:           user.Username,
		DisplayName:        user.DisplayName,
		Email:              user.Email,
		Role:               string(user.Role),
		UID:                user.UID,
		Groups:             user.GetGroupNames(),
		Enabled:            user.Enabled,
		MustChangePassword: user.MustChangePassword,
	}
}
