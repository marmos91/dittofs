package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// CacheHandler handles cache management endpoints.
type CacheHandler struct {
	runtime *runtime.Runtime
}

// NewCacheHandler creates a new cache handler.
func NewCacheHandler(rt *runtime.Runtime) *CacheHandler {
	return &CacheHandler{runtime: rt}
}

// CacheEvictRequest is the JSON body for cache eviction requests.
type CacheEvictRequest struct {
	L1Only    bool `json:"l1_only"`
	LocalOnly bool `json:"local_only"`
}

// Stats handles GET /api/v1/cache/stats (global) and GET /api/v1/shares/{name}/cache/stats (per-share).
func (h *CacheHandler) Stats(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	shareName := chi.URLParam(r, "name")

	stats, err := h.runtime.GetCacheStats(shareName)
	if err != nil {
		logger.Debug("Cache stats error", "share", shareName, "error", err)
		NotFound(w, err.Error())
		return
	}

	WriteJSONOK(w, stats)
}

// Evict handles POST /api/v1/cache/evict (global) and POST /api/v1/shares/{name}/cache/evict (per-share).
func (h *CacheHandler) Evict(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	shareName := chi.URLParam(r, "name")

	var req CacheEvictRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	opts := shares.EvictOptions{
		L1Only:    req.L1Only,
		LocalOnly: req.LocalOnly,
	}

	result, err := h.runtime.EvictCache(r.Context(), shareName, opts)
	if err != nil {
		logger.Debug("Cache evict error", "share", shareName, "error", err)
		// Check if it's a safety error (no remote store)
		BadRequest(w, err.Error())
		return
	}

	WriteJSONOK(w, result)
}
