package handlers

import (
	"sync"

	"github.com/marmos91/dittofs/pkg/registry"
)

// Handler is the concrete implementation for NFS v3 protocol handlers.
// It processes all NFSv3 procedures (LOOKUP, READ, WRITE, etc.) and uses
// the registry to access per-share stores and configuration.
type Handler struct {
	// Registry provides access to all stores and shares
	// Exported to allow injection by the NFS adapter
	Registry *registry.Registry

	// fileLocks provides per-ContentID mutexes to prevent race conditions
	// when multiple concurrent COMMIT operations target the same file.
	// This prevents session conflicts during incremental uploads.
	// Uses sync.Map for lock-free reads and minimal contention.
	fileLocks sync.Map // map[metadata.ContentID]*sync.Mutex
}
