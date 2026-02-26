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

// PayloadStoreHandler handles payload store configuration API endpoints.
type PayloadStoreHandler struct {
	store store.PayloadStoreConfigStore
}

// NewPayloadStoreHandler creates a new PayloadStoreHandler.
func NewPayloadStoreHandler(s store.PayloadStoreConfigStore) *PayloadStoreHandler {
	return &PayloadStoreHandler{store: s}
}

// CreatePayloadStoreRequest is the request body for POST /api/v1/payload-stores.
type CreatePayloadStoreRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config string `json:"config,omitempty"` // JSON string for type-specific config
}

// UpdatePayloadStoreRequest is the request body for PUT /api/v1/payload-stores/{name}.
type UpdatePayloadStoreRequest struct {
	Type   *string `json:"type,omitempty"`
	Config *string `json:"config,omitempty"`
}

// PayloadStoreResponse is the response body for payload store endpoints.
type PayloadStoreResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Config    string    `json:"config,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Create handles POST /api/v1/payload-stores.
// Creates a new payload store configuration (admin only).
func (h *PayloadStoreHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreatePayloadStoreRequest
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

	store := &models.PayloadStoreConfig{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Type:      req.Type,
		Config:    req.Config,
		CreatedAt: time.Now(),
	}

	if _, err := h.store.CreatePayloadStore(r.Context(), store); err != nil {
		if errors.Is(err, models.ErrDuplicateStore) {
			Conflict(w, "Payload store already exists")
			return
		}
		InternalServerError(w, "Failed to create payload store")
		return
	}

	WriteJSONCreated(w, payloadStoreToResponse(store))
}

// List handles GET /api/v1/payload-stores.
// Lists all payload store configurations (admin only).
func (h *PayloadStoreHandler) List(w http.ResponseWriter, r *http.Request) {
	stores, err := h.store.ListPayloadStores(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list payload stores")
		return
	}

	response := make([]PayloadStoreResponse, len(stores))
	for i, s := range stores {
		response[i] = payloadStoreToResponse(s)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/payload-stores/{name}.
// Gets a payload store configuration by name (admin only).
func (h *PayloadStoreHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	store, err := h.store.GetPayloadStore(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Payload store not found")
			return
		}
		InternalServerError(w, "Failed to get payload store")
		return
	}

	WriteJSONOK(w, payloadStoreToResponse(store))
}

// Update handles PUT /api/v1/payload-stores/{name}.
// Updates a payload store configuration (admin only).
func (h *PayloadStoreHandler) Update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req UpdatePayloadStoreRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Fetch existing store
	store, err := h.store.GetPayloadStore(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Payload store not found")
			return
		}
		InternalServerError(w, "Failed to get payload store")
		return
	}

	// Apply updates
	if req.Type != nil {
		store.Type = *req.Type
	}
	if req.Config != nil {
		store.Config = *req.Config
	}

	if err := h.store.UpdatePayloadStore(r.Context(), store); err != nil {
		InternalServerError(w, "Failed to update payload store")
		return
	}

	WriteJSONOK(w, payloadStoreToResponse(store))
}

// Delete handles DELETE /api/v1/payload-stores/{name}.
// Deletes a payload store configuration (admin only).
func (h *PayloadStoreHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	if err := h.store.DeletePayloadStore(r.Context(), name); err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Payload store not found")
			return
		}
		if errors.Is(err, models.ErrStoreInUse) {
			Conflict(w, "Cannot delete payload store: it is in use by one or more shares")
			return
		}
		InternalServerError(w, "Failed to delete payload store")
		return
	}

	WriteNoContent(w)
}

// payloadStoreToResponse converts a models.PayloadStoreConfig to PayloadStoreResponse.
func payloadStoreToResponse(s *models.PayloadStoreConfig) PayloadStoreResponse {
	return PayloadStoreResponse{
		ID:        s.ID,
		Name:      s.Name,
		Type:      s.Type,
		Config:    s.Config,
		CreatedAt: s.CreatedAt,
	}
}
