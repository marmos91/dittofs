package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// BlockStoreHandler handles block store configuration API endpoints.
// It serves both local and remote block stores, with kind extracted from the URL path.
type BlockStoreHandler struct {
	store store.BlockStoreConfigStore
}

// NewBlockStoreHandler creates a new BlockStoreHandler.
func NewBlockStoreHandler(s store.BlockStoreConfigStore) *BlockStoreHandler {
	return &BlockStoreHandler{store: s}
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
type BlockStoreResponse struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Kind      models.BlockStoreKind `json:"kind"`
	Type      string                `json:"type"`
	Config    string                `json:"config,omitempty"`
	CreatedAt time.Time             `json:"created_at"`
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

	if _, err := h.store.CreateBlockStore(r.Context(), bs); err != nil {
		if errors.Is(err, models.ErrDuplicateStore) {
			Conflict(w, "Block store already exists")
			return
		}
		InternalServerError(w, "Failed to create block store")
		return
	}

	WriteJSONCreated(w, blockStoreToResponse(bs))
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

	response := make([]BlockStoreResponse, len(stores))
	for i, s := range stores {
		response[i] = blockStoreToResponse(s)
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

	WriteJSONOK(w, blockStoreToResponse(bs))
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
	}

	if err := h.store.UpdateBlockStore(r.Context(), bs); err != nil {
		InternalServerError(w, "Failed to update block store")
		return
	}

	WriteJSONOK(w, blockStoreToResponse(bs))
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
// Always returns 200 with health status in the response body.
// The healthy field indicates whether the store is reachable.
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

	start := time.Now()
	healthy, details := checkBlockStoreConfigHealth(ctx, bs)
	latency := time.Since(start)

	WriteJSONOK(w, BlockStoreHealthResponse{
		Healthy:   healthy,
		LatencyMs: latency.Milliseconds(),
		CheckedAt: start.UTC().Format(time.RFC3339),
		Details:   details,
	})
}

// checkBlockStoreConfigHealth dispatches a health check based on the block store kind.
func checkBlockStoreConfigHealth(ctx context.Context, bs *models.BlockStoreConfig) (healthy bool, details string) {
	switch bs.Kind {
	case models.BlockStoreKindLocal:
		return checkLocalBlockStoreHealth(ctx, bs)
	case models.BlockStoreKindRemote:
		return checkRemoteBlockStoreHealth(ctx, bs)
	default:
		return false, fmt.Sprintf("unknown block store kind: %s", bs.Kind)
	}
}

// checkLocalBlockStoreHealth verifies a local block store is accessible.
func checkLocalBlockStoreHealth(_ context.Context, bs *models.BlockStoreConfig) (bool, string) {
	if bs.Type == "memory" {
		return true, "in-memory store is always healthy"
	}
	if bs.Type != "fs" {
		return false, fmt.Sprintf("unknown local store type: %s", bs.Type)
	}

	config, err := bs.GetConfig()
	if err != nil {
		return false, "failed to parse store configuration"
	}

	rawPath, _ := config["path"].(string)
	if rawPath == "" {
		return false, "no path configured"
	}
	path := filepath.Clean(rawPath)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "configured path does not exist"
		}
		return false, "cannot access configured path"
	}
	if !info.IsDir() {
		return false, "configured path is not a directory"
	}

	f, err := os.CreateTemp(path, ".dfs-health-check-*")
	if err != nil {
		return false, "configured path is not writable"
	}
	_ = f.Close()
	_ = os.Remove(f.Name())

	return true, "path accessible and writable"
}

// checkRemoteBlockStoreHealth verifies a remote block store is reachable.
func checkRemoteBlockStoreHealth(ctx context.Context, bs *models.BlockStoreConfig) (bool, string) {
	if bs.Type == "memory" {
		return true, "in-memory store is always healthy"
	}
	if bs.Type != "s3" {
		return false, fmt.Sprintf("unknown remote store type: %s", bs.Type)
	}

	config, err := bs.GetConfig()
	if err != nil {
		return false, "failed to parse store configuration"
	}

	bucket, _ := config["bucket"].(string)
	if bucket == "" {
		return false, "no bucket configured"
	}

	region := "us-east-1"
	if r, ok := config["region"].(string); ok && r != "" {
		region = r
	}

	endpoint, _ := config["endpoint"].(string)
	accessKey, _ := config["access_key_id"].(string)
	secretKey, _ := config["secret_access_key"].(string)
	forcePathStyle, hasPathStyle := config["force_path_style"].(bool)
	if endpoint != "" && !hasPathStyle {
		forcePathStyle = true
	}

	remoteStore, err := s3.NewFromConfig(ctx, s3.Config{
		Bucket:         bucket,
		Region:         region,
		Endpoint:       endpoint,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		ForcePathStyle: forcePathStyle,
	})
	if err != nil {
		return false, "failed to initialize S3 client"
	}
	defer func() { _ = remoteStore.Close() }()

	if err := remoteStore.HealthCheck(ctx); err != nil {
		return false, "S3 connectivity check failed"
	}

	return true, fmt.Sprintf("S3 bucket accessible: %s (region: %s)", bucket, region)
}
