package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/api/auth"
	"github.com/marmos91/dittofs/pkg/api/middleware"
	"github.com/marmos91/dittofs/pkg/identity"
)

// AuthHandler handles authentication-related API endpoints.
type AuthHandler struct {
	identityStore identity.IdentityStore
	jwtService    *auth.JWTService
}

// NewAuthHandler creates a new AuthHandler.
func NewAuthHandler(identityStore identity.IdentityStore, jwtService *auth.JWTService) *AuthHandler {
	return &AuthHandler{
		identityStore: identityStore,
		jwtService:    jwtService,
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
	Groups             []string `json:"groups,omitempty"`
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
	user, err := h.identityStore.ValidateCredentials(req.Username, req.Password)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCredentials) || errors.Is(err, identity.ErrUserNotFound) {
			Unauthorized(w, "Invalid username or password")
			return
		}
		if errors.Is(err, identity.ErrUserDisabled) {
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
	if err := h.identityStore.UpdateLastLogin(r.Context(), user.Username, time.Now()); err != nil {
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
	user, ok := getUserOrUnauthorized(w, h.identityStore, claims.Username)
	if !ok {
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
	user, ok := getUserOrUnauthorized(w, h.identityStore, claims.Username)
	if !ok {
		return
	}

	WriteJSONOK(w, userToResponse(user))
}

// userToResponse converts a User to a UserResponse for API output.
func userToResponse(user *identity.User) UserResponse {
	return UserResponse{
		ID:                 user.ID,
		Username:           user.Username,
		DisplayName:        user.DisplayName,
		Email:              user.Email,
		Role:               string(user.Role),
		Groups:             user.Groups,
		MustChangePassword: user.MustChangePassword,
	}
}
