package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// UsageRecomputeRuntime is the narrow Runtime surface needed by
// UsageRecomputeHandler. Declaring it here rather than depending on
// *runtime.Runtime keeps the handler unit-testable with a fake.
type UsageRecomputeRuntime interface {
	// RecomputeShareUsage rebuilds the metadata store's used-bytes counters
	// from its file rows and reports what the named share held before and
	// after.
	RecomputeShareUsage(ctx context.Context, shareName string) (*runtime.UsageRecomputeResult, error)
}

// UsageRecomputeHandler exposes the on-demand used-bytes repair endpoint.
type UsageRecomputeHandler struct {
	runtime UsageRecomputeRuntime
}

// NewUsageRecomputeHandler constructs a handler bound to the given Runtime
// surface. A nil runtime is refused per-request so the server still boots in
// degraded modes.
func NewUsageRecomputeHandler(rt UsageRecomputeRuntime) *UsageRecomputeHandler {
	return &UsageRecomputeHandler{runtime: rt}
}

// UsageRecomputeResponse wraps the repair result for JSON output. Returned by
// POST /api/v1/shares/{name}/usage/recompute.
type UsageRecomputeResponse struct {
	Result *runtime.UsageRecomputeResult `json:"result"`
}

// Recompute handles POST /api/v1/shares/{name}/usage/recompute.
//
// It rebuilds the metadata store's used-bytes counters from its file rows and
// reports the named share's figure before and after. A share carrying bytes it
// no longer holds reports itself fuller than it is, and that figure gates
// writes through the share quota — this is the repair for one.
//
// The rebuild scans every file row in the store, so it is slow in proportion to
// the store's size and covers every share the store serves, not only the named
// one.
//
// Status codes:
//   - 200 OK with UsageRecomputeResponse on success
//   - 400 Bad Request when {name} is empty
//   - 404 Not Found when {name} is not a registered share
//   - 500 Internal Server Error on unexpected runtime errors
func (h *UsageRecomputeHandler) Recompute(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
		BadRequest(w, "share name is required")
		return
	}

	res, err := h.runtime.RecomputeShareUsage(r.Context(), name)
	if err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "share not found: "+name)
			return
		}
		// The underlying error can carry filesystem paths and DSNs, so it is
		// logged rather than returned.
		logger.Debug("Share used-bytes recompute error", "share", name, "error", err)
		InternalServerError(w, "usage recompute failed")
		return
	}

	WriteJSONOK(w, UsageRecomputeResponse{Result: res})
}
