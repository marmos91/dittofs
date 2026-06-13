package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/internal/controlplane/api/middleware"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// userStore is the minimal store surface needed by UserHandler. It composes the
// sub-interfaces required to manage users plus their group memberships and
// share permissions. store.Store satisfies it because Store embeds all of these.
type userStore interface {
	store.UserStore
	store.GroupStore
	store.PermissionStore
	store.ShareStore
}

// UserHandler handles user management API endpoints.
type UserHandler struct {
	store      userStore
	jwtService *auth.JWTService
}

// NewUserHandler creates a new UserHandler. jwtService is required for generating
// new tokens after password changes to ensure users receive fresh credentials.
// Returns an error if jwtService is nil, allowing callers to handle the
// misconfiguration gracefully (e.g., at startup).
func NewUserHandler(s userStore, jwtService *auth.JWTService) (*UserHandler, error) {
	if jwtService == nil {
		return nil, errors.New("NewUserHandler: jwtService is required and must not be nil")
	}
	return &UserHandler{store: s, jwtService: jwtService}, nil
}

// CreateUserRequest is the request body for POST /api/v1/users.
type CreateUserRequest struct {
	Username    string                            `json:"username"`
	Password    string                            `json:"password"`
	Email       string                            `json:"email,omitempty"`
	DisplayName string                            `json:"display_name,omitempty"`
	Role        string                            `json:"role,omitempty"`
	UID         *uint32                           `json:"uid,omitempty"`
	Groups      []string                          `json:"groups,omitempty"`
	Enabled     *bool                             `json:"enabled,omitempty"`
	SharePerms  map[string]models.SharePermission `json:"share_permissions,omitempty"`
}

