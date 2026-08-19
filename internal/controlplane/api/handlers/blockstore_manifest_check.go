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

// ManifestCheckRuntime is the narrow Runtime surface needed by
// BlockStoreManifestCheckHandler, defined here so the handler can be unit
// tested against a fake rather than a whole *runtime.Runtime.
type ManifestCheckRuntime interface {
	// CheckManifests runs the metadata-only manifest-coverage scan for the
	// named share.
	CheckManifests(ctx context.Context, shareName string) (*engine.ManifestCheckResult, error)
}

// BlockStoreManifestCheckHandler exposes the on-demand manifest-coverage scan.
type BlockStoreManifestCheckHandler struct {
	runtime ManifestCheckRuntime
}

// NewBlockStoreManifestCheckHandler constructs a handler bound to the given
// Runtime surface. A nil runtime makes the handler refuse requests rather than
// panic, so the server still boots in degraded modes.
func NewBlockStoreManifestCheckHandler(rt ManifestCheckRuntime) *BlockStoreManifestCheckHandler {
	return &BlockStoreManifestCheckHandler{runtime: rt}
}

// BlockStoreManifestCheckResponse wraps engine.ManifestCheckResult for JSON
// output. Returned by POST /api/v1/shares/{name}/audit/manifest.
type BlockStoreManifestCheckResponse struct {
	Result *engine.ManifestCheckResult `json:"result"`
}

// RunManifestCheck handles POST /api/v1/shares/{name}/audit/manifest.
//
// The scan walks the share's metadata store and, per payload, compares the
// ranges its manifest rows cover against the file's recorded size. It reads no
// block data and touches no remote store.
//
// Status codes:
//   - 200 OK with BlockStoreManifestCheckResponse on success
//   - 400 Bad Request when {name} is empty
//   - 404 Not Found when {name} is not a registered share
//   - 500 Internal Server Error on unexpected runtime errors
func (h *BlockStoreManifestCheckHandler) RunManifestCheck(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
		BadRequest(w, "share name is required")
		return
	}

	res, err := h.runtime.CheckManifests(r.Context(), name)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+name)
			return
		}
		logger.Debug("Store check error", logger.KeyShare, name, "error", err)
		// The underlying error can carry filesystem paths; it is logged at
		// Debug above rather than returned in the body.
		InternalServerError(w, "store check failed")
		return
	}

	logger.Info("Store check complete",
		logger.KeyShare, name,
		"files_scanned", res.FilesScanned,
		"damaged_payloads", res.DamagedPayloads,
		"claimed_uncovered_bytes", res.ClaimedUncoveredBytes,
		"unplaceable_rows", res.UnplaceableRows,
		"unknown_hash_rows", res.UnknownHashRows,
		"duration_ms", res.DurationMS,
	)

	WriteJSONOK(w, BlockStoreManifestCheckResponse{Result: res})
}
