package handlers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// HealthCheckTimeout is the maximum time allowed for health check operations.
// This timeout prevents slow entity probes from blocking API handlers
// indefinitely.
const HealthCheckTimeout = 5 * time.Second

// dbCacheTTL is how long a cached DB health result is considered fresh.
// Prevents every /health request from issuing a synchronous DB ping,
// which matters when K8s liveness probes fire every 10s.
const dbCacheTTL = 5 * time.Second

// HealthHandler handles health check endpoints.
//
// Health endpoints are unauthenticated and provide:
//   - Liveness probe: Is the server process running?
//   - Readiness probe: Is the server ready to accept requests?

type HealthHandler struct {
	registry  *runtime.Runtime
	startTime time.Time

	// Cached DB health check result to avoid probing on every request.
	dbStatusMu        sync.Mutex
	dbStatusCache     string // "reachable" | "unreachable"
	dbStatusCacheTime time.Time
}

// NewHealthHandler creates a new health handler.
//
// The registry parameter may be nil, in which case readiness checks
// will return unhealthy status.
func NewHealthHandler(registry *runtime.Runtime) *HealthHandler {
	return &HealthHandler{
		registry:  registry,
		startTime: time.Now(),
	}
}

// Liveness handles GET /health - simple liveness probe.
//
// Returns 200 OK if the server process is running. This endpoint is designed
// for Kubernetes liveness probes and should always succeed as long as the
// HTTP server is responsive.
//
// The response includes a control_plane_db field indicating whether the
// control plane database is reachable. A degraded status (DB unreachable)
// still returns 200 -- K8s should NOT restart on a transient DB blip.
func (h *HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(h.startTime)

	dbStatus := "unknown"
	overallStatus := "healthy"

	if h.registry != nil {
		cpStore := h.registry.Store()
		if cpStore != nil {
			dbStatus = h.cachedDBStatus(r.Context(), cpStore)
			if dbStatus == "unreachable" {
				overallStatus = "degraded"
			}
		}
	}

	data := map[string]any{
		"service":          "dittofs",
		"started_at":       h.startTime.UTC().Format(time.RFC3339),
		"uptime":           uptime.Round(time.Second).String(),
		"uptime_sec":       int64(uptime.Seconds()),
		"control_plane_db": dbStatus,
	}

	if overallStatus == "degraded" {
		WriteJSON(w, http.StatusOK, degradedResponse(data))
	} else {
		WriteJSON(w, http.StatusOK, healthyResponse(data))
	}
}

// Readiness handles GET /health/ready - readiness probe.
// Returns 200 OK if registry is initialized.
// Includes grace period information when a grace period is active.
func (h *HealthHandler) Readiness(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		WriteJSON(w, http.StatusServiceUnavailable, unhealthyResponse("registry not initialized"))
		return
	}

	runningAdapters := h.registry.ListRunningAdapters()
	data := map[string]any{
		"shares":          h.registry.CountShares(),
		"metadata_stores": h.registry.CountMetadataStores(),
		"adapters": map[string]any{
			"running": len(runningAdapters),
			"types":   runningAdapters,
		},
	}

	// Include grace period info if NFS adapter is configured
	if graceHandler := NewGraceHandlerFromProvider(h.registry.NFSClientProvider()); graceHandler != nil {
		info := graceHandler.sm.GraceStatus()
		data["grace_period"] = map[string]any{
			"active":            info.Active,
			"remaining_seconds": info.RemainingSeconds,
			"expected_clients":  info.ExpectedClients,
			"reclaimed_clients": info.ReclaimedClients,
		}
	}

	WriteJSON(w, http.StatusOK, healthyResponse(data))
}

// cachedDBStatus returns "reachable" or "unreachable", probing the DB only
// when the cached result is older than dbCacheTTL.
func (h *HealthHandler) cachedDBStatus(ctx context.Context, s interface{ Healthcheck(context.Context) error }) string {
	h.dbStatusMu.Lock()
	defer h.dbStatusMu.Unlock()

	if h.dbStatusCache != "" && time.Since(h.dbStatusCacheTime) < dbCacheTTL {
		return h.dbStatusCache
	}

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := s.Healthcheck(probeCtx)
	cancel()

	if err != nil {
		h.dbStatusCache = "unreachable"
	} else {
		h.dbStatusCache = "reachable"
	}
	h.dbStatusCacheTime = time.Now()
	return h.dbStatusCache
}
