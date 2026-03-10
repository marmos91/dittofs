package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// DrainUploadsTimeout is the maximum time allowed for draining all uploads.
const DrainUploadsTimeout = 5 * time.Minute

// SystemHandler handles system-level endpoints.
type SystemHandler struct {
	runtime *runtime.Runtime
}

// NewSystemHandler creates a new system handler.
func NewSystemHandler(rt *runtime.Runtime) *SystemHandler {
	return &SystemHandler{runtime: rt}
}

// DrainUploads handles POST /api/v1/system/drain-uploads.
//
// Waits for all in-flight block store uploads to complete across all files.
// Useful for benchmarking to ensure clean boundaries between test workloads.
//
// Returns 200 OK when all uploads have drained, or 504 Gateway Timeout
// if the drain does not complete within 5 minutes.
func (h *SystemHandler) DrainUploads(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), DrainUploadsTimeout)
	defer cancel()

	logger.Info("Drain uploads requested")
	start := time.Now()

	if err := h.runtime.DrainAllUploads(ctx); err != nil {
		logger.Error("Drain uploads failed", "error", err, "duration", time.Since(start))
		if ctx.Err() != nil {
			WriteProblem(w, http.StatusGatewayTimeout, "Gateway Timeout",
				"drain uploads did not complete within timeout: "+err.Error())
		} else {
			WriteProblem(w, http.StatusInternalServerError, "Internal Server Error",
				"drain uploads failed: "+err.Error())
		}
		return
	}

	logger.Info("Drain uploads complete", "duration", time.Since(start))
	WriteJSONOK(w, map[string]any{
		"status":   "drained",
		"duration": time.Since(start).String(),
	})
}
