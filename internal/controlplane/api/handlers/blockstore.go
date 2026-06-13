package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// BlockStoreRuntime is the narrow Runtime surface needed by
// BlockStoreStatsHandler. Defining the interface here (rather than depending on
// *runtime.Runtime directly) keeps the handler unit-testable: tests substitute
// a fake that returns canned responses, mirroring the BlockGCRuntime idiom in
// block_gc.go. *runtime.Runtime satisfies this interface implicitly.
type BlockStoreRuntime interface {
	GetBlockStoreStats(shareName string) (*shares.BlockStoreStatsResponse, error)
	EvictBlockStore(ctx context.Context, shareName string, opts shares.EvictOptions) (*shares.EvictResult, error)
}

// BlockStoreStatsHandler handles block store stats and eviction endpoints.
type BlockStoreStatsHandler struct {
	runtime BlockStoreRuntime
}

// NewBlockStoreStatsHandler creates a new block store stats handler.
func NewBlockStoreStatsHandler(rt BlockStoreRuntime) *BlockStoreStatsHandler {
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
		// Strip the underlying err string from the response body — it
		// echoes the share path and storage-config detail verbatim. The
		// full error is logged at Debug for operator postmortems.
		logger.Debug("block store stats error", "share", shareName, "error", err)
		NotFound(w, "share not found")
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
		// Strip the underlying err string from the response body — it
		// leaks the share path and storage topology (e.g. "no remote store
		// configured"). The full error is logged at Debug.
		logger.Debug("block store evict error", "share", shareName, "error", err)
		BadRequest(w, "eviction failed")
		return
	}

	WriteJSONOK(w, result)
}
