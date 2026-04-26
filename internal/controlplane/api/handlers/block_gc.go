package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// BlockGCRuntime is the narrow Runtime surface needed by BlockStoreGCHandler.
// Defining the interface here (rather than depending on *runtime.Runtime
// directly) keeps the handler unit-testable: tests substitute a fake that
// records calls and returns canned responses, mirroring the
// testBlockStoreHandler pattern in blockstore_test.go.
type BlockGCRuntime interface {
	// RunBlockGCForShare dispatches a GC run scoped to the named share's
	// gc-state directory for last-run.json persistence (D-10).
	RunBlockGCForShare(ctx context.Context, shareName string, dryRun bool) (*engine.GCStats, error)

	// GCStateDirForShare returns the per-share gc-state directory the GC
	// engine writes `last-run.json` into. Empty when the share's local
	// store has no persistent root (in-memory backend).
	GCStateDirForShare(shareName string) (string, error)
}

// BlockStoreGCHandler exposes on-demand GC + last-run-summary endpoints
// (Phase 11 D-08/D-10).
type BlockStoreGCHandler struct {
	runtime BlockGCRuntime
}

// NewBlockStoreGCHandler constructs a handler bound to the given Runtime
// surface. Pass a nil-safe value: the handler refuses requests when
// runtime is nil so the server can still boot in degraded modes.
func NewBlockStoreGCHandler(rt BlockGCRuntime) *BlockStoreGCHandler {
	return &BlockStoreGCHandler{runtime: rt}
}

// BlockStoreGCRequest is the JSON body for POST /api/v1/shares/{name}/blockstore/gc.
// The dry_run flag flows through to engine.Options.DryRun: mark + sweep
// enumeration runs, but no DELETEs are issued, and the candidate set is
// captured in GCStats.DryRunCandidates (capped at engine.Options.DryRunSampleSize,
// default 1000 — D-09).
type BlockStoreGCRequest struct {
	DryRun bool `json:"dry_run,omitempty"`
}

// BlockStoreGCResponse wraps the *engine.GCStats result for JSON output.
// Returned by POST /api/v1/shares/{name}/blockstore/gc.
type BlockStoreGCResponse struct {
	Stats *engine.GCStats `json:"stats"`
}

// RunGC handles POST /api/v1/shares/{name}/blockstore/gc.
//
// Body: BlockStoreGCRequest (optional; missing body equals {dry_run:false}).
// Behavior: invokes Runtime.RunBlockGCForShare with the URL share name
// and the body's dry_run flag. The cross-share aggregation (D-03) means
// the GC scans every share whose remote-store config matches; the {name}
// path parameter scopes the last-run.json persistence target, not the
// mark/sweep set itself.
//
// Status codes:
//   - 200 OK with BlockStoreGCResponse on success
//   - 400 Bad Request when {name} is empty or the body decode fails
//   - 404 Not Found when {name} is not a registered share
//   - 500 Internal Server Error on unexpected runtime errors
func (h *BlockStoreGCHandler) RunGC(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}

	var req BlockStoreGCRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	stats, err := h.runtime.RunBlockGCForShare(r.Context(), name, req.DryRun)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+name)
			return
		}
		logger.Debug("Block store GC error", "share", name, "error", err)
		// Strip the underlying err string from the response body — it
		// can leak filesystem paths or other internal details. Details
		// are logged at Debug above for operator postmortems.
		InternalServerError(w, "GC failed")
		return
	}

	WriteJSONOK(w, BlockStoreGCResponse{Stats: stats})
}

// GCStatus handles GET /api/v1/shares/{name}/blockstore/gc-status.
//
// Reads `<gcStateRoot>/last-run.json` (Phase 11 D-10) for the share and
// returns the parsed engine.GCRunSummary. Returns 404 when no run has
// completed yet (file does not exist), letting operators distinguish
// "GC has never run for this share" from "GC ran and reported errors".
//
// Status codes:
//   - 200 OK with engine.GCRunSummary
//   - 400 Bad Request when {name} is empty
//   - 404 Not Found when the share is unknown OR when no last-run.json exists
//   - 500 Internal Server Error on filesystem or parse failures
func (h *BlockStoreGCHandler) GCStatus(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "share name is required")
		return
	}

	gcRoot, err := h.runtime.GCStateDirForShare(name)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+name)
			return
		}
		logger.Debug("Block store GC status lookup error", "share", name, "error", err)
		// IN-4-02: strip underlying err string from the response — same
		// rationale as IN-2-01 in RunGC. Details are logged at Debug.
		InternalServerError(w, "GC status lookup failed")
		return
	}
	if gcRoot == "" {
		// In-memory local store: no persistent run summary is written.
		NotFound(w, "no GC run recorded for share "+name+" (local store has no persistent root)")
		return
	}

	summaryPath := filepath.Join(gcRoot, "last-run.json")
	data, err := os.ReadFile(summaryPath) //nolint:gosec // path is server-controlled (joined to gc-state root)
	if err != nil {
		if os.IsNotExist(err) {
			NotFound(w, "no GC run recorded for share "+name)
			return
		}
		logger.Debug("Block store GC status read error", "share", name, "path", summaryPath, "error", err)
		// IN-4-02: don't leak filesystem path / underlying error.
		InternalServerError(w, "GC status read failed")
		return
	}

	var summary engine.GCRunSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		logger.Debug("Block store GC status parse error", "share", name, "path", summaryPath, "error", err)
		// IN-4-02: don't leak parse-error internals (file path embedded
		// via fmt.Errorf wrap previously surfaced to caller).
		InternalServerError(w, "GC status parse failed")
		return
	}

	WriteJSONOK(w, summary)
}
