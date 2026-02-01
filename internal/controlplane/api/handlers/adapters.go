package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// AdapterHandler handles adapter configuration API endpoints.
// It uses Runtime methods to ensure both persistent store and in-memory
// adapter state are updated together.
type AdapterHandler struct {
	runtime *runtime.Runtime
}

// NewAdapterHandler creates a new AdapterHandler.
func NewAdapterHandler(rt *runtime.Runtime) *AdapterHandler {
	return &AdapterHandler{runtime: rt}
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
	Running   bool           `json:"running"`
	Port      int            `json:"port"`
	Config    map[string]any `json:"config,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Create handles POST /api/v1/adapters.
// Creates a new adapter configuration AND starts it immediately (admin only).
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

	// CreateAdapter saves to store AND starts the adapter
	if err := h.runtime.CreateAdapter(r.Context(), adapter); err != nil {
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
	adapters, err := h.runtime.Store().ListAdapters(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list adapters")
		return
	}

	response := make([]AdapterResponse, len(adapters))
	for i, a := range adapters {
		resp := adapterToResponse(a)
		// Add running status
		resp.Running = h.runtime.IsAdapterRunning(a.Type)
		response[i] = resp
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

	adapter, err := h.runtime.Store().GetAdapter(r.Context(), adapterType)
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	resp := adapterToResponse(adapter)
	resp.Running = h.runtime.IsAdapterRunning(adapterType)
	WriteJSONOK(w, resp)
}

// Update handles PUT /api/v1/adapters/{type}.
// Updates an adapter configuration AND restarts with new config (admin only).
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
	adapter, err := h.runtime.Store().GetAdapter(r.Context(), adapterType)
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

	// UpdateAdapter updates store AND restarts the adapter
	if err := h.runtime.UpdateAdapter(r.Context(), adapter); err != nil {
		InternalServerError(w, "Failed to update adapter")
		return
	}

	resp := adapterToResponse(adapter)
	resp.Running = h.runtime.IsAdapterRunning(adapterType)
	WriteJSONOK(w, resp)
}

// Delete handles DELETE /api/v1/adapters/{type}.
// Stops the adapter AND deletes configuration (admin only).
func (h *AdapterHandler) Delete(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	if adapterType == "" {
		BadRequest(w, "Adapter type is required")
		return
	}

	// DeleteAdapter stops the running adapter AND removes from store
	if err := h.runtime.DeleteAdapter(r.Context(), adapterType); err != nil {
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
