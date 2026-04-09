package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/health"
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
// Status is non-omitempty so clients can render "unknown" explicitly
// when an adapter is configured but not running.
type AdapterResponse struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Enabled   bool           `json:"enabled"`
	Running   bool           `json:"running"`
	Port      int            `json:"port"`
	Config    map[string]any `json:"config,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Status    health.Report  `json:"status"`
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

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	resp := adapterToResponse(adapter)
	resp.Status = h.statusFor(ctx, adapter.Type)
	WriteJSONCreated(w, resp)
}

// List handles GET /api/v1/adapters.
// Lists all adapter configurations (admin only).
func (h *AdapterHandler) List(w http.ResponseWriter, r *http.Request) {
	adapters, err := h.runtime.Store().ListAdapters(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list adapters")
		return
	}

	// Share a single HealthCheckTimeout budget across the populate
	// loop so N adapters do not compound to N*5s on a cold cache.
	listCtx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	response := make([]AdapterResponse, len(adapters))
	for i, a := range adapters {
		resp := adapterToResponse(a)
		resp.Running = h.runtime.IsAdapterRunning(a.Type)
		resp.Status = h.statusFor(listCtx, a.Type)
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

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	resp := adapterToResponse(adapter)
	resp.Running = h.runtime.IsAdapterRunning(adapterType)
	resp.Status = h.statusFor(ctx, adapterType)
	WriteJSONOK(w, resp)
}

// Status handles GET /api/v1/adapters/{type}/status. Returns 404
// when the adapter config does not exist (matching Get semantics)
// and 200 with a [health.Report] JSON body otherwise.
func (h *AdapterHandler) Status(w http.ResponseWriter, r *http.Request) {
	adapterType := chi.URLParam(r, "type")
	if adapterType == "" {
		BadRequest(w, "Adapter type is required")
		return
	}

	if _, err := h.runtime.Store().GetAdapter(r.Context(), adapterType); err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			NotFound(w, "Adapter not found")
			return
		}
		InternalServerError(w, "Failed to get adapter")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	WriteJSONOK(w, h.statusFor(ctx, adapterType))
}

// statusFor returns a [health.Report] for the named adapter via the
// runtime's cached checker layer. The runtime methods handle a nil
// receiver by returning an "unknown" checker, so no nil check is
// needed here. The caller is responsible for bounding ctx with
// [HealthCheckTimeout]: single-entity /status handlers wrap once at
// the handler level, and list handlers wrap once before the populate
// loop so all entities share a single 5s budget instead of
// compounding to N*5s worst case.
func (h *AdapterHandler) statusFor(ctx context.Context, adapterType string) health.Report {
	return h.runtime.AdapterChecker(adapterType).Healthcheck(ctx)
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

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	resp := adapterToResponse(adapter)
	resp.Running = h.runtime.IsAdapterRunning(adapterType)
	resp.Status = h.statusFor(ctx, adapterType)
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

	// Evict any cached health checker so a later adapter of the same
	// type starts with a fresh probe rather than stale cached output.
	h.runtime.InvalidateAdapterChecker(adapterType)

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
