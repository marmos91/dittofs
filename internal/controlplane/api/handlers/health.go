package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// HealthCheckTimeout is the maximum time allowed for health check operations.
// This timeout applies to store health checks to prevent slow stores from
// blocking health probes indefinitely.
const HealthCheckTimeout = 5 * time.Second

// HealthHandler handles health check endpoints.
//
// Health endpoints are unauthenticated and provide:
//   - Liveness probe: Is the server process running?
//   - Readiness probe: Is the server ready to accept requests?
//   - Store health: Detailed health status of all stores
type HealthHandler struct {
	registry  *runtime.Runtime
	startTime time.Time
}

// NewHealthHandler creates a new health handler.
//
// The registry parameter may be nil, in which case readiness and store
// health checks will return unhealthy status.
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
func (h *HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(h.startTime)
	writeJSON(w, http.StatusOK, healthyResponse(map[string]interface{}{
		"service":    "dittofs",
		"started_at": h.startTime.UTC().Format(time.RFC3339),
		"uptime":     uptime.Round(time.Second).String(),
		"uptime_sec": int64(uptime.Seconds()),
	}))
}

// Readiness handles GET /health/ready - readiness probe.
// Returns 200 OK if registry is initialized.
func (h *HealthHandler) Readiness(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, unhealthyResponse("registry not initialized"))
		return
	}

	runningAdapters := h.registry.ListRunningAdapters()
	writeJSON(w, http.StatusOK, healthyResponse(map[string]interface{}{
		"shares":          h.registry.CountShares(),
		"metadata_stores": h.registry.CountMetadataStores(),
		"adapters": map[string]interface{}{
			"running": len(runningAdapters),
			"types":   runningAdapters,
		},
	}))
}

// StoreHealth represents the health status of a single store.
type StoreHealth struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Latency string `json:"latency,omitempty"`
}

// StoresResponse represents the detailed store health response.
type StoresResponse struct {
	MetadataStores []StoreHealth `json:"metadata_stores"`
	BlockStore     *StoreHealth  `json:"block_store,omitempty"`
}

// Stores handles GET /health/stores - detailed store health.
//
// Checks the health of all registered stores:
//   - Metadata stores: Calls Healthcheck() method
//
// Returns 200 OK if all stores are healthy, 503 Service Unavailable if any
// store is unhealthy.
func (h *HealthHandler) Stores(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, unhealthyResponse("registry not initialized"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	response := StoresResponse{
		MetadataStores: make([]StoreHealth, 0),
	}

	allHealthy := true

	// Check metadata stores
	for _, name := range h.registry.ListMetadataStores() {
		store, err := h.registry.GetMetadataStore(name)
		if err != nil {
			response.MetadataStores = append(response.MetadataStores, StoreHealth{
				Name:   name,
				Type:   "metadata",
				Status: "unhealthy",
				Error:  err.Error(),
			})
			allHealthy = false
			continue
		}

		start := time.Now()
		err = store.Healthcheck(ctx)
		latency := time.Since(start)

		health := StoreHealth{
			Name:    name,
			Type:    "metadata",
			Latency: latency.String(),
		}

		if err != nil {
			health.Status = "unhealthy"
			health.Error = err.Error()
			allHealthy = false
		} else {
			health.Status = "healthy"
		}

		response.MetadataStores = append(response.MetadataStores, health)
	}

	// Check block store (via payload service)
	payloadSvc := h.registry.GetBlockService()
	if payloadSvc != nil {
		start := time.Now()
		err := payloadSvc.HealthCheck(ctx)
		latency := time.Since(start)

		blockHealth := &StoreHealth{
			Name:    "block-store",
			Type:    "block",
			Latency: latency.String(),
		}

		if err != nil {
			blockHealth.Status = "unhealthy"
			blockHealth.Error = err.Error()
			allHealthy = false
		} else {
			blockHealth.Status = "healthy"
		}

		response.BlockStore = blockHealth
	}

	if allHealthy {
		writeJSON(w, http.StatusOK, healthyResponse(response))
	} else {
		writeJSON(w, http.StatusServiceUnavailable, unhealthyResponseWithData(response))
	}
}