// UpdateUserRequest is the request body for PUT /api/v1/users/{username}.
type UpdateUserRequest struct {
	Email       *string                            `json:"email,omitempty"`
	DisplayName *string                            `json:"display_name,omitempty"`
	Role        *string                            `json:"role,omitempty"`
	UID         *uint32                            `json:"uid,omitempty"`
	Groups      *[]string                          `json:"groups,omitempty"`
	Enabled     *bool                              `json:"enabled,omitempty"`
	SharePerms  *map[string]models.SharePermission `json:"share_permissions,omitempty"`
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
	passwordHash, ntHashHex, err := models.HashPasswordWithNT(req.Password)
	if err != nil {
		InternalServerError(w, "Failed to hash password")
		return
	}

	// Determine role
	role := models.RoleUser
	if req.Role != "" {
		role = models.UserRole(req.Role)
		if !role.IsValid() {
			BadRequest(w, "Invalid role. Must be 'user', 'admin', or 'operator'")
			return
		}
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	user := &models.User{
		Username:           req.Username,
		PasswordHash:       passwordHash,
		NTHash:             ntHashHex,
		Enabled:            enabled,
		MustChangePassword: role == models.RoleAdmin,
		Role:               string(role),
		UID:                req.UID,
		DisplayName:        req.DisplayName,
		Email:              req.Email,
	}

	if _, err := h.store.CreateUserWithGroups(r.Context(), user, req.Groups); err != nil {
		if errors.Is(err, models.ErrDuplicateUser) {
			Conflict(w, "User already exists")
			return
		}
		if errors.Is(err, models.ErrGroupNotFound) {
			BadRequest(w, "One or more specified groups do not exist")
			return
		}
		InternalServerError(w, "Failed to create user")
		return
	}

	// Apply any requested share permissions. These are best-effort and
	// non-transactional with user creation: an unresolvable share or invalid
	// permission is skipped rather than rolling back the created user. The
	// dedicated permissions endpoint remains the canonical management path.
	h.applySharePerms(r, user.ID, req.SharePerms)

	WriteJSONCreated(w, userToResponse(user))
}

// List handles GET /api/v1/users.
// Lists all users (admin only).
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers(r.Context())
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
// Gets a user by username. Admins can get any user, non-admins can only get their own info.
func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	// Check authorization - allow admin or self-access
	claims := middleware.GetClaimsFromContext(r.Context())
	if claims == nil {
		Unauthorized(w, "Authentication required")
		return
	}

	// Non-admins can only access their own info
	if !claims.IsAdmin() && claims.Username != username {
		Forbidden(w, "Access denied")
		return
	}

	user, err := h.store.GetUser(r.Context(), username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to get user")
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
	user, err := h.store.GetUser(r.Context(), username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to get user")
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
		role := models.UserRole(*req.Role)
		if !role.IsValid() {
			BadRequest(w, "Invalid role. Must be 'user', 'admin', or 'operator'")
			return
		}
		user.Role = string(role)
	}
	if req.Enabled != nil {
		user.Enabled = *req.Enabled
	}
	if req.UID != nil {
		user.UID = req.UID
	}

	if err := h.store.UpdateUser(r.Context(), user); err != nil {
		InternalServerError(w, "Failed to update user")
		return
	}

	// Replace group memberships if the caller provided a Groups list.
	if req.Groups != nil {
		if err := h.store.ReplaceUserGroups(r.Context(), username, *req.Groups); err != nil {
			if errors.Is(err, models.ErrGroupNotFound) {
				BadRequest(w, "One or more specified groups do not exist")
				return
			}
			InternalServerError(w, "Failed to update user groups")
			return
		}
	}

	// Apply any requested share permissions (best-effort; see Create).
	if req.SharePerms != nil {
		h.applySharePerms(r, user.ID, *req.SharePerms)
	}

	// Re-fetch so the response reflects the updated groups and permissions.
	user, err = h.store.GetUser(r.Context(), username)
	if err != nil {
		InternalServerError(w, "Failed to reload user")
		return
	}

	WriteJSONOK(w, userToResponse(user))
}

// applySharePerms applies the requested share permissions to a user. It is
// best-effort: invalid permissions and unresolvable share names are skipped so
// a bad entry never blocks user create/update. Logged at debug per the
// expected-error logging convention.
func (h *UserHandler) applySharePerms(r *http.Request, userID string, perms map[string]models.SharePermission) {
	for shareName, perm := range perms {
		if !perm.IsValid() {
			continue
		}
		sh, err := h.store.GetShare(r.Context(), shareName)
		if err != nil {
			// Share not found (or other lookup error): skip silently.
			continue
		}
		_ = h.store.SetUserSharePermission(r.Context(), &models.UserSharePermission{
			UserID:     userID,
			ShareID:    sh.ID,
			ShareName:  sh.Name,
			Permission: string(perm),
		})
	}
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
	if models.IsAdminUsername(username) {
		Forbidden(w, "Cannot delete admin user")
		return
	}

	if err := h.store.DeleteUser(r.Context(), username); err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
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
	user, err := h.store.GetUser(r.Context(), username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to get user")
		return
	}

	// Hash password and compute NT hash
	passwordHash, ntHashHex, err := models.HashPasswordWithNT(req.NewPassword)
	if err != nil {
		InternalServerError(w, "Failed to hash password")
		return
	}

	// Set must change password flag only for admin users.
	// Admin accounts are high-privilege, so reset passwords are treated as temporary
	// credentials that must be personalized. For regular users, the admin-set password
	// is considered final per deployment policy (admins can choose a permanent password).
	mustChange := user.Role == string(models.RoleAdmin)

	// Update password and the must-change flag atomically so a partial failure
	// cannot leave the new password persisted while the flag is stale.
	if err := h.store.UpdatePasswordAndFlags(r.Context(), username, passwordHash, ntHashHex, mustChange); err != nil {
		InternalServerError(w, "Failed to update password")
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
	user, err := h.store.GetUser(r.Context(), claims.Username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			Unauthorized(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to get user")
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
		if !models.VerifyPassword(req.CurrentPassword, user.PasswordHash) {
			Unauthorized(w, "Current password is incorrect")
			return
		}
	}

	// Hash password and compute NT hash
	passwordHash, ntHashHex, err := models.HashPasswordWithNT(req.NewPassword)
	if err != nil {
		InternalServerError(w, "Failed to hash password")
		return
	}

	// Update the password and clear the must-change flag atomically. Doing both
	// in one operation prevents a partial failure from persisting the new
	// password while leaving MustChangePassword set, which would lock the user
	// out (the old password needed to satisfy the current-password gate is gone).
	if err := h.store.UpdatePasswordAndFlags(r.Context(), claims.Username, passwordHash, ntHashHex, false); err != nil {
		InternalServerError(w, "Failed to update password")
		return
	}

	// Reflect the cleared flag on the in-memory user for token generation.
	user.MustChangePassword = false

	// Generate new tokens with updated claims (MustChangePassword = false)
	tokenPair, err := h.jwtService.GenerateTokenPair(user)
	if err != nil {
		InternalServerError(w, "Failed to generate new tokens")
		return
	}

	// Return new tokens so client can update stored credentials
	WriteJSONOK(w, LoginResponse{
		AccessToken:  tokenPair.AccessToken,
		RefreshToken: tokenPair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    tokenPair.ExpiresIn,
		ExpiresAt:    tokenPair.ExpiresAt,
		User:         userToResponse(user),
	})
}
