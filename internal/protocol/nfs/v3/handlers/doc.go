package handlers

import (
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Handler is the concrete implementation for NFS v3 protocol handlers.
// It processes all NFSv3 procedures (LOOKUP, READ, WRITE, etc.) and uses
// the registry to access per-share stores and configuration.
type Handler struct {
	// Registry provides access to all stores and shares
	// Exported to allow injection by the NFS adapter
	Registry *registry.Registry

	// Metrics collects observability data for NFS operations
	// Optional - may be nil to disable metrics with zero overhead
	Metrics metrics.NFSMetrics

	// fileLocks provides per-ContentID mutexes to prevent race conditions
	// when multiple concurrent COMMIT operations target the same file.
	// This prevents session conflicts during incremental uploads.
	// Uses sync.Map for lock-free reads and minimal contention.
	fileLocks sync.Map // map[metadata.ContentID]*sync.Mutex
}

// getMetadataStore retrieves the metadata store for the share specified in the context.
// This helper consolidates the common pattern of:
//  1. Checking if the share exists
//  2. Getting the metadata store for the share
//
// Returns:
//   - metadata.MetadataStore: The metadata store for the share
//   - error: If the share doesn't exist or the store cannot be retrieved
func (h *Handler) getMetadataStore(ctx *NFSHandlerContext) (metadata.MetadataStore, error) {
	// Check if share exists
	if !h.Registry.ShareExists(ctx.Share) {
		return nil, fmt.Errorf("share not found: %s", ctx.Share)
	}

	// Get metadata store for this share
	metadataStore, err := h.Registry.GetMetadataStoreForShare(ctx.Share)
	if err != nil {
		return nil, fmt.Errorf("cannot get metadata store for share %s: %w", ctx.Share, err)
	}

	return metadataStore, nil
}

// getContentStore retrieves the content store for the share specified in the context.
// This helper is used by handlers that need to access file data (READ, WRITE, REMOVE, etc.).
//
// Returns:
//   - content.ContentStore: The content store for the share
//   - error: If the share doesn't exist or the store cannot be retrieved
func (h *Handler) getContentStore(ctx *NFSHandlerContext) (content.ContentStore, error) {
	// Check if share exists
	if !h.Registry.ShareExists(ctx.Share) {
		return nil, fmt.Errorf("share not found: %s", ctx.Share)
	}

	// Get content store for this share
	contentStore, err := h.Registry.GetContentStoreForShare(ctx.Share)
	if err != nil {
		return nil, fmt.Errorf("cannot get content store for share %s: %w", ctx.Share, err)
	}

	return contentStore, nil
}
