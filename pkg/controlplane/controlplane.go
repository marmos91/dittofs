// Package controlplane provides the control plane for DittoFS.
//
// The control plane manages:
//   - Persistent configuration (users, groups, shares, store configs) via Store
//   - Runtime state (metadata store instances, share handles, mounts) via Runtime
//   - REST API for management operations via API Server
//
// Usage:
//
//	cp, err := controlplane.New(ctx, cfg)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer cp.Close()
//
//	// Access runtime for protocol adapters
//	runtime := cp.Runtime()
package controlplane

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// ControlPlane is the central management component for DittoFS.
//
// It owns and coordinates:
//   - Store: Persistent configuration (users, groups, permissions)
//   - Runtime: Ephemeral state (metadata stores, handles, mounts)
//   - API Server: REST API for management (optional)
//
// The ControlPlane provides a unified initialization path and ensures
// proper coordination between components.
type ControlPlane struct {
	store     *store.GORMStore
	runtime   *runtime.Runtime
	apiServer *api.Server
}

// Options configures the ControlPlane.
type Options struct {
	// Database configuration for persistent storage
	Database *store.Config

	// API configuration (optional - set Enabled=false to disable)
	API *api.APIConfig
}

// New creates a new ControlPlane with the given options.
//
// This initializes:
//  1. Persistent store (SQLite/PostgreSQL)
//  2. Runtime for ephemeral state
//  3. API server (if enabled)
//
// Call Close() when done to release resources.
func New(ctx context.Context, opts *Options) (*ControlPlane, error) {
	if opts == nil {
		return nil, fmt.Errorf("options cannot be nil")
	}
	if opts.Database == nil {
		return nil, fmt.Errorf("database configuration is required")
	}

	// Create persistent store
	cpStore, err := store.New(opts.Database)
	if err != nil {
		return nil, fmt.Errorf("failed to create store: %w", err)
	}

	// Create runtime with store
	rt := runtime.New(cpStore)

	cp := &ControlPlane{
		store:   cpStore,
		runtime: rt,
	}

	// Initialize API server if enabled
	if opts.API != nil && opts.API.IsEnabled() {
		apiServer, err := api.NewServer(*opts.API, rt, cpStore)
		if err != nil {
			return nil, fmt.Errorf("failed to create API server: %w", err)
		}
		cp.apiServer = apiServer
		logger.Info("Control plane API server initialized", "port", opts.API.Port)
	}

	return cp, nil
}

// Store returns the persistent configuration store.
func (cp *ControlPlane) Store() *store.GORMStore {
	return cp.store
}

// Runtime returns the runtime state manager.
//
// Protocol adapters use the runtime to:
//   - Access metadata stores
//   - Resolve share handles
//   - Apply identity mapping
func (cp *ControlPlane) Runtime() *runtime.Runtime {
	return cp.runtime
}

// APIServer returns the API server (may be nil if not enabled).
func (cp *ControlPlane) APIServer() *api.Server {
	return cp.apiServer
}

// EnsureAdminUser creates the admin user if it doesn't exist.
// Returns the generated password (empty string if user already exists).
func (cp *ControlPlane) EnsureAdminUser(ctx context.Context) (string, error) {
	return cp.store.EnsureAdminUser(ctx)
}

// IdentityStore returns the store as an IdentityStore interface.
// This is used by protocol handlers for identity resolution.
func (cp *ControlPlane) IdentityStore() models.IdentityStore {
	return cp.store
}

// Close releases all resources held by the ControlPlane.
func (cp *ControlPlane) Close() error {
	// Store cleanup is handled by GORM's connection pool
	// No explicit close needed for current implementation
	return nil
}
