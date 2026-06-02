package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// BlockAuditRuntime is the narrow Runtime surface needed by
// BlockStoreAuditHandler. Defining the interface here (rather than
// depending on *runtime.Runtime directly) keeps the handler unit-testable
// — tests substitute a fake that records calls and returns canned
// responses, mirroring testBlockStoreHandler in blockstore_test.go and
// fakeGCRuntime in block_gc_test.go.
type BlockAuditRuntime interface {
	// AuditRefcounts dispatches a refcount reconciliation walk for the
	// named share and persists last-inv02.json under the share's
	// audit-state directory.
	AuditRefcounts(ctx context.Context, shareName string) (*engine.AuditRefcountsResult, error)
}

// BlockStoreAuditHandler exposes the on-demand refcount audit endpoint.
type BlockStoreAuditHandler struct {
	runtime BlockAuditRuntime
}

// NewBlockStoreAuditHandler constructs a handler bound to the given
// Runtime surface. Pass a nil-safe value: the handler refuses requests
// when runtime is nil so the server can still boot in degraded modes.
func NewBlockStoreAuditHandler(rt BlockAuditRuntime) *BlockStoreAuditHandler {
	return &BlockStoreAuditHandler{runtime: rt}
}

// BlockStoreAuditResponse wraps engine.AuditRefcountsResult for JSON
// output. Returned by POST /api/v1/shares/{name}/audit/refcounts.
type BlockStoreAuditResponse struct {
	Result *engine.AuditRefcountsResult `json:"result"`
}

// RunAudit handles POST /api/v1/shares/{name}/audit/refcounts.
//
// Behavior: invokes Runtime.AuditRefcounts with the URL share name.
// The audit walks the share's metadata store, computing
// ∑ FileBlock.RefCount and ∑ len(FileAttr.Blocks); a non-zero delta
// indicates refcount drift. Last-run summary is persisted under
// <localStoreRoot>/audit-state/last-inv02.json.
//
// Status codes:
//   - 200 OK with BlockStoreAuditResponse on success
//   - 400 Bad Request when {name} is empty
//   - 404 Not Found when {name} is not a registered share
//   - 500 Internal Server Error on unexpected runtime errors
func (h *BlockStoreAuditHandler) RunAudit(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}

	res, err := h.runtime.AuditRefcounts(r.Context(), name)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+name)
			return
		}
		logger.Debug("Block store INV-02 audit error", "share", name, "error", err)
		// Strip the underlying err string from the response body — it
		// can leak filesystem paths or other internal details. Details
		// are logged at Debug above for operator postmortems (mirrors
		// the GC handler's behavior).
		InternalServerError(w, "audit failed")
		return
	}

	// Structured slog INFO for the audit result so operators can grep
	// refcount violations without scraping HTTP logs.
	logger.Info("Block store INV-02 audit complete",
		"share", name,
		"total_files", res.TotalFiles,
		"total_refs", res.TotalRefs,
		"total_refcount", res.TotalRefCount,
		"delta", res.Delta,
		"duration_ms", res.DurationMS,
	)

	WriteJSONOK(w, BlockStoreAuditResponse{Result: res})
}
