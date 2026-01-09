package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/pkg/identity"
)

// ShareMappingHandler handles share identity mapping API endpoints.
type ShareMappingHandler struct {
	identityStore identity.IdentityStore
}

// NewShareMappingHandler creates a new ShareMappingHandler.
func NewShareMappingHandler(identityStore identity.IdentityStore) *ShareMappingHandler {
	return &ShareMappingHandler{identityStore: identityStore}
}

// ShareMappingRequest is the request body for setting a share identity mapping.
type ShareMappingRequest struct {
	UID       uint32   `json:"uid"`
	GID       uint32   `json:"gid"`
	GIDs      []uint32 `json:"gids,omitempty"`
	SID       string   `json:"sid,omitempty"`
	GroupSIDs []string `json:"group_sids,omitempty"`
}

// ShareMappingResponse is the response for share identity mapping endpoints.
type ShareMappingResponse struct {
	Username  string   `json:"username"`
	ShareName string   `json:"share_name"`
	UID       uint32   `json:"uid"`
	GID       uint32   `json:"gid"`
	GIDs      []uint32 `json:"gids,omitempty"`
	SID       string   `json:"sid,omitempty"`
	GroupSIDs []string `json:"group_sids,omitempty"`
}

// List handles GET /api/v1/users/{username}/shares.
// Lists all share identity mappings for a user (admin only).
func (h *ShareMappingHandler) List(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	mappings, err := h.identityStore.ListUserShareMappings(username)
	if err != nil {
		if err == identity.ErrUserNotFound {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to list share mappings")
		return
	}

	response := make([]ShareMappingResponse, len(mappings))
	for i, m := range mappings {
		response[i] = mappingToResponse(m)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/users/{username}/shares/{share}.
// Gets a specific share identity mapping (admin only).
func (h *ShareMappingHandler) Get(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	shareName := chi.URLParam(r, "share")

	if username == "" || shareName == "" {
		BadRequest(w, "Username and share name are required")
		return
	}

	// URL-decode the share name (handles slashes like /export -> %2Fexport)
	shareName = "/" + shareName // Prepend slash since it's a path

	mapping, err := h.identityStore.GetShareIdentityMapping(username, shareName)
	if err != nil {
		if err == identity.ErrUserNotFound {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to get share mapping")
		return
	}

	if mapping == nil {
		NotFound(w, "Share mapping not found")
		return
	}

	WriteJSONOK(w, mappingToResponse(mapping))
}

// Set handles PUT /api/v1/users/{username}/shares/{share}.
// Creates or updates a share identity mapping (admin only).
func (h *ShareMappingHandler) Set(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	shareName := chi.URLParam(r, "share")

	if username == "" || shareName == "" {
		BadRequest(w, "Username and share name are required")
		return
	}

	// URL-decode the share name
	shareName = "/" + shareName

	var req ShareMappingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}

	mapping := &identity.ShareIdentityMapping{
		Username:  username,
		ShareName: shareName,
		UID:       req.UID,
		GID:       req.GID,
		GIDs:      req.GIDs,
		SID:       req.SID,
		GroupSIDs: req.GroupSIDs,
	}

	if err := h.identityStore.SetShareIdentityMapping(r.Context(), mapping); err != nil {
		if err == identity.ErrUserNotFound {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to set share mapping")
		return
	}

	WriteJSONOK(w, mappingToResponse(mapping))
}

// Delete handles DELETE /api/v1/users/{username}/shares/{share}.
// Deletes a share identity mapping (admin only).
func (h *ShareMappingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	shareName := chi.URLParam(r, "share")

	if username == "" || shareName == "" {
		BadRequest(w, "Username and share name are required")
		return
	}

	// URL-decode the share name
	shareName = "/" + shareName

	if err := h.identityStore.DeleteShareIdentityMapping(r.Context(), username, shareName); err != nil {
		if err == identity.ErrUserNotFound {
			NotFound(w, "User not found")
			return
		}
		InternalServerError(w, "Failed to delete share mapping")
		return
	}

	WriteNoContent(w)
}

// mappingToResponse converts a ShareIdentityMapping to ShareMappingResponse.
func mappingToResponse(m *identity.ShareIdentityMapping) ShareMappingResponse {
	return ShareMappingResponse{
		Username:  m.Username,
		ShareName: m.ShareName,
		UID:       m.UID,
		GID:       m.GID,
		GIDs:      m.GIDs,
		SID:       m.SID,
		GroupSIDs: m.GroupSIDs,
	}
}
