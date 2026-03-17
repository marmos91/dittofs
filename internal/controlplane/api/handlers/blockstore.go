package handlers

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// BlockStoreStatsHandler handles block store stats and eviction endpoints.
type BlockStoreStatsHandler struct {
	runtime *runtime.Runtime
}

// NewBlockStoreStatsHandler creates a new block store stats handler.
func NewBlockStoreStatsHandler(rt *runtime.Runtime) *BlockStoreStatsHandler {
	return &BlockStoreStatsHandler{runtime: rt}
}

// BlockStoreEvictRequest is the JSON body for block store eviction requests.
type BlockStoreEvictRequest struct {
	ReadBufferOnly bool `json:"read_buffer_only"`
	LocalOnly      bool `json:"local_only"`
}

// Stats handles GET /api/v1/blockstore/stats (global) and GET /api/v1/shares/{name}/blockstore/stats (per-share).
func (h *BlockStoreStatsHandler) Stats(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	shareName := chi.URLParam(r, "name")

	stats, err := h.runtime.GetBlockStoreStats(shareName)
	if err != nil {
		logger.Debug("Block store stats error", "share", shareName, "error", err)
		NotFound(w, err.Error())
		return
	}

	WriteJSONOK(w, stats)
}

// Evict handles POST /api/v1/blockstore/evict (global) and POST /api/v1/shares/{name}/blockstore/evict (per-share).
func (h *BlockStoreStatsHandler) Evict(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	shareName := chi.URLParam(r, "name")

	var req BlockStoreEvictRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	opts := shares.EvictOptions{
		ReadBufferOnly: req.ReadBufferOnly,
		LocalOnly:      req.LocalOnly,
	}

	result, err := h.runtime.EvictBlockStore(r.Context(), shareName, opts)
	if err != nil {
		logger.Debug("Block store evict error", "share", shareName, "error", err)
		BadRequest(w, err.Error())
		return
	}

	WriteJSONOK(w, result)
}
