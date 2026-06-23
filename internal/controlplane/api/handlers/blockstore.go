package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

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
	StartWarmBlockStore(ctx context.Context, shareName string) (*shares.WarmJob, error)
	GetWarmStatus(jobID string) (*shares.WarmJob, bool)
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

	// The per-share route carries {name}; the global route does not. An empty
	// param means "all shares" — only normalize (prepend the registry's leading
	// slash) when a share name is actually present.
	shareName := chi.URLParam(r, "name")
	if shareName != "" {
		shareName = normalizeShareName(shareName)
	}

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

	// As in Stats: empty {name} is the global route ("all shares"); only
	// normalize when a per-share name is actually present.
	shareName := chi.URLParam(r, "name")
	if shareName != "" {
		shareName = normalizeShareName(shareName)
	}

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

// WarmJobStatusResponse is the JSON status body for a warm job, returned by
// both Warm (202) and WarmStatus (200). Mirrors shares.WarmJob's wire shape.
type WarmJobStatusResponse struct {
	ID          string `json:"id"`
	Share       string `json:"share"`
	State       string `json:"state"`
	BlocksTotal int64  `json:"blocks_total"`
	BlocksDone  int64  `json:"blocks_done"`
	BytesDone   int64  `json:"bytes_done"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Error       string `json:"error,omitempty"`
}

func warmJobToResponse(j *shares.WarmJob) WarmJobStatusResponse {
	resp := WarmJobStatusResponse{
		ID:          j.ID,
		Share:       j.Share,
		State:       j.State,
		BlocksTotal: j.BlocksTotal,
		BlocksDone:  j.BlocksDone,
		BytesDone:   j.BytesDone,
		Error:       j.Err,
	}
	if !j.StartedAt.IsZero() {
		resp.StartedAt = j.StartedAt.UTC().Format(time.RFC3339)
	}
	if !j.FinishedAt.IsZero() {
		resp.FinishedAt = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// Warm handles POST /api/v1/shares/{name}/blockstore/warm. It starts (or
// returns the already-running) async warm job that materializes the share's
// blocks onto the local tier and responds 202 with the job id + initial status.
func (h *BlockStoreStatsHandler) Warm(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	shareName := chi.URLParam(r, "name")
	if shareName == "" {
		BadRequest(w, "share name required")
		return
	}
	shareName = normalizeShareName(shareName)

	job, err := h.runtime.StartWarmBlockStore(r.Context(), shareName)
	if err != nil {
		logger.Debug("block store warm start error", "share", shareName, "error", err)
		BadRequest(w, "warm failed")
		return
	}

	WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": warmJobToResponse(job),
	})
}

// WarmStatus handles GET /api/v1/shares/{name}/blockstore/warm/{job_id}. It
// returns the current status of a warm job. The {name} segment is accepted for
// route symmetry with the start endpoint but the job id alone identifies the
// job.
func (h *BlockStoreStatsHandler) WarmStatus(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	jobID := chi.URLParam(r, "job_id")
	if jobID == "" {
		BadRequest(w, "job id required")
		return
	}

	job, ok := h.runtime.GetWarmStatus(jobID)
	if !ok {
		NotFound(w, "warm job not found")
		return
	}

	WriteJSONOK(w, warmJobToResponse(job))
}
