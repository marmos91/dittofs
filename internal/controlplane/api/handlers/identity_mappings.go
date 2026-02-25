package handlers

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// IdentityMappingHandler handles identity mapping API endpoints.
type IdentityMappingHandler struct {
	store store.IdentityMappingStore
}

// NewIdentityMappingHandler creates a new IdentityMappingHandler.
// The cpStore must implement store.IdentityMappingStore (GORMStore does).
func NewIdentityMappingHandler(cpStore store.IdentityMappingStore) *IdentityMappingHandler {
	return &IdentityMappingHandler{store: cpStore}
}

// CreateIdentityMappingRequest is the request body for POST /api/v1/identity-mappings.
type CreateIdentityMappingRequest struct {
	Principal string `json:"principal"`
	Username  string `json:"username"`
}

// IdentityMappingResponse is the response body for identity mapping operations.
type IdentityMappingResponse struct {
	ID        string `json:"id"`
	Principal string `json:"principal"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// List handles GET /api/v1/identity-mappings.
// Lists all identity mappings (admin only).
func (h *IdentityMappingHandler) List(w http.ResponseWriter, r *http.Request) {
	mappings, err := h.store.ListIdentityMappings(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list identity mappings")
		return
	}

	response := make([]IdentityMappingResponse, len(mappings))
	for i, m := range mappings {
		response[i] = mappingToResponse(m)
	}

	WriteJSONOK(w, response)
}

// Create handles POST /api/v1/identity-mappings.
// Creates a new identity mapping (admin only).
func (h *IdentityMappingHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateIdentityMappingRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Principal == "" {
		BadRequest(w, "Principal is required")
		return
	}
	if req.Username == "" {
		BadRequest(w, "Username is required")
		return
	}

	mapping := &models.IdentityMapping{
		Principal: req.Principal,
		Username:  req.Username,
	}

	if err := h.store.CreateIdentityMapping(r.Context(), mapping); err != nil {
		if errors.Is(err, models.ErrDuplicateMapping) {
			Conflict(w, "Identity mapping for this principal already exists")
			return
		}
		InternalServerError(w, "Failed to create identity mapping")
		return
	}

	WriteJSONCreated(w, mappingToResponse(mapping))
}

// Delete handles DELETE /api/v1/identity-mappings/{principal}.
// Deletes an identity mapping by principal (admin only).
func (h *IdentityMappingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	principal := chi.URLParam(r, "principal")
	if principal == "" {
		BadRequest(w, "Principal is required")
		return
	}

	// URL-decode the principal (it may contain @ and other special characters)
	decodedPrincipal, err := url.PathUnescape(principal)
	if err != nil {
		BadRequest(w, "Invalid principal format")
		return
	}

	if err := h.store.DeleteIdentityMapping(r.Context(), decodedPrincipal); err != nil {
		if errors.Is(err, models.ErrMappingNotFound) {
			NotFound(w, "Identity mapping not found")
			return
		}
		InternalServerError(w, "Failed to delete identity mapping")
		return
	}

	WriteNoContent(w)
}

// mappingToResponse converts an IdentityMapping model to an API response.
func mappingToResponse(m *models.IdentityMapping) IdentityMappingResponse {
	return IdentityMappingResponse{
		ID:        m.ID,
		Principal: m.Principal,
		Username:  m.Username,
		CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt: m.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
}
