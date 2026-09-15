package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

// CreateBlockStoreRequest is the request body for POST /api/v1/store/block.
type CreateBlockStoreRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config string `json:"config,omitempty"` // JSON string for type-specific config
}

// UpdateBlockStoreRequest is the request body for PUT /api/v1/store/block/{name}.
type UpdateBlockStoreRequest struct {
	// Name renames the store. Names identify a block store on their own, so a
	// rename that would collide is refused rather than applied.
	Name   *string `json:"name,omitempty"`
	Type   *string `json:"type,omitempty"`
	Config *string `json:"config,omitempty"`
}

// BlockStoreResponse is the response body for block store endpoints.
// Status is non-omitempty so clients can render "unknown" explicitly
// when the runtime has no definitive report.
type BlockStoreResponse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Status    health.Report   `json:"status"`
}

// validateBlockStoreType checks that a store type is one a block store can have.
func validateBlockStoreType(storeType string) bool {
	return storeType == "s3" || storeType == "memory"
}

// Create handles POST /api/v1/store/block.
// Creates a new block store configuration (admin only).
func (h *BlockStoreHandler) Create(w http.ResponseWriter, r *http.Request) {
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

	if !validateBlockStoreType(req.Type) {
		BadRequest(w, "Store type '"+req.Type+"' is not a valid block store type")
		return
	}

	bs := &models.BlockStoreConfig{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Type:      req.Type,
		Config:    req.Config,
		CreatedAt: time.Now(),
	}

	// Validate before persisting so a saved config is never one that would
	// fail on attach.
	if err := runtime.ValidateBlockStoreConfig(req.Type, bs); err != nil {
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

// List handles GET /api/v1/store/block.
// Lists all block store configurations (admin only).
func (h *BlockStoreHandler) List(w http.ResponseWriter, r *http.Request) {
	stores, err := h.store.ListBlockStores(r.Context())
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

// Get handles GET /api/v1/store/block/{name}.
// Gets a block store configuration by name (admin only).
func (h *BlockStoreHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	bs, err := h.store.GetBlockStore(r.Context(), name)
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

// Update handles PUT /api/v1/store/block/{name}.
// Updates a block store configuration (admin only).
func (h *BlockStoreHandler) Update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req UpdateBlockStoreRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	bs, err := h.store.GetBlockStore(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Block store not found")
			return
		}
		InternalServerError(w, "Failed to get block store")
		return
	}

	renameTo := ""
	if req.Name != nil && *req.Name != bs.Name {
		if strings.TrimSpace(*req.Name) == "" {
			BadRequest(w, "Block store name cannot be empty")
			return
		}
		renameTo = *req.Name

		// The rename runs after the type/config write below, so a name that is
		// already taken would answer 409 with the new config already committed —
		// a reply that reads as "nothing happened" against a store that changed.
		// Refuse here instead, before anything is written. A store taking the
		// name between this check and the rename still lands on the rename's own
		// refusal.
		existing, err := h.store.GetBlockStore(r.Context(), renameTo)
		if err != nil && !errors.Is(err, models.ErrStoreNotFound) {
			InternalServerError(w, "Failed to get block store")
			return
		}
		// The lookup also answers to an ID, but only a name collides: a store
		// whose ID reads like the new name does not block the rename.
		if err == nil && existing.Name == renameTo {
			Conflict(w, "Block store "+renameTo+" already exists")
			return
		}
	}

	if req.Type != nil {
		if !validateBlockStoreType(*req.Type) {
			BadRequest(w, "Store type '"+*req.Type+"' is not a valid block store type")
			return
		}
		bs.Type = *req.Type
	}
	if req.Config != nil {
		// Read paths redact secrets to "********"; reconcile any sentinel
		// the client echoed back so we never overwrite a real credential
		// with the redaction marker.
		previous := bs.Config
		bs.Config = mergeRedactedSecrets(bs.Config, *req.Config)
		bs.ParsedConfig = nil
		// Blocks already written to an encrypted store carry an encryption
		// frame. Dropping the policy does not fail anywhere downstream — the
		// undecorated store would hand that framed ciphertext back as if it
		// were plaintext — so the removal is refused here.
		// The stored blob's parse result is discarded because an
		// unparseable one cannot have carried a policy that was ever in
		// effect: building the encrypted store parses the same blob and
		// fails first, so no block was written under it. Neither write path
		// can store such a blob either — validation below, and the same
		// check on create, both parse before persisting.
		hadEncryption, _ := hasEncryptionPolicy(previous)
		hasEncryption, parsed := hasEncryptionPolicy(bs.Config)
		// An unparseable incoming blob is left to the config validation
		// below, which reports the parse error rather than a removal the
		// request may not have asked for.
		if parsed && hadEncryption && !hasEncryption {
			BadRequest(w, "Encryption cannot be removed from a block store once enabled: "+
				"blocks already written are encrypted and would be served as ciphertext. "+
				"Change the encryption policy instead, or move the share to a new block store.")
			return
		}
	}

	// Re-validate on any type/config change so a no-op PUT does not
	// re-touch the filesystem, mirroring Create's pre-persist check.
	if req.Type != nil || req.Config != nil {
		if err := runtime.ValidateBlockStoreConfig(bs.Type, bs); err != nil {
			BadRequest(w, "Invalid block store config: "+err.Error())
			return
		}
	}

	if err := h.store.UpdateBlockStore(r.Context(), bs); err != nil {
		InternalServerError(w, "Failed to update block store")
		return
	}

	// A share's binding normally holds the store's UUID, but the older update
	// path persisted the name instead. Those shares would resolve nothing once
	// the name moves, so repoint them — onto the UUID, which cannot go stale
	// the next time the store is renamed.
	prevName := bs.Name
	if renameTo != "" {
		renamed, err := h.store.RenameBlockStore(r.Context(), name, renameTo)
		switch {
		case errors.Is(err, models.ErrDuplicateStore):
			Conflict(w, "Block store "+renameTo+" already exists")
			return
		case errors.Is(err, models.ErrStoreNotFound):
			NotFound(w, "Block store not found")
			return
		case err != nil:
			InternalServerError(w, "Failed to rename block store")
			return
		}
		bs = renamed
	}

	// Evict the cached checker so the post-update response does not
	// observe a stale probe from before the config change landed. A rename
	// leaves an entry under the old name too.
	if h.runtime != nil {
		h.runtime.InvalidateBlockStoreChecker(name)
		// Checkers are keyed by the store's name, and the route may address it
		// by ID, so the name it was cached under is evicted explicitly rather
		// than assumed to be the one in the URL.
		h.runtime.InvalidateBlockStoreChecker(prevName)
		if renameTo != "" {
			h.runtime.InvalidateBlockStoreChecker(renameTo)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	resp := blockStoreToResponse(bs)
	resp.Status = h.statusForConfig(ctx, bs)
	WriteJSONOK(w, resp)
}

// Remove handles DELETE /api/v1/store/block/{name}.
// Deletes a block store configuration (admin only).
func (h *BlockStoreHandler) Remove(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	if err := h.store.DeleteBlockStore(r.Context(), name); err != nil {
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
		h.runtime.InvalidateBlockStoreChecker(name)
	}

	WriteNoContent(w)
}

// blockStoreToResponse converts a models.BlockStoreConfig to BlockStoreResponse.
func blockStoreToResponse(s *models.BlockStoreConfig) BlockStoreResponse {
	return BlockStoreResponse{
		ID:        s.ID,
		Name:      s.Name,
		Type:      s.Type,
		Config:    redactedConfigRaw(s.Config),
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

// HealthCheck handles GET /api/v1/store/block/{name}/health.
// Always returns 200 with health status in the response body. This
// is the legacy probe route; the newer /status route returns a full
// [health.Report]. Both share [blockstoreprobe.Probe] so answers
// cannot drift.
func (h *BlockStoreHandler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	bs, err := h.store.GetBlockStore(r.Context(), name)
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

// Status handles GET /api/v1/store/block/{name}/status.
// Returns 404 when the config does not exist (matching Get
// semantics) and 200 with a [health.Report] body otherwise.
func (h *BlockStoreHandler) Status(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	// Existence check preserves 404 semantics. The fetched config is
	// then handed to statusForConfig so the runtime checker layer
	// does not issue a second identical round-trip.
	bs, err := h.store.GetBlockStore(r.Context(), name)
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
