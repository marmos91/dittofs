package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// normalizeShareName ensures a share name has exactly one leading slash.
// "export" -> "/export"
// "/export" -> "/export"
// "//export" -> "/export"
func normalizeShareName(name string) string {
	// URL-decode the name in case it was encoded
	decoded, err := url.PathUnescape(name)
	if err != nil {
		decoded = name
	}
	// Trim all leading slashes, then add exactly one
	trimmed := strings.TrimLeft(decoded, "/")
	if trimmed == "" {
		return "/"
	}
	return "/" + trimmed
}

// ShareHandlerStore is the composite interface required by ShareHandler.
// ShareHandler needs share CRUD, permission management, store config lookups
// (to validate metadata/payload store references), and user/group lookups
// (to resolve permission display names).
type ShareHandlerStore interface {
	store.ShareStore
	store.PermissionStore
	store.MetadataStoreConfigStore
	store.PayloadStoreConfigStore
	store.UserStore
	store.GroupStore
}

// ShareHandler handles share management API endpoints.
type ShareHandler struct {
	store   ShareHandlerStore
	runtime *runtime.Runtime
}

// NewShareHandler creates a new ShareHandler.
func NewShareHandler(s ShareHandlerStore, rt *runtime.Runtime) *ShareHandler {
	return &ShareHandler{store: s, runtime: rt}
}

// CreateShareRequest is the request body for POST /api/v1/shares.
type CreateShareRequest struct {
	Name              string    `json:"name"`
	MetadataStoreID   string    `json:"metadata_store_id"`
	PayloadStoreID    string    `json:"payload_store_id"`
	ReadOnly          bool      `json:"read_only,omitempty"`
	DefaultPermission string    `json:"default_permission,omitempty"`
	BlockedOperations *[]string `json:"blocked_operations,omitempty"`
}

// UpdateShareRequest is the request body for PUT /api/v1/shares/{name}.
type UpdateShareRequest struct {
	MetadataStoreID   *string   `json:"metadata_store_id,omitempty"`
	PayloadStoreID    *string   `json:"payload_store_id,omitempty"`
	ReadOnly          *bool     `json:"read_only,omitempty"`
	DefaultPermission *string   `json:"default_permission,omitempty"`
	BlockedOperations *[]string `json:"blocked_operations,omitempty"`
}

