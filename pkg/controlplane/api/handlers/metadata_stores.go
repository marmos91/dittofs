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

// MetadataStoreHandler handles metadata store configuration API endpoints.
type MetadataStoreHandler struct {
	store store.Store
}

// NewMetadataStoreHandler creates a new MetadataStoreHandler.
func NewMetadataStoreHandler(store store.Store) *MetadataStoreHandler {
	return &MetadataStoreHandler{store: store}
}

// CreateMetadataStoreRequest is the request body for POST /api/v1/metadata-stores.
type CreateMetadataStoreRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config string `json:"config,omitempty"` // JSON string for type-specific config
}

// UpdateMetadataStoreRequest is the request body for PUT /api/v1/metadata-stores/{name}.
type UpdateMetadataStoreRequest struct {
	Type   *string `json:"type,omitempty"`
	Config *string `json:"config,omitempty"`
}

// MetadataStoreResponse is the response body for metadata store endpoints.
type MetadataStoreResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Config    string    `json:"config,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Create handles POST /api/v1/metadata-stores.
// Creates a new metadata store configuration (admin only).
func (h *MetadataStoreHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateMetadataStoreRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Name == "" {
		BadRequest(w, "Store name is required")
		return
	}
	if req.Type == "" {
		BadRequest(w, "Store type is required")
		return
	}

	store := &models.MetadataStoreConfig{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Type:      req.Type,
		Config:    req.Config,
		CreatedAt: time.Now(),
	}

	if _, err := h.store.CreateMetadataStore(r.Context(), store); err != nil {
		if errors.Is(err, models.ErrDuplicateStore) {
			Conflict(w, "Metadata store already exists")
			return
		}
		InternalServerError(w, "Failed to create metadata store")
		return
	}

	WriteJSONCreated(w, metadataStoreToResponse(store))
}

// List handles GET /api/v1/metadata-stores.
// Lists all metadata store configurations (admin only).
func (h *MetadataStoreHandler) List(w http.ResponseWriter, r *http.Request) {
	stores, err := h.store.ListMetadataStores(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list metadata stores")
		return
	}

	response := make([]MetadataStoreResponse, len(stores))
	for i, s := range stores {
		response[i] = metadataStoreToResponse(s)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/metadata-stores/{name}.
// Gets a metadata store configuration by name (admin only).
func (h *MetadataStoreHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	store, err := h.store.GetMetadataStore(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return
		}
		InternalServerError(w, "Failed to get metadata store")
		return
	}

	WriteJSONOK(w, metadataStoreToResponse(store))
}

// Update handles PUT /api/v1/metadata-stores/{name}.
// Updates a metadata store configuration (admin only).
func (h *MetadataStoreHandler) Update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req UpdateMetadataStoreRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Fetch existing store
	store, err := h.store.GetMetadataStore(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return
		}
		InternalServerError(w, "Failed to get metadata store")
		return
	}

	// Apply updates
	if req.Type != nil {
		store.Type = *req.Type
	}
	if req.Config != nil {
		store.Config = *req.Config
	}

	if err := h.store.UpdateMetadataStore(r.Context(), store); err != nil {
		InternalServerError(w, "Failed to update metadata store")
		return
	}

	WriteJSONOK(w, metadataStoreToResponse(store))
}

// Delete handles DELETE /api/v1/metadata-stores/{name}.
// Deletes a metadata store configuration (admin only).
func (h *MetadataStoreHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	if err := h.store.DeleteMetadataStore(r.Context(), name); err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return
		}
		// Check if store is in use
		Conflict(w, "Cannot delete metadata store: it may be in use by shares")
		return
	}

	WriteNoContent(w)
}

// metadataStoreToResponse converts a models.MetadataStoreConfig to MetadataStoreResponse.
func metadataStoreToResponse(s *models.MetadataStoreConfig) MetadataStoreResponse {
	return MetadataStoreResponse{
		ID:        s.ID,
		Name:      s.Name,
		Type:      s.Type,
		Config:    s.Config,
		CreatedAt: s.CreatedAt,
	}
}
