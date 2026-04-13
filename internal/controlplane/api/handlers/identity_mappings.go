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
	store    store.IdentityMappingStore
	onChange func()
}

// NewIdentityMappingHandler creates a new IdentityMappingHandler.
func NewIdentityMappingHandler(cpStore store.IdentityMappingStore) *IdentityMappingHandler {
	return &IdentityMappingHandler{store: cpStore}
}

// SetOnChange registers a callback invoked after successful create/delete.
func (h *IdentityMappingHandler) SetOnChange(fn func()) {
	h.onChange = fn
}

// CreateIdentityMappingRequest is the request body for POST /api/v1/identity-mappings.
type CreateIdentityMappingRequest struct {
	ProviderName string `json:"provider_name"`
	Principal    string `json:"principal"`
	Username     string `json:"username"`
}

// IdentityMappingResponse is the response body for identity mapping operations.
type IdentityMappingResponse struct {
	ID           string `json:"id"`
	ProviderName string `json:"provider_name"`
	Principal    string `json:"principal"`
	Username     string `json:"username"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// List handles GET /api/v1/identity-mappings.
// Optional query parameter: ?provider=kerberos
func (h *IdentityMappingHandler) List(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")

	mappings, err := h.store.ListIdentityMappings(r.Context(), provider)
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
	if req.ProviderName == "" {
		req.ProviderName = "kerberos"
	}

	mapping := &models.IdentityMapping{
		ProviderName: req.ProviderName,
		Principal:    req.Principal,
		Username:     req.Username,
	}

	if err := h.store.CreateIdentityMapping(r.Context(), mapping); err != nil {
		if errors.Is(err, models.ErrDuplicateMapping) {
			Conflict(w, "Identity mapping for this provider and principal already exists")
			return
		}
		InternalServerError(w, "Failed to create identity mapping")
		return
	}

	if h.onChange != nil {
		h.onChange()
	}

	WriteJSONCreated(w, mappingToResponse(mapping))
}

// Delete handles DELETE /api/v1/identity-mappings/{provider}/{principal}.
func (h *IdentityMappingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if provider == "" {
		BadRequest(w, "Provider is required")
		return
	}

	h.deleteMapping(w, r, provider)
}

// ListForUser handles GET /api/v1/identity-mappings/users/{username}.
func (h *IdentityMappingHandler) ListForUser(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		BadRequest(w, "Username is required")
		return
	}

	mappings, err := h.store.ListIdentityMappingsForUser(r.Context(), username)
	if err != nil {
		InternalServerError(w, "Failed to list identity mappings for user")
		return
	}

	response := make([]IdentityMappingResponse, len(mappings))
	for i, m := range mappings {
		response[i] = mappingToResponse(m)
	}

	WriteJSONOK(w, response)
}

// DeleteLegacy handles DELETE /api/v1/adapters/{type}/identity-mappings/{principal}.
// Backward-compatible endpoint that defaults provider to "kerberos".
func (h *IdentityMappingHandler) DeleteLegacy(w http.ResponseWriter, r *http.Request) {
	h.deleteMapping(w, r, "kerberos")
}

// deleteMapping is the shared implementation for Delete and DeleteLegacy.
func (h *IdentityMappingHandler) deleteMapping(w http.ResponseWriter, r *http.Request, provider string) {
	principal := chi.URLParam(r, "principal")
	if principal == "" {
		BadRequest(w, "Principal is required")
		return
	}

	decodedPrincipal, err := url.PathUnescape(principal)
	if err != nil {
		BadRequest(w, "Invalid principal format")
		return
	}

	if err := h.store.DeleteIdentityMapping(r.Context(), provider, decodedPrincipal); err != nil {
		if errors.Is(err, models.ErrMappingNotFound) {
			NotFound(w, "Identity mapping not found")
			return
		}
		InternalServerError(w, "Failed to delete identity mapping")
		return
	}

	if h.onChange != nil {
		h.onChange()
	}

	WriteNoContent(w)
}

func mappingToResponse(m *models.IdentityMapping) IdentityMappingResponse {
	return IdentityMappingResponse{
		ID:           m.ID,
		ProviderName: m.ProviderName,
		Principal:    m.Principal,
		Username:     m.Username,
		CreatedAt:    m.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    m.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
}