// ShareResponse is the response body for share endpoints.
type ShareResponse struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	MetadataStoreID   string    `json:"metadata_store_id"`
	PayloadStoreID    string    `json:"payload_store_id"`
	ReadOnly          bool      `json:"read_only"`
	DefaultPermission string    `json:"default_permission"`
	BlockedOperations []string  `json:"blocked_operations,omitempty"`
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

	// Normalize share name to always have leading slash
	req.Name = normalizeShareName(req.Name)
	if req.MetadataStoreID == "" {
		BadRequest(w, "Metadata store ID is required")
		return
	}
	if req.PayloadStoreID == "" {
		BadRequest(w, "Payload store ID is required")
		return
	}

	// Validate that metadata store exists (try by name first, then by ID)
	metaStore, err := h.store.GetMetadataStore(r.Context(), req.MetadataStoreID)
	if err != nil {
		metaStore, err = h.store.GetMetadataStoreByID(r.Context(), req.MetadataStoreID)
	}
	if err != nil {
		BadRequest(w, "Metadata store not found: "+req.MetadataStoreID)
		return
	}

	// Validate that payload store exists (try by name first, then by ID)
	payloadStore, err := h.store.GetPayloadStore(r.Context(), req.PayloadStoreID)
	if err != nil {
		payloadStore, err = h.store.GetPayloadStoreByID(r.Context(), req.PayloadStoreID)
	}
	if err != nil {
		BadRequest(w, "Payload store not found: "+req.PayloadStoreID)
		return
	}

	// Set default permission if not provided
	// Use "read-write" for NFS compatibility (same as traditional NFS servers)
	// This allows anonymous/unknown UIDs to access the share, with file-level
	// permissions enforcing access control (Unix DAC model).
	defaultPerm := req.DefaultPermission
	if defaultPerm == "" {
		defaultPerm = "read-write"
	}

	// Validate BlockedOperations entries
	if req.BlockedOperations != nil {
		for _, op := range *req.BlockedOperations {
			if !isValidBlockedOperation(op) {
				BadRequest(w, "Unknown blocked operation: "+op)
				return
			}
		}
	}

	now := time.Now()
	share := &models.Share{
		ID:                uuid.New().String(),
		Name:              req.Name,
		MetadataStoreID:   metaStore.ID,    // Use actual store ID (UUID), not name
		PayloadStoreID:    payloadStore.ID, // Use actual store ID (UUID), not name
		ReadOnly:          req.ReadOnly,
		DefaultPermission: defaultPerm,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	// Set blocked operations
	if req.BlockedOperations != nil {
		share.SetBlockedOps(*req.BlockedOperations)
	}

	if _, err := h.store.CreateShare(r.Context(), share); err != nil {
		if errors.Is(err, models.ErrDuplicateShare) {
			Conflict(w, "Share already exists")
			return
		}
		InternalServerError(w, "Failed to create share")
		return
	}

	// Create default adapter configs for the new share
	nfsOpts := models.DefaultNFSExportOptions()
	nfsCfg := &models.ShareAdapterConfig{ShareID: share.ID, AdapterType: "nfs"}
	if err := nfsCfg.SetConfig(nfsOpts); err == nil {
		_ = h.store.SetShareAdapterConfig(r.Context(), nfsCfg)
	}

	smbOpts := models.DefaultSMBShareOptions()
	smbCfg := &models.ShareAdapterConfig{ShareID: share.ID, AdapterType: "smb"}
	if err := smbCfg.SetConfig(smbOpts); err == nil {
		_ = h.store.SetShareAdapterConfig(r.Context(), smbCfg)
	}

	// Add share to runtime if runtime is available
	if h.runtime != nil {
		shareConfig := &runtime.ShareConfig{
			Name:              req.Name,
			MetadataStore:     metaStore.Name,
			ReadOnly:          req.ReadOnly,
			DefaultPermission: defaultPerm,
			Squash:            nfsOpts.GetSquashMode(),
			AnonymousUID:      nfsOpts.GetAnonymousUID(),
			AnonymousGID:      nfsOpts.GetAnonymousGID(),
			AllowAuthSys:      nfsOpts.AllowAuthSys,
			AllowAuthSysSet:   true,
			RequireKerberos:   nfsOpts.RequireKerberos,
			MinKerberosLevel:  nfsOpts.MinKerberosLevel,
			BlockedOperations: share.GetBlockedOps(),
		}

		if err := h.runtime.AddShare(r.Context(), shareConfig); err != nil {
			// Share created in DB but failed to load into runtime
			// Log but don't fail the request - share can be loaded on restart
			logger.Warn("Share created but failed to add to runtime",
				"share", req.Name, "error", err)
		}
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
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
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
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
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
	if req.BlockedOperations != nil {
		for _, op := range *req.BlockedOperations {
			if !isValidBlockedOperation(op) {
				BadRequest(w, "Unknown blocked operation: "+op)
				return
			}
		}
		share.SetBlockedOps(*req.BlockedOperations)
	}

	share.UpdatedAt = time.Now()

	if err := h.store.UpdateShare(r.Context(), share); err != nil {
		logger.Error("Failed to update share", "share", share.Name, "id", share.ID, "error", err)
		InternalServerError(w, "Failed to update share")
		return
	}

	// Update runtime if available
	if h.runtime != nil {
		if err := h.runtime.UpdateShare(share.Name, req.ReadOnly, req.DefaultPermission); err != nil {
			// Log but don't fail - database was updated successfully
			logger.Warn("Failed to update share in runtime", "share", share.Name, "error", err)
		}
	}

	WriteJSONOK(w, shareToResponse(share))
}

// Delete handles DELETE /api/v1/shares/{name}.
// Deletes a share (admin only).
func (h *ShareHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
		BadRequest(w, "Share name is required")
		return
	}

	// Remove from runtime first (if runtime is available)
	if h.runtime != nil {
		_ = h.runtime.RemoveShare(name) // Ignore error if not found in runtime
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
	shareName := normalizeShareName(chi.URLParam(r, "name"))
	username := chi.URLParam(r, "username")

	if shareName == "/" {
		BadRequest(w, "Share name is required")
		return
	}
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	var req struct {
		Level string `json:"level"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Level == "" {
		BadRequest(w, "Permission level is required")
		return
	}

	// Look up user and share to get their IDs
	user, err := h.store.GetUser(r.Context(), username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to get user")
		return
	}

	share, err := h.store.GetShare(r.Context(), shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to get share")
		return
	}

	perm := &models.UserSharePermission{
		UserID:     user.ID,
		ShareID:    share.ID,
		ShareName:  share.Name,
		Permission: req.Level,
	}

	if err := h.store.SetUserSharePermission(r.Context(), perm); err != nil {
		InternalServerError(w, "Failed to set user permission")
		return
	}

	WriteNoContent(w)
}

// DeleteUserPermission handles DELETE /api/v1/shares/{name}/permissions/users/{username}.
// Removes a user's permission for a share (admin only).
func (h *ShareHandler) DeleteUserPermission(w http.ResponseWriter, r *http.Request) {
	shareName := normalizeShareName(chi.URLParam(r, "name"))
	username := chi.URLParam(r, "username")

	if shareName == "/" {
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
	shareName := normalizeShareName(chi.URLParam(r, "name"))
	groupName := chi.URLParam(r, "groupname")

	if shareName == "/" {
		BadRequest(w, "Share name is required")
		return
	}
	if groupName == "" {
		BadRequest(w, "Group name is required")
		return
	}

	var req struct {
		Level string `json:"level"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Level == "" {
		BadRequest(w, "Permission level is required")
		return
	}

	// Look up group and share to get their IDs
	group, err := h.store.GetGroup(r.Context(), groupName)
	if err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			NotFound(w, "Group not found")
			return
		}
		InternalServerError(w, "Failed to get group")
		return
	}

	share, err := h.store.GetShare(r.Context(), shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to get share")
		return
	}

	perm := &models.GroupSharePermission{
		GroupID:    group.ID,
		ShareID:    share.ID,
		ShareName:  share.Name,
		Permission: req.Level,
	}

	if err := h.store.SetGroupSharePermission(r.Context(), perm); err != nil {
		InternalServerError(w, "Failed to set group permission")
		return
	}

	WriteNoContent(w)
}

