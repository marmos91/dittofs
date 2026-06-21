package handlers

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// SIDMappingHandler handles foreign-SID idmap admin API endpoints.
//
// These endpoints expose the durable foreign-SID -> Unix UID/GID allocation
// table for administrative inspection and cleanup. The never-remap invariant
// lives on the allocation path; deletion here is the admin escape hatch.
type SIDMappingHandler struct {
	store store.SIDMappingStore
}

// NewSIDMappingHandler creates a new SIDMappingHandler.
func NewSIDMappingHandler(cpStore store.SIDMappingStore) *SIDMappingHandler {
	return &SIDMappingHandler{store: cpStore}
}

// SIDMappingResponse is the response body for SID mapping operations.
type SIDMappingResponse struct {
	SID         string `json:"sid"`
	UnixID      uint32 `json:"unix_id"`
	IsGroup     bool   `json:"is_group"`
	DisplayName string `json:"display_name,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// List handles GET /api/v1/sid-mappings.
func (h *SIDMappingHandler) List(w http.ResponseWriter, r *http.Request) {
	mappings, err := h.store.ListSIDMappings(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list SID mappings")
		return
	}

	WriteJSONOK(w, sidMappingsToResponse(mappings))
}

// Delete handles DELETE /api/v1/sid-mappings/{sid}.
func (h *SIDMappingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	if sid == "" {
		BadRequest(w, "SID is required")
		return
	}

	decodedSID, err := url.PathUnescape(sid)
	if err != nil {
		BadRequest(w, "Invalid SID format")
		return
	}

	if err := h.store.DeleteSIDMapping(r.Context(), decodedSID); err != nil {
		if errors.Is(err, models.ErrSIDMappingNotFound) {
			NotFound(w, "SID mapping not found")
			return
		}
		InternalServerError(w, "Failed to delete SID mapping")
		return
	}

	WriteNoContent(w)
}

func sidMappingsToResponse(mappings []*models.SIDMapping) []SIDMappingResponse {
	response := make([]SIDMappingResponse, len(mappings))
	for i, m := range mappings {
		response[i] = sidMappingToResponse(m)
	}
	return response
}

func sidMappingToResponse(m *models.SIDMapping) SIDMappingResponse {
	return SIDMappingResponse{
		SID:         m.SID,
		UnixID:      m.UnixID,
		IsGroup:     m.IsGroup,
		DisplayName: m.DisplayName,
		CreatedAt:   m.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}
