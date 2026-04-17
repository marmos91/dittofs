package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/blockstoreprobe"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
)

// BlockStoreHandler handles block store configuration API endpoints.
// It serves both local and remote block stores, with kind extracted from the URL path.
type BlockStoreHandler struct {
	store   store.BlockStoreConfigStore
	runtime *runtime.Runtime
}

// NewBlockStoreHandler creates a new BlockStoreHandler. rt may be
// nil in unit tests that do not exercise runtime probes; status
// reads degrade to [health.StatusUnknown] in that case via the
// runtime accessor methods' nil-receiver handling.
func NewBlockStoreHandler(s store.BlockStoreConfigStore, rt *runtime.Runtime) *BlockStoreHandler {
	return &BlockStoreHandler{store: s, runtime: rt}
}

// CreateBlockStoreRequest is the request body for POST /api/v1/store/block/{kind}.
type CreateBlockStoreRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config string `json:"config,omitempty"` // JSON string for type-specific config
}

// UpdateBlockStoreRequest is the request body for PUT /api/v1/store/block/{kind}/{name}.
type UpdateBlockStoreRequest struct {
	Type   *string `json:"type,omitempty"`
	Config *string `json:"config,omitempty"`
}

// BlockStoreResponse is the response body for block store endpoints.
// Status is non-omitempty so clients can render "unknown" explicitly
// when the runtime has no definitive report.
type BlockStoreResponse struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Kind      models.BlockStoreKind `json:"kind"`
	Type      string                `json:"type"`
	Config    string                `json:"config,omitempty"`
	CreatedAt time.Time             `json:"created_at"`
	Status    health.Report         `json:"status"`
}

// extractKind extracts the block store kind from the URL path parameter.
func extractKind(r *http.Request) (models.BlockStoreKind, bool) {
	kindStr := chi.URLParam(r, "kind")
	switch kindStr {
	case "local":
		return models.BlockStoreKindLocal, true
	case "remote":
		return models.BlockStoreKindRemote, true
	default:
		return "", false
	}
}

// validateBlockStoreType checks that a store type is valid for the given kind.
// Local block stores accept: fs, memory.
// Remote block stores accept: s3, memory.
func validateBlockStoreType(kind models.BlockStoreKind, storeType string) bool {
	switch kind {
	case models.BlockStoreKindLocal:
		return storeType == "fs" || storeType == "memory"
	case models.BlockStoreKindRemote:
		return storeType == "s3" || storeType == "memory"
	default:
		return false
	}
}

// Create handles POST /api/v1/store/block/{kind}.
// Creates a new block store configuration (admin only).
func (h *BlockStoreHandler) Create(w http.ResponseWriter, r *http.Request) {
	kind, ok := extractKind(r)
	if !ok {
		BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
		return
	}

	var req CreateBlockStoreRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Name == "" {
		BadRequest(w, "Store name is required")
		return
	}
	if req.Type == "" {
		BadRequest(w, "Store type is required")
		return
	}

	if !validateBlockStoreType(kind, req.Type) {
		BadRequest(w, "Store type '"+req.Type+"' is not valid for kind '"+string(kind)+"'")
		return
	}

	bs := &models.BlockStoreConfig{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Kind:      kind,
		Type:      req.Type,
		Config:    req.Config,
		CreatedAt: time.Now(),
	}

	// Validate (and materialise the fs base path) before persisting so a
	// saved config is never one that would fail on attach.
	if err := runtime.ValidateBlockStoreConfig(kind, req.Type, bs); err != nil {
		BadRequest(w, "Invalid block store config: "+err.Error())
		return
	}

	if _, err := h.store.CreateBlockStore(r.Context(), bs); err != nil {
		if errors.Is(err, models.ErrDuplicateStore) {
			Conflict(w, "Block store already exists")
			return
		}
		InternalServerError(w, "Failed to create block store")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	resp := blockStoreToResponse(bs)
	resp.Status = h.statusForConfig(ctx, bs)
	WriteJSONCreated(w, resp)
}

// List handles GET /api/v1/store/block/{kind}.
// Lists all block store configurations of the given kind (admin only).
func (h *BlockStoreHandler) List(w http.ResponseWriter, r *http.Request) {
	kind, ok := extractKind(r)
	if !ok {
		BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
		return
	}

	stores, err := h.store.ListBlockStores(r.Context(), kind)
	if err != nil {
		InternalServerError(w, "Failed to list block stores")
		return
	}

	// Share a single HealthCheckTimeout budget across the populate
	// loop so N stores do not compound to N*5s on a cold cache.
	listCtx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	response := make([]BlockStoreResponse, len(stores))
	for i, s := range stores {
		response[i] = blockStoreToResponse(s)
		// List already holds each config in memory; avoid a second
		// per-entity round-trip by probing with the loaded pointer.
		response[i].Status = h.statusForConfig(listCtx, s)
	}

	WriteJSONOK(w, response)
}

// Get handles GET /api/v1/store/block/{kind}/{name}.
// Gets a block store configuration by name (admin only).
func (h *BlockStoreHandler) Get(w http.ResponseWriter, r *http.Request) {
	kind, ok := extractKind(r)
	if !ok {
		BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	bs, err := h.store.GetBlockStore(r.Context(), name, kind)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Block store not found")
			return
		}
		InternalServerError(w, "Failed to get block store")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	resp := blockStoreToResponse(bs)
	resp.Status = h.statusForConfig(ctx, bs)
	WriteJSONOK(w, resp)
}

// Update handles PUT /api/v1/store/block/{kind}/{name}.
// Updates a block store configuration (admin only).
func (h *BlockStoreHandler) Update(w http.ResponseWriter, r *http.Request) {
	kind, ok := extractKind(r)
	if !ok {
		BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req UpdateBlockStoreRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	bs, err := h.store.GetBlockStore(r.Context(), name, kind)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Block store not found")
			return
		}
		InternalServerError(w, "Failed to get block store")
		return
	}

	if req.Type != nil {
		if !validateBlockStoreType(bs.Kind, *req.Type) {
			BadRequest(w, "Store type '"+*req.Type+"' is not valid for kind '"+string(bs.Kind)+"'")
			return
		}
		bs.Type = *req.Type
	}
	if req.Config != nil {
		bs.Config = *req.Config
		bs.ParsedConfig = nil
	}

	// Re-validate on any type/config change so a no-op PUT does not
	// re-touch the filesystem, mirroring Create's pre-persist check.
	if req.Type != nil || req.Config != nil {
		if err := runtime.ValidateBlockStoreConfig(bs.Kind, bs.Type, bs); err != nil {
			BadRequest(w, "Invalid block store config: "+err.Error())
			return
		}
	}

	if err := h.store.UpdateBlockStore(r.Context(), bs); err != nil {
		InternalServerError(w, "Failed to update block store")
		return
	}

	// Evict the cached checker so the post-update response does not
	// observe a stale probe from before the config change landed.
	if h.runtime != nil {
		h.runtime.InvalidateBlockStoreChecker(kind, name)
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	resp := blockStoreToResponse(bs)
	resp.Status = h.statusForConfig(ctx, bs)
	WriteJSONOK(w, resp)
}

// Delete handles DELETE /api/v1/store/block/{kind}/{name}.
// Deletes a block store configuration (admin only).
func (h *BlockStoreHandler) Delete(w http.ResponseWriter, r *http.Request) {
	kind, ok := extractKind(r)
	if !ok {
		BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	if err := h.store.DeleteBlockStore(r.Context(), name, kind); err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Block store not found")
			return
		}
		if errors.Is(err, models.ErrStoreInUse) {
			Conflict(w, "Cannot delete block store: it is in use by one or more shares")
			return
		}
		InternalServerError(w, "Failed to delete block store")
		return
	}

	// Evict any cached health checker so a subsequently-recreated
	// store with the same name does not inherit a stale probe.
	if h.runtime != nil {
		h.runtime.InvalidateBlockStoreChecker(kind, name)
	}

	WriteNoContent(w)
}