// DeleteGroupPermission handles DELETE /api/v1/shares/{name}/permissions/groups/{groupname}.
// Removes a group's permission for a share (admin only).
func (h *ShareHandler) DeleteGroupPermission(w http.ResponseWriter, r *http.Request) {
	shareName := normalizeShareName(chi.URLParam(r, "name"))
	groupName := chi.URLParam(r, "groupname")

	if shareName == "/" {
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

// PermissionResponse represents a permission entry for a share.
type PermissionResponse struct {
	Type  string `json:"type"`  // "user" or "group"
	Name  string `json:"name"`  // username or group name
	Level string `json:"level"` // permission level
}

// ListPermissions handles GET /api/v1/shares/{name}/permissions.
// Returns all permissions configured for a share (admin only).
func (h *ShareHandler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	shareName := normalizeShareName(chi.URLParam(r, "name"))
	if shareName == "/" {
		BadRequest(w, "Share name is required")
		return
	}

	// Get share with permissions loaded
	share, err := h.store.GetShare(r.Context(), shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to get share")
		return
	}

	// Build permission list
	var perms []PermissionResponse

	// Get user permissions with usernames
	for _, up := range share.UserPermissions {
		// Look up user to get username
		user, err := h.store.GetUserByID(r.Context(), up.UserID)
		if err != nil {
			// Skip if user no longer exists (orphaned permission)
			continue
		}
		perms = append(perms, PermissionResponse{
			Type:  "user",
			Name:  user.Username,
			Level: up.Permission,
		})
	}

	// Get group permissions with group names
	for _, gp := range share.GroupPermissions {
		// Look up group to get name
		group, err := h.store.GetGroupByID(r.Context(), gp.GroupID)
		if err != nil {
			// Skip if group no longer exists (orphaned permission)
			continue
		}
		perms = append(perms, PermissionResponse{
			Type:  "group",
			Name:  group.Name,
			Level: gp.Permission,
		})
	}

	WriteJSONOK(w, perms)
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
		BlockedOperations: s.GetBlockedOps(),
		CreatedAt:         s.CreatedAt,
		UpdatedAt:         s.UpdatedAt,
	}
}

// isValidBlockedOperation checks if a blocked operation name is valid for any protocol.
func isValidBlockedOperation(op string) bool {
	return isValidNFSOperation(op) || isValidSMBOperation(op)
}
