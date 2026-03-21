package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
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
// (to validate metadata/block store references), and user/group lookups
// (to resolve permission display names).
type ShareHandlerStore interface {
	store.ShareStore
	store.PermissionStore
	store.MetadataStoreConfigStore
	store.BlockStoreConfigStore
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
	LocalBlockStore   string    `json:"local_block_store"`
	RemoteBlockStore  *string   `json:"remote_block_store,omitempty"`
	ReadOnly          bool      `json:"read_only,omitempty"`
	EncryptData       bool      `json:"encrypt_data,omitempty"`
	DefaultPermission string    `json:"default_permission,omitempty"`
	BlockedOperations *[]string `json:"blocked_operations,omitempty"`
	RetentionPolicy   string    `json:"retention_policy,omitempty"`
	RetentionTTL      string    `json:"retention_ttl,omitempty"` // Duration string like "72h"
	LocalStoreSize    string    `json:"local_store_size,omitempty"`
	ReadBufferSize    string    `json:"read_buffer_size,omitempty"`
	QuotaBytes        string    `json:"quota_bytes,omitempty"` // Human-readable, e.g., "10GiB" (0 = unlimited)
}

// UpdateShareRequest is the request body for PUT /api/v1/shares/{name}.
type UpdateShareRequest struct {
	MetadataStoreID    *string   `json:"metadata_store_id,omitempty"`
	LocalBlockStoreID  *string   `json:"local_block_store_id,omitempty"`
	RemoteBlockStoreID *string   `json:"remote_block_store_id,omitempty"`
	ReadOnly           *bool     `json:"read_only,omitempty"`
	EncryptData        *bool     `json:"encrypt_data,omitempty"`
	DefaultPermission  *string   `json:"default_permission,omitempty"`
	BlockedOperations  *[]string `json:"blocked_operations,omitempty"`
	RetentionPolicy    *string   `json:"retention_policy,omitempty"`
	RetentionTTL       *string   `json:"retention_ttl,omitempty"` // Duration string like "72h"
	LocalStoreSize     *string   `json:"local_store_size,omitempty"`
	ReadBufferSize     *string   `json:"read_buffer_size,omitempty"`
	QuotaBytes         *string   `json:"quota_bytes,omitempty"` // Human-readable, nil = no change, "0" = remove quota
}