// blockStoreToResponse converts a models.BlockStoreConfig to BlockStoreResponse.
func blockStoreToResponse(s *models.BlockStoreConfig) BlockStoreResponse {
	return BlockStoreResponse{
		ID:        s.ID,
		Name:      s.Name,
		Kind:      s.Kind,
		Type:      s.Type,
		Config:    s.Config,
		CreatedAt: s.CreatedAt,
	}
}

// BlockStoreHealthResponse is the response body for the health check endpoint.
type BlockStoreHealthResponse struct {
	Healthy   bool   `json:"healthy"`
	LatencyMs int64  `json:"latency_ms"`
	CheckedAt string `json:"checked_at"`
	Details   string `json:"details,omitempty"`
}

// HealthCheck handles GET /api/v1/store/block/{kind}/{name}/health.
// Always returns 200 with health status in the response body. This
// is the legacy probe route; the newer /status route returns a full
// [health.Report]. Both share [blockstoreprobe.Probe] so answers
// cannot drift.
func (h *BlockStoreHandler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	kind, ok := extractKind(r)
	if !ok {
		BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	bs, err := h.store.GetBlockStore(r.Context(), name, kind)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Block store not found")
			return
		}
		InternalServerError(w, "Failed to get block store")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()

	rep := blockstoreprobe.Probe(ctx, bs)

	WriteJSONOK(w, BlockStoreHealthResponse{
		Healthy:   rep.Status == health.StatusHealthy,
		LatencyMs: rep.LatencyMs,
		CheckedAt: rep.CheckedAt.Format(time.RFC3339),
		Details:   rep.Message,
	})
}

// Status handles GET /api/v1/store/block/{kind}/{name}/status.
// Returns 404 when the config does not exist (matching Get
// semantics) and 200 with a [health.Report] body otherwise.
func (h *BlockStoreHandler) Status(w http.ResponseWriter, r *http.Request) {
	kind, ok := extractKind(r)
	if !ok {
		BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	// Existence check preserves 404 semantics. The fetched config is
	// then handed to statusForConfig so the runtime checker layer
	// does not issue a second identical round-trip.
	bs, err := h.store.GetBlockStore(r.Context(), name, kind)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Block store not found")
			return
		}
		InternalServerError(w, "Failed to get block store")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	WriteJSONOK(w, h.statusForConfig(ctx, bs))
}

// statusFor returns a [health.Report] for the named block store via
// the runtime's cached checker layer. A nil runtime is handled by
// the runtime accessor's nil-receiver path, which returns an
// "unknown" checker instead of panicking. The caller is responsible
// for bounding ctx with [HealthCheckTimeout]: single-entity /status
// handlers wrap once at the handler level, and list handlers wrap
// once before the populate loop so all entities share a single 5s
// budget instead of compounding to N*5s worst case.
//
// Prefer [statusForConfig] when the caller already holds a fetched
// statusForConfig probes the block store's cached checker using an already-fetched config: Get,
// Create, Update, Status (after its 404 check), and List (after its
// populate fetch) all hold the concrete config and can avoid a second
// round-trip by probing it directly via [runtime.Runtime.BlockStoreCheckerFor].
func (h *BlockStoreHandler) statusForConfig(ctx context.Context, bs *models.BlockStoreConfig) health.Report {
	return h.runtime.BlockStoreCheckerFor(bs).Healthcheck(ctx)
}
