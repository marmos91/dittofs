package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// AdapterHandler handles adapter configuration API endpoints.
type AdapterHandler struct {
	store store.Store
}

// NewAdapterHandler creates a new AdapterHandler.
func NewAdapterHandler(store store.Store) *AdapterHandler {
	return &AdapterHandler{store: store}
}

// CreateAdapterRequest is the request body for POST /api/v1/adapters.
type CreateAdapterRequest struct {
	Type    string         `json:"type"`
	Enabled *bool          `json:"enabled,omitempty"`
	Port    int            `json:"port,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
}

// UpdateAdapterRequest is the request body for PUT /api/v1/adapters/{type}.
type UpdateAdapterRequest struct {
	Enabled *bool          `json:"enabled,omitempty"`
	Port    *int           `json:"port,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
}

// AdapterResponse is the response body for adapter endpoints.
type AdapterResponse struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Enabled   bool           `json:"enabled"`
	Port      int            `json:"port"`
	Config    map[string]any `json:"config,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Create handles POST /api/v1/adapters.
// Creates a new adapter configuration (admin only).
func (h *AdapterHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateAdapterRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Type == "" {
		BadRequest(w, "Adapter type is required")
		return
	}

	// Default enabled to true if not specified
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	adapter := &models.AdapterConfig{
		ID:        uuid.New().String(),
		Type:      req.Type,
		Enabled:   enabled,
		Port:      req.Port,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Set config if provided
	if req.Config != nil {
		if err := adapter.SetConfig(req.Config); err != nil {
			BadRequest(w, "Invalid config format")
			return
		}
	}

	if _, err := h.store.CreateAdapter(r.Context(), adapter); err != nil {
		if errors.Is(err, models.ErrDuplicateAdapter) {
			Conflict(w, "Adapter already exists")
			return
		}
		InternalServerError(w, "Failed to create adapter")
		return
	}

	WriteJSONCreated(w, adapterToResponse(adapter))
}

// List handles GET /api/v1/adapters.
// Lists all adapter configurations (admin only).
func (h *AdapterHandler) List(w http.ResponseWriter, r *http.Request) {
	adapters, err := h.store.ListAdapters(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list adapters")
		return
	}

	response := make([]AdapterResponse, len(adapters))
	for i, a := range adapters {
		response[i] = adapterToResponse(a)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/adapters/{type}.
// Gets an adapter configuration by type (admin only).
func (h *AdapterHandler) Get(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	if adapterType == "" {
		BadRequest(w, "Adapter type is required")
		return
	}

	adapter, err := h.store.GetAdapter(r.Context(), adapterType)
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	WriteJSONOK(w, adapterToResponse(adapter))
}

// Update handles PUT /api/v1/adapters/{type}.
// Updates an adapter configuration (admin only).
func (h *AdapterHandler) Update(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	if adapterType == "" {
		BadRequest(w, "Adapter type is required")
		return
	}

	var req UpdateAdapterRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Fetch existing adapter
	adapter, err := h.store.GetAdapter(r.Context(), adapterType)
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	// Apply updates
	if req.Enabled != nil {
		adapter.Enabled = *req.Enabled
	}
	if req.Port != nil {
		adapter.Port = *req.Port
	}
	if req.Config != nil {
		if err := adapter.SetConfig(req.Config); err != nil {
			BadRequest(w, "Invalid config format")
			return
		}
	}

	if err := h.store.UpdateAdapter(r.Context(), adapter); err != nil {
		InternalServerError(w, "Failed to update adapter")
		return
	}

	WriteJSONOK(w, adapterToResponse(adapter))
}

// Delete handles DELETE /api/v1/adapters/{type}.
// Deletes an adapter configuration (admin only).
func (h *AdapterHandler) Delete(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	if adapterType == "" {
		BadRequest(w, "Adapter type is required")
		return
	}

	if err := h.store.DeleteAdapter(r.Context(), adapterType); err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to delete adapter")
		return
	}

	WriteNoContent(w)
}

// adapterToResponse converts a models.AdapterConfig to AdapterResponse.
func adapterToResponse(a *models.AdapterConfig) AdapterResponse {
	config, _ := a.GetConfig()
	return AdapterResponse{
		ID:        a.ID,
		Type:      a.Type,
		Enabled:   a.Enabled,
		Port:      a.Port,
		Config:    config,
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
}

// decodeJSONBody is needed for the json decoder
func init() {
	// Ensure json is imported
	_ = json.Marshal
}
