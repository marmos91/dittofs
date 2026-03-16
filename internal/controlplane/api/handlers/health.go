package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// HealthCheckTimeout is the maximum time allowed for health check operations.
// This timeout applies to store health checks to prevent slow stores from
// blocking health probes indefinitely.
const HealthCheckTimeout = 5 * time.Second

// ShareHealthInfo represents the health state of a single share's remote store.
type ShareHealthInfo struct {
	Name                string  `json:"name"`
	RemoteHealthy       bool    `json:"remote_healthy"`
	OutageDurationSec   float64 `json:"outage_duration_seconds,omitempty"`
	PendingUploads      int     `json:"pending_uploads,omitempty"`
	OfflineReadsBlocked int64   `json:"offline_reads_blocked,omitempty"`
}

// StorageHealthInfo represents aggregate storage health across all shares.
type StorageHealthInfo struct {
	Status       string            `json:"status"` // "healthy", "degraded"
	Shares       []ShareHealthInfo `json:"shares,omitempty"`
	TotalPending int               `json:"total_pending"`
}

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
//
// When an NFS adapter is configured, the response includes server identity
// information (server_owner, server_impl, server_scope) for trunking verification.
func (h *HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(h.startTime)
	data := map[string]any{
		"service":    "dittofs",
		"started_at": h.startTime.UTC().Format(time.RFC3339),
		"uptime":     uptime.Round(time.Second).String(),
		"uptime_sec": int64(uptime.Seconds()),
	}

	// Include NFS server identity and per-share storage health when available.
	anyDegraded := false
	if h.registry != nil {
		if serverInfo := ServerIdentityFromProvider(h.registry.NFSClientProvider()); serverInfo != nil {
			for k, v := range serverInfo {
				data[k] = v
			}
		}

		storageHealth := h.getStorageHealth()
		data["storage_health"] = storageHealth
		anyDegraded = storageHealth.Status == "degraded"
	}

	if anyDegraded {
		// Return 200 (not 503) with "degraded" status.
		// Edge nodes are expected to operate offline; K8s should NOT restart.
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
	BlockStores    []StoreHealth `json:"block_stores"`
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
		WriteJSON(w, http.StatusServiceUnavailable, unhealthyResponse("registry not initialized"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	response := StoresResponse{
		MetadataStores: make([]StoreHealth, 0),
		BlockStores:    make([]StoreHealth, 0),
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

		health := checkStoreHealth(ctx, name, "metadata", store.Healthcheck)
		if health.Status == "unhealthy" {
			allHealthy = false
		}
		response.MetadataStores = append(response.MetadataStores, health)
	}

	// Check per-share block store health
	for _, shareName := range h.registry.ListShares() {
		share, err := h.registry.GetShare(shareName)
		if err != nil || share.BlockStore == nil {
			continue
		}

		health := checkStoreHealth(ctx, fmt.Sprintf("block-store/%s", shareName), "block", share.BlockStore.HealthCheck)
		if health.Status == "unhealthy" {
			allHealthy = false
		}
		response.BlockStores = append(response.BlockStores, health)
	}

	if allHealthy {
		WriteJSON(w, http.StatusOK, healthyResponse(response))
	} else {
		WriteJSON(w, http.StatusServiceUnavailable, unhealthyResponseWithData(response))
	}
}

// checkStoreHealth runs a health check function and returns the result as StoreHealth.
func checkStoreHealth(ctx context.Context, name, storeType string, check func(context.Context) error) StoreHealth {
	start := time.Now()
	err := check(ctx)
	latency := time.Since(start)

	health := StoreHealth{
		Name:    name,
		Type:    storeType,
		Status:  "healthy",
		Latency: latency.String(),
	}
	if err != nil {
		health.Status = "unhealthy"
		health.Error = err.Error()
	}
	return health
}

// getStorageHealth collects per-share remote store health information.
func (h *HealthHandler) getStorageHealth() StorageHealthInfo {
	info := StorageHealthInfo{
		Status: "healthy",
		Shares: make([]ShareHealthInfo, 0),
	}

	for _, shareName := range h.registry.ListShares() {
		share, err := h.registry.GetShare(shareName)
		if err != nil || share.BlockStore == nil {
			continue
		}

		if !share.BlockStore.HasRemoteStore() {
			continue // Skip local-only shares
		}

		stats := share.BlockStore.GetCacheStats()
		shareInfo := ShareHealthInfo{
			Name:                shareName,
			RemoteHealthy:       stats.RemoteHealthy,
			OutageDurationSec:   stats.OutageDurationSecs,
			PendingUploads:      stats.PendingUploads,
			OfflineReadsBlocked: stats.OfflineReadsBlocked,
		}
		info.Shares = append(info.Shares, shareInfo)
		info.TotalPending += stats.PendingUploads

		if !stats.RemoteHealthy {
			info.Status = "degraded"
		}
	}

	return info
}
