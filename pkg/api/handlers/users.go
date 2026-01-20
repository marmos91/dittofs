package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/api/middleware"
	"github.com/marmos91/dittofs/pkg/identity"
)

// UserHandler handles user management API endpoints.
type UserHandler struct {
	identityStore identity.IdentityStore
}

// NewUserHandler creates a new UserHandler.
func NewUserHandler(identityStore identity.IdentityStore) *UserHandler {
	return &UserHandler{identityStore: identityStore}
}

// CreateUserRequest is the request body for POST /api/v1/users.
type CreateUserRequest struct {
	Username    string                              `json:"username"`
	Password    string                              `json:"password"`
	Email       string                              `json:"email,omitempty"`
	DisplayName string                              `json:"display_name,omitempty"`
	Role        string                              `json:"role,omitempty"`
	Groups      []string                            `json:"groups,omitempty"`
	Enabled     *bool                               `json:"enabled,omitempty"`
	SharePerms  map[string]identity.SharePermission `json:"share_permissions,omitempty"`
}

// UpdateUserRequest is the request body for PUT /api/v1/users/{username}.
type UpdateUserRequest struct {
	Email       *string                              `json:"email,omitempty"`
	DisplayName *string                              `json:"display_name,omitempty"`
	Role        *string                              `json:"role,omitempty"`
	Groups      *[]string                            `json:"groups,omitempty"`
	Enabled     *bool                                `json:"enabled,omitempty"`
	SharePerms  *map[string]identity.SharePermission `json:"share_permissions,omitempty"`
}

// ChangePasswordRequest is the request body for password change endpoints.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password,omitempty"`
	NewPassword     string `json:"new_password"`
}

// Create handles POST /api/v1/users.
// Creates a new user (admin only).
func (h *UserHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Username == "" {
		BadRequest(w, "Username is required")
		return
	}
	if req.Password == "" {
		BadRequest(w, "Password is required")
		return
	}

	// Hash password and compute NT hash
	passwordHash, ntHashHex, err := identity.HashPasswordWithNT(req.Password)
	if err != nil {
		InternalServerError(w, "Failed to hash password")
		return
	}

	// Determine role
	role := identity.RoleUser
	if req.Role != "" {
		role = identity.UserRole(req.Role)
		if !role.IsValid() {
			BadRequest(w, "Invalid role. Must be 'user' or 'admin'")
			return
		}
	}

	// Create user
	user := &identity.User{
		ID:                 uuid.New().String(),
		Username:           req.Username,
		PasswordHash:       passwordHash,
		NTHash:             ntHashHex,
		Enabled:            true,
		MustChangePassword: true, // New users must change password
		Role:               role,
		Groups:             req.Groups,
		SharePermissions:   req.SharePerms,
		DisplayName:        req.DisplayName,
		Email:              req.Email,
		CreatedAt:          time.Now(),
	}

	// Override enabled if explicitly set
	if req.Enabled != nil {
		user.Enabled = *req.Enabled
	}

	if err := h.identityStore.CreateUser(r.Context(), user); err != nil {
		if errors.Is(err, identity.ErrDuplicateUser) {
			Conflict(w, "User already exists")
			return
		}
		InternalServerError(w, "Failed to create user")
		return
	}

	WriteJSONCreated(w, userToResponse(user))
}

// List handles GET /api/v1/users.
// Lists all users (admin only).
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	users, err := h.identityStore.ListUsers()
	if err != nil {
		InternalServerError(w, "Failed to list users")
		return
	}

	response := make([]UserResponse, len(users))
	for i, u := range users {
		response[i] = userToResponse(u)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/users/{username}.
// Gets a user by username (admin only).
func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	user, ok := getUserOrError(w, h.identityStore, username)
	if !ok {
		return
	}

	WriteJSONOK(w, userToResponse(user))
}

