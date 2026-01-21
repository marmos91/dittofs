package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// ShareHandler handles share management API endpoints.
type ShareHandler struct {
	store store.Store
}

// NewShareHandler creates a new ShareHandler.
func NewShareHandler(store store.Store) *ShareHandler {
	return &ShareHandler{store: store}
}

// CreateShareRequest is the request body for POST /api/v1/shares.
type CreateShareRequest struct {
	Name              string `json:"name"`
	MetadataStoreID   string `json:"metadata_store_id"`
	PayloadStoreID    string `json:"payload_store_id"`
	ReadOnly          bool   `json:"read_only,omitempty"`
	DefaultPermission string `json:"default_permission,omitempty"`
}

// UpdateShareRequest is the request body for PUT /api/v1/shares/{name}.
type UpdateShareRequest struct {
	MetadataStoreID   *string `json:"metadata_store_id,omitempty"`
	PayloadStoreID    *string `json:"payload_store_id,omitempty"`
	ReadOnly          *bool   `json:"read_only,omitempty"`
	DefaultPermission *string `json:"default_permission,omitempty"`
}

// ShareResponse is the response body for share endpoints.
type ShareResponse struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	MetadataStoreID   string    `json:"metadata_store_id"`
	PayloadStoreID    string    `json:"payload_store_id"`
	ReadOnly          bool      `json:"read_only"`
	DefaultPermission string    `json:"default_permission"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Create handles POST /api/v1/shares.
// Creates a new share (admin only).
func (h *ShareHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateShareRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Name == "" {
		BadRequest(w, "Share name is required")
		return
	}
	if req.MetadataStoreID == "" {
		BadRequest(w, "Metadata store ID is required")
		return
	}
	if req.PayloadStoreID == "" {
		BadRequest(w, "Payload store ID is required")
		return
	}

	// Set default permission if not provided
	defaultPerm := req.DefaultPermission
	if defaultPerm == "" {
		defaultPerm = "none"
	}

	now := time.Now()
	share := &models.Share{
		ID:                uuid.New().String(),
		Name:              req.Name,
		MetadataStoreID:   req.MetadataStoreID,
		PayloadStoreID:    req.PayloadStoreID,
		ReadOnly:          req.ReadOnly,
		DefaultPermission: defaultPerm,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if _, err := h.store.CreateShare(r.Context(), share); err != nil {
		if errors.Is(err, models.ErrDuplicateShare) {
			Conflict(w, "Share already exists")
			return
		}
		InternalServerError(w, "Failed to create share")
		return
	}

	WriteJSONCreated(w, shareToResponse(share))
}

// List handles GET /api/v1/shares.
// Lists all shares (admin only).
func (h *ShareHandler) List(w http.ResponseWriter, r *http.Request) {
	shares, err := h.store.ListShares(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list shares")
		return
	}

	response := make([]ShareResponse, len(shares))
	for i, s := range shares {
		response[i] = shareToResponse(s)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/shares/{name}.
// Gets a share by name (admin only).
func (h *ShareHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Share name is required")
		return
	}

	share, err := h.store.GetShare(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to get share")
		return
	}

	WriteJSONOK(w, shareToResponse(share))
}

// Update handles PUT /api/v1/shares/{name}.
// Updates a share (admin only).
func (h *ShareHandler) Update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Share name is required")
		return
	}

	var req UpdateShareRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Fetch existing share
	share, err := h.store.GetShare(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to get share")
		return
	}

	// Apply updates
	if req.MetadataStoreID != nil {
		share.MetadataStoreID = *req.MetadataStoreID
	}
	if req.PayloadStoreID != nil {
		share.PayloadStoreID = *req.PayloadStoreID
	}
	if req.ReadOnly != nil {
		share.ReadOnly = *req.ReadOnly
	}
	if req.DefaultPermission != nil {
		share.DefaultPermission = *req.DefaultPermission
	}
	share.UpdatedAt = time.Now()

	if err := h.store.UpdateShare(r.Context(), share); err != nil {
		InternalServerError(w, "Failed to update share")
		return
	}

	WriteJSONOK(w, shareToResponse(share))
}

// Delete handles DELETE /api/v1/shares/{name}.
// Deletes a share (admin only).
func (h *ShareHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Share name is required")
		return
	}

	if err := h.store.DeleteShare(r.Context(), name); err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to delete share")
		return
	}

	WriteNoContent(w)
}

// SetUserPermission handles PUT /api/v1/shares/{name}/users/{username}.
// Sets a user's permission for a share (admin only).
func (h *ShareHandler) SetUserPermission(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	username := chi.URLParam(r, "username")

	if shareName == "" {
		BadRequest(w, "Share name is required")
		return
	}
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	var req struct {
		Permission string `json:"permission"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Permission == "" {
		BadRequest(w, "Permission is required")
		return
	}

	perm := &models.UserSharePermission{
		UserID:     username,
		ShareID:    shareName,
		Permission: req.Permission,
	}

	if err := h.store.SetUserSharePermission(r.Context(), perm); err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to set user permission")
		return
	}

	WriteNoContent(w)
}

// DeleteUserPermission handles DELETE /api/v1/shares/{name}/users/{username}.
// Removes a user's permission for a share (admin only).
func (h *ShareHandler) DeleteUserPermission(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	username := chi.URLParam(r, "username")

	if shareName == "" {
		BadRequest(w, "Share name is required")
		return
	}
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	if err := h.store.DeleteUserSharePermission(r.Context(), username, shareName); err != nil {
		InternalServerError(w, "Failed to delete user permission")
		return
	}

	WriteNoContent(w)
}

// SetGroupPermission handles PUT /api/v1/shares/{name}/groups/{groupname}.
// Sets a group's permission for a share (admin only).
func (h *ShareHandler) SetGroupPermission(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	groupName := chi.URLParam(r, "groupname")

	if shareName == "" {
		BadRequest(w, "Share name is required")
		return
	}
	if groupName == "" {
		BadRequest(w, "Group name is required")
		return
	}

	var req struct {
		Permission string `json:"permission"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Permission == "" {
		BadRequest(w, "Permission is required")
		return
	}

	perm := &models.GroupSharePermission{
		GroupID:    groupName,
		ShareID:    shareName,
		Permission: req.Permission,
	}

	if err := h.store.SetGroupSharePermission(r.Context(), perm); err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			NotFound(w, "Group not found")
			return
		}
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to set group permission")
		return
	}

	WriteNoContent(w)
}

// DeleteGroupPermission handles DELETE /api/v1/shares/{name}/groups/{groupname}.
// Removes a group's permission for a share (admin only).
func (h *ShareHandler) DeleteGroupPermission(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	groupName := chi.URLParam(r, "groupname")

	if shareName == "" {
		BadRequest(w, "Share name is required")
		return
	}
	if groupName == "" {
		BadRequest(w, "Group name is required")
		return
	}

	if err := h.store.DeleteGroupSharePermission(r.Context(), groupName, shareName); err != nil {
		InternalServerError(w, "Failed to delete group permission")
		return
	}

	WriteNoContent(w)
}

// shareToResponse converts a models.Share to ShareResponse.
func shareToResponse(s *models.Share) ShareResponse {
	return ShareResponse{
		ID:                s.ID,
		Name:              s.Name,
		MetadataStoreID:   s.MetadataStoreID,
		PayloadStoreID:    s.PayloadStoreID,
		ReadOnly:          s.ReadOnly,
		DefaultPermission: s.DefaultPermission,
		CreatedAt:         s.CreatedAt,
		UpdatedAt:         s.UpdatedAt,
	}
}
