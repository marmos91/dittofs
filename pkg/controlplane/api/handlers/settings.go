package handlers

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// SettingsHandler handles system settings API endpoints.
type SettingsHandler struct {
	store store.Store
}

// NewSettingsHandler creates a new SettingsHandler.
func NewSettingsHandler(store store.Store) *SettingsHandler {
	return &SettingsHandler{store: store}
}

// SetSettingRequest is the request body for PUT /api/v1/settings/{key}.
type SetSettingRequest struct {
	Value string `json:"value"`
}

// SettingResponse is the response body for setting endpoints.
type SettingResponse struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// List handles GET /api/v1/settings.
// Lists all system settings (admin only).
func (h *SettingsHandler) List(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.ListSettings(r.Context())
	if err != nil {
		InternalServerError(w, "Failed to list settings")
		return
	}

	response := make([]SettingResponse, len(settings))
	for i, s := range settings {
		response[i] = settingToResponse(s)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/settings/{key}.
// Gets a setting by key (admin only).
func (h *SettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		BadRequest(w, "Setting key is required")
		return
	}

	value, err := h.store.GetSetting(r.Context(), key)
	if err != nil {
		NotFound(w, "Setting not found")
		return
	}

	// For Get, we don't have UpdatedAt from the store method, so we use current time
	// This could be improved by returning the full Setting struct
	WriteJSONOK(w, map[string]string{
		"key":   key,
		"value": value,
	})
}

// Set handles PUT /api/v1/settings/{key}.
// Creates or updates a setting (admin only).
func (h *SettingsHandler) Set(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		BadRequest(w, "Setting key is required")
		return
	}

	var req SetSettingRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if err := h.store.SetSetting(r.Context(), key, req.Value); err != nil {
		InternalServerError(w, "Failed to set setting")
		return
	}

	WriteJSONOK(w, map[string]string{
		"key":   key,
		"value": req.Value,
	})
}

// Delete handles DELETE /api/v1/settings/{key}.
// Deletes a setting (admin only).
func (h *SettingsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		BadRequest(w, "Setting key is required")
		return
	}

	if err := h.store.DeleteSetting(r.Context(), key); err != nil {
		// DeleteSetting may not return specific errors, treat all as success
		// or internal error depending on implementation
		InternalServerError(w, "Failed to delete setting")
		return
	}

	WriteNoContent(w)
}

// settingToResponse converts a models.Setting to SettingResponse.
func settingToResponse(s *models.Setting) SettingResponse {
	return SettingResponse{
		Key:       s.Key,
		Value:     s.Value,
		UpdatedAt: s.UpdatedAt,
	}
}