// Update handles PUT /api/v1/users/{username}.
// Updates a user (admin only).
func (h *UserHandler) Update(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	var req UpdateUserRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Fetch existing user
	user, ok := getUserOrError(w, h.identityStore, username)
	if !ok {
		return
	}

	// Apply updates
	if req.Email != nil {
		user.Email = *req.Email
	}
	if req.DisplayName != nil {
		user.DisplayName = *req.DisplayName
	}
	if req.Role != nil {
		role := identity.UserRole(*req.Role)
		if !role.IsValid() {
			BadRequest(w, "Invalid role. Must be 'user' or 'admin'")
			return
		}
		user.Role = role
	}
	if req.Groups != nil {
		user.Groups = *req.Groups
	}
	if req.Enabled != nil {
		user.Enabled = *req.Enabled
	}
	if req.SharePerms != nil {
		user.SharePermissions = *req.SharePerms
	}

	if err := h.identityStore.UpdateUser(r.Context(), user); err != nil {
		InternalServerError(w, "Failed to update user")
		return
	}

	WriteJSONOK(w, userToResponse(user))
}

// Delete handles DELETE /api/v1/users/{username}.
// Deletes a user (admin only).
func (h *UserHandler) Delete(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	// Prevent deleting admin user
	if identity.IsAdminUsername(username) {
		Forbidden(w, "Cannot delete admin user")
		return
	}

	if err := h.identityStore.DeleteUser(r.Context(), username); err != nil {
		if errors.Is(err, identity.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to delete user")
		return
	}

	WriteNoContent(w)
}

// ResetPassword handles POST /api/v1/users/{username}/password.
// Resets a user's password (admin only).
func (h *UserHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	var req ChangePasswordRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.NewPassword == "" {
		BadRequest(w, "New password is required")
		return
	}

	// Verify user exists
	user, ok := getUserOrError(w, h.identityStore, username)
	if !ok {
		return
	}

	// Hash password and compute NT hash
	passwordHash, ntHashHex, err := identity.HashPasswordWithNT(req.NewPassword)
	if err != nil {
		InternalServerError(w, "Failed to hash password")
		return
	}

	// Update password and set must change flag
	if err := h.identityStore.UpdatePassword(r.Context(), username, passwordHash, ntHashHex); err != nil {
		InternalServerError(w, "Failed to update password")
		return
	}

	// Set must change password flag
	user.MustChangePassword = true
	if err := h.identityStore.UpdateUser(r.Context(), user); err != nil {
		InternalServerError(w, "Failed to update user")
		return
	}

	WriteNoContent(w)
}

// ChangeOwnPassword handles POST /api/v1/users/me/password.
// Changes the current user's own password.
func (h *UserHandler) ChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	claims := middleware.GetClaimsFromContext(r.Context())
	if claims == nil {
		Unauthorized(w, "Authentication required")
		return
	}

	var req ChangePasswordRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.NewPassword == "" {
		BadRequest(w, "New password is required")
		return
	}

	// Get current user
	user, ok := getUserOrUnauthorized(w, h.identityStore, claims.Username)
	if !ok {
		return
	}

	// If user must change password, current password validation is optional
	// Otherwise, require current password
	if !user.MustChangePassword {
		if req.CurrentPassword == "" {
			BadRequest(w, "Current password is required")
			return
		}

		// Validate current password
		if !identity.VerifyPassword(req.CurrentPassword, user.PasswordHash) {
			Unauthorized(w, "Current password is incorrect")
			return
		}
	}

	// Hash password and compute NT hash
	passwordHash, ntHashHex, err := identity.HashPasswordWithNT(req.NewPassword)
	if err != nil {
		InternalServerError(w, "Failed to hash password")
		return
	}

	// Update password
	if err := h.identityStore.UpdatePassword(r.Context(), claims.Username, passwordHash, ntHashHex); err != nil {
		InternalServerError(w, "Failed to update password")
		return
	}

	// Clear must change password flag
	user.MustChangePassword = false
	if err := h.identityStore.UpdateUser(r.Context(), user); err != nil {
		InternalServerError(w, "Failed to update user")
		return
	}

	WriteNoContent(w)
}