// ShareResponse is the response body for share endpoints.
type ShareResponse struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	MetadataStoreID    string    `json:"metadata_store_id"`
	LocalBlockStoreID  string    `json:"local_block_store_id"`
	RemoteBlockStoreID *string   `json:"remote_block_store_id"`
	ReadOnly           bool      `json:"read_only"`
	EncryptData        bool      `json:"encrypt_data"`
	DefaultPermission  string    `json:"default_permission"`
	BlockedOperations  []string  `json:"blocked_operations,omitempty"`
	RetentionPolicy    string    `json:"retention_policy"`
	RetentionTTL       string    `json:"retention_ttl,omitempty"`    // Human-readable duration
	LocalStoreSize     string    `json:"local_store_size,omitempty"` // Human-readable byte size
	ReadBufferSize     string    `json:"read_buffer_size,omitempty"` // Human-readable byte size
	QuotaBytes         string    `json:"quota_bytes,omitempty"`      // Human-readable, e.g., "10 GiB" or empty if unlimited
	UsedBytes          int64     `json:"used_bytes"`                 // Logical used bytes (sum of file sizes)
	PhysicalBytes      int64     `json:"physical_bytes"`             // Block store disk usage
	UsagePercent       float64   `json:"usage_percent"`              // 0-100, 0 if no quota
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
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
	if req.LocalBlockStore == "" {
		BadRequest(w, "Local block store is required")
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

	// Validate that local block store exists (try by name first, then by ID)
	localBlockStore, err := h.store.GetBlockStore(r.Context(), req.LocalBlockStore, models.BlockStoreKindLocal)
	if err != nil {
		localBlockStore, err = h.store.GetBlockStoreByID(r.Context(), req.LocalBlockStore)
	}
	if err != nil {
		BadRequest(w, "Local block store not found: "+req.LocalBlockStore)
		return
	}

	// Validate optional remote block store
	var remoteBlockStoreID *string
	if req.RemoteBlockStore != nil && *req.RemoteBlockStore != "" {
		remoteStore, remoteErr := h.store.GetBlockStore(r.Context(), *req.RemoteBlockStore, models.BlockStoreKindRemote)
		if remoteErr != nil {
			remoteStore, remoteErr = h.store.GetBlockStoreByID(r.Context(), *req.RemoteBlockStore)
		}
		if remoteErr != nil {
			BadRequest(w, "Remote block store not found: "+*req.RemoteBlockStore)
			return
		}
		remoteBlockStoreID = &remoteStore.ID
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

	// Parse and validate retention policy
	retPolicy, err := blockstore.ParseRetentionPolicy(req.RetentionPolicy)
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	var retTTL time.Duration
	if req.RetentionTTL != "" {
		retTTL, err = time.ParseDuration(req.RetentionTTL)
		if err != nil {
			BadRequest(w, "Invalid retention TTL duration: "+err.Error())
			return
		}
	}
	if err = blockstore.ValidateRetentionPolicy(retPolicy, retTTL); err != nil {
		BadRequest(w, err.Error())
		return
	}

	// Parse optional per-share size overrides
	var localStoreSize, readBufferSize, quotaBytes int64
	if req.LocalStoreSize != "" {
		bs, parseErr := bytesize.ParseByteSize(req.LocalStoreSize)
		if parseErr != nil {
			BadRequest(w, "Invalid local_store_size: "+parseErr.Error())
			return
		}
		localStoreSize = bs.Int64()
	}
	if req.ReadBufferSize != "" {
		bs, parseErr := bytesize.ParseByteSize(req.ReadBufferSize)
		if parseErr != nil {
			BadRequest(w, "Invalid read_buffer_size: "+parseErr.Error())
			return
		}
		readBufferSize = bs.Int64()
	}
	if req.QuotaBytes != "" {
		bs, parseErr := bytesize.ParseByteSize(req.QuotaBytes)
		if parseErr != nil {
			BadRequest(w, "Invalid quota_bytes: "+parseErr.Error())
			return
		}
		quotaBytes = bs.Int64()
	}

	now := time.Now()
	share := &models.Share{
		ID:                 uuid.New().String(),
		Name:               req.Name,
		MetadataStoreID:    metaStore.ID,       // Use actual store ID (UUID), not name
		LocalBlockStoreID:  localBlockStore.ID, // Use actual store ID (UUID), not name
		RemoteBlockStoreID: remoteBlockStoreID, // Nullable
		ReadOnly:           req.ReadOnly,
		EncryptData:        req.EncryptData,
		DefaultPermission:  defaultPerm,
		RetentionPolicy:    string(retPolicy),
		RetentionTTL:       int64(retTTL.Seconds()),
		LocalStoreSize:     localStoreSize,
		ReadBufferSize:     readBufferSize,
		QuotaBytes:         quotaBytes,
		CreatedAt:          now,
		UpdatedAt:          now,
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
			EncryptData:       req.EncryptData,
			DefaultPermission: defaultPerm,
			Squash:            nfsOpts.GetSquashMode(),
			AnonymousUID:      nfsOpts.GetAnonymousUID(),
			AnonymousGID:      nfsOpts.GetAnonymousGID(),
			AllowAuthSys:      nfsOpts.AllowAuthSys,
			AllowAuthSysSet:   true,
			RequireKerberos:   nfsOpts.RequireKerberos,
			MinKerberosLevel:  nfsOpts.MinKerberosLevel,
			BlockedOperations: share.GetBlockedOps(),
			LocalStoreSize:    localStoreSize,
			ReadBufferSize:    readBufferSize,
			QuotaBytes:        quotaBytes,
			LocalBlockStoreID: localBlockStore.ID,
			RetentionPolicy:   retPolicy,
			RetentionTTL:      retTTL,
		}
		if remoteBlockStoreID != nil {
			shareConfig.RemoteBlockStoreID = *remoteBlockStoreID
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
		response[i] = h.shareToResponseWithUsage(s)
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

	WriteJSONOK(w, h.shareToResponseWithUsage(share))
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
	if req.LocalBlockStoreID != nil {
		share.LocalBlockStoreID = *req.LocalBlockStoreID
	}
	if req.RemoteBlockStoreID != nil {
		share.RemoteBlockStoreID = req.RemoteBlockStoreID
	}
	if req.ReadOnly != nil {
		share.ReadOnly = *req.ReadOnly
	}
	if req.EncryptData != nil {
		share.EncryptData = *req.EncryptData
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

	// Handle retention policy updates
	var rtRetPolicy *blockstore.RetentionPolicy
	var rtRetTTL *time.Duration
	if req.RetentionPolicy != nil {
		retPolicy, err := blockstore.ParseRetentionPolicy(*req.RetentionPolicy)
		if err != nil {
			BadRequest(w, err.Error())
			return
		}
		// Determine effective TTL: use new value if provided, else keep existing
		var retTTL time.Duration
		if req.RetentionTTL != nil {
			retTTL, err = time.ParseDuration(*req.RetentionTTL)
			if err != nil {
				BadRequest(w, "Invalid retention TTL duration: "+err.Error())
				return
			}
		} else {
			retTTL = share.GetRetentionTTL()
		}
		if err = blockstore.ValidateRetentionPolicy(retPolicy, retTTL); err != nil {
			BadRequest(w, err.Error())
			return
		}
		share.RetentionPolicy = string(retPolicy)
		share.RetentionTTL = int64(retTTL.Seconds())
		rtRetPolicy = &retPolicy
		rtRetTTL = &retTTL
	} else if req.RetentionTTL != nil {
		// TTL update without policy change -- validate against current policy
		retTTL, err := time.ParseDuration(*req.RetentionTTL)
		if err != nil {
			BadRequest(w, "Invalid retention TTL duration: "+err.Error())
			return
		}
		currentPolicy := share.GetRetentionPolicy()
		if err = blockstore.ValidateRetentionPolicy(currentPolicy, retTTL); err != nil {
			BadRequest(w, err.Error())
			return
		}
		share.RetentionTTL = int64(retTTL.Seconds())
		rtRetTTL = &retTTL
	}

	// Handle per-share size overrides (saved to DB, take effect on restart)
	sizeChanged := false
	if req.LocalStoreSize != nil {
		bs, parseErr := bytesize.ParseByteSize(*req.LocalStoreSize)
		if parseErr != nil {
			BadRequest(w, "Invalid local_store_size: "+parseErr.Error())
			return
		}
		share.LocalStoreSize = bs.Int64()
		sizeChanged = true
	}
	if req.ReadBufferSize != nil {
		bs, parseErr := bytesize.ParseByteSize(*req.ReadBufferSize)
		if parseErr != nil {
			BadRequest(w, "Invalid read_buffer_size: "+parseErr.Error())
			return
		}
		share.ReadBufferSize = bs.Int64()
		sizeChanged = true
	}

	// Handle quota update
	if req.QuotaBytes != nil {
		if *req.QuotaBytes == "" || *req.QuotaBytes == "0" {
			share.QuotaBytes = 0 // Remove quota
		} else {
			bs, parseErr := bytesize.ParseByteSize(*req.QuotaBytes)
			if parseErr != nil {
				BadRequest(w, "Invalid quota_bytes: "+parseErr.Error())
				return
			}
			share.QuotaBytes = bs.Int64()
		}
	}

	share.UpdatedAt = time.Now()

	if err := h.store.UpdateShare(r.Context(), share); err != nil {
		logger.Error("Failed to update share", "share", share.Name, "id", share.ID, "error", err)
		InternalServerError(w, "Failed to update share")
		return
	}

	if sizeChanged {
		logger.Info("Store size override updated for share (takes effect on restart)", "share", share.Name,
			"local_store_size", share.LocalStoreSize, "read_buffer_size", share.ReadBufferSize)
	}

	// Update runtime if available
	if h.runtime != nil {
		if err := h.runtime.UpdateShare(share.Name, req.ReadOnly, req.DefaultPermission, rtRetPolicy, rtRetTTL); err != nil {
			// Log but don't fail - database was updated successfully
			logger.Warn("Failed to update share in runtime", "share", share.Name, "error", err)
		}
		// Hot-update quota if changed
		if req.QuotaBytes != nil {
			h.runtime.UpdateShareQuota(share.Name, share.QuotaBytes)
		}
	}

	WriteJSONOK(w, h.shareToResponseWithUsage(share))
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

// shareToResponse converts a models.Share to ShareResponse (without runtime usage data).
func shareToResponse(s *models.Share) ShareResponse {
	var retTTL string
	if s.RetentionTTL > 0 {
		retTTL = s.GetRetentionTTL().String()
	}
	var localStoreSizeStr, readBufferSizeStr string
	if s.LocalStoreSize > 0 {
		localStoreSizeStr = bytesize.ByteSize(s.LocalStoreSize).String()
	}
	if s.ReadBufferSize > 0 {
		readBufferSizeStr = bytesize.ByteSize(s.ReadBufferSize).String()
	}
	var quotaBytesStr string
	if s.QuotaBytes > 0 {
		quotaBytesStr = bytesize.ByteSize(s.QuotaBytes).String()
	}
	return ShareResponse{
		ID:                 s.ID,
		Name:               s.Name,
		MetadataStoreID:    s.MetadataStoreID,
		LocalBlockStoreID:  s.LocalBlockStoreID,
		RemoteBlockStoreID: s.RemoteBlockStoreID,
		ReadOnly:           s.ReadOnly,
		EncryptData:        s.EncryptData,
		DefaultPermission:  s.DefaultPermission,
		BlockedOperations:  s.GetBlockedOps(),
		RetentionPolicy:    string(s.GetRetentionPolicy()),
		RetentionTTL:       retTTL,
		LocalStoreSize:     localStoreSizeStr,
		ReadBufferSize:     readBufferSizeStr,
		QuotaBytes:         quotaBytesStr,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}
}

// shareToResponseWithUsage converts a models.Share to ShareResponse with runtime usage data.
func (h *ShareHandler) shareToResponseWithUsage(s *models.Share) ShareResponse {
	resp := shareToResponse(s)
	if h.runtime != nil {
		usedBytes, physicalBytes := h.runtime.GetShareUsage(s.Name)
		resp.UsedBytes = usedBytes
		resp.PhysicalBytes = physicalBytes
		if s.QuotaBytes > 0 {
			resp.UsagePercent = float64(usedBytes) / float64(s.QuotaBytes) * 100
			if resp.UsagePercent > 100 {
				resp.UsagePercent = 100
			}
		}
	}
	return resp
}

// isValidBlockedOperation checks if a blocked operation name is valid for any protocol.
func isValidBlockedOperation(op string) bool {
	return isValidNFSOperation(op) || isValidSMBOperation(op)
}
