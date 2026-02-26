package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// MetadataStoreHandler handles metadata store configuration API endpoints.
type MetadataStoreHandler struct {
	store   store.MetadataStoreConfigStore
	runtime *runtime.Runtime
}

// NewMetadataStoreHandler creates a new MetadataStoreHandler.
func NewMetadataStoreHandler(s store.MetadataStoreConfigStore, rt *runtime.Runtime) *MetadataStoreHandler {
	return &MetadataStoreHandler{store: s, runtime: rt}
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
// Also creates the actual metadata store instance and registers it with the runtime.
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

	storeCfg := &models.MetadataStoreConfig{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Type:      req.Type,
		Config:    req.Config,
		CreatedAt: time.Now(),
	}

	// Validate store can be created before persisting configuration
	// This prevents inconsistent state where config exists but store cannot be instantiated
	var metaStore metadata.MetadataStore
	if h.runtime != nil {
		var err error
		metaStore, err = runtime.CreateMetadataStoreFromConfig(r.Context(), storeCfg.Type, storeCfg)
		if err != nil {
			logger.Error("Failed to create metadata store instance",
				"name", req.Name, "type", req.Type, "error", err)
			BadRequest(w, "Failed to create metadata store: "+err.Error())
			return
		}
	}

	// Store creation succeeded, now persist the configuration
	if _, err := h.store.CreateMetadataStore(r.Context(), storeCfg); err != nil {
		if errors.Is(err, models.ErrDuplicateStore) {
			Conflict(w, "Metadata store already exists")
			return
		}
		InternalServerError(w, "Failed to create metadata store")
		return
	}

	// Register with runtime (store already validated above)
	if h.runtime != nil && metaStore != nil {
		if err := h.runtime.RegisterMetadataStore(req.Name, metaStore); err != nil {
			// Config saved but registration failed - log warning
			// Store will be registered on next server restart
			logger.Warn("Metadata store created but failed to register with runtime",
				"name", req.Name, "error", err)
		} else {
			logger.Info("Metadata store created and registered", "name", req.Name, "type", req.Type)
		}
	}

	WriteJSONCreated(w, metadataStoreToResponse(storeCfg))
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
		if errors.Is(err, models.ErrStoreInUse) {
			Conflict(w, "Cannot delete metadata store: it is in use by one or more shares")
			return
		}
		logger.Error("Failed to delete metadata store", "name", name, "error", err)
		InternalServerError(w, "Failed to delete metadata store")
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
