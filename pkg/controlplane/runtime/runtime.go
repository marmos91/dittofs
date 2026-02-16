package runtime

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload"
)

// DefaultShutdownTimeout is the default timeout for graceful adapter shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// ProtocolAdapter is an interface for protocol adapters (NFS, SMB) that can be
// managed by Runtime. This interface is defined here to break the import cycle
// between runtime and adapter packages.
//
// Note: SetRuntime is handled separately via RuntimeSetter interface to avoid
// type compatibility issues across package boundaries.
type ProtocolAdapter interface {
	// Serve starts the protocol server and blocks until the context is cancelled.
	Serve(ctx context.Context) error
	// Stop initiates graceful shutdown of the protocol server.
	Stop(ctx context.Context) error
	// Protocol returns the protocol name (e.g., "NFS", "SMB").
	Protocol() string
	// Port returns the TCP port the adapter is listening on.
	Port() int
}

// RuntimeSetter is implemented by adapters that need runtime access.
// This is separate from ProtocolAdapter to avoid type compatibility issues.
type RuntimeSetter interface {
	SetRuntime(rt *Runtime)
}

// AuxiliaryServer is an interface for auxiliary HTTP servers (API, Metrics)
// that can be managed alongside protocol adapters.
type AuxiliaryServer interface {
	// Start starts the HTTP server and blocks until context is cancelled or error.
	Start(ctx context.Context) error
	// Stop initiates graceful shutdown.
	Stop(ctx context.Context) error
	// Port returns the TCP port the server is listening on.
	Port() int
}

// AdapterFactory is a function that creates a ProtocolAdapter from configuration.
// This allows Runtime to manage adapters without importing the adapter package.
type AdapterFactory func(cfg *models.AdapterConfig) (ProtocolAdapter, error)

// CacheConfig contains cache configuration for the runtime.
// This is a minimal config struct to avoid import cycles with pkg/config.
type CacheConfig struct {
	// Path is the directory for the cache WAL file (e.g., /var/lib/dittofs/cache)
	Path string
	// Size is the maximum cache size in bytes
	Size uint64
}

// adapterEntry holds adapter state for lifecycle management.
type adapterEntry struct {
	adapter ProtocolAdapter
	config  *models.AdapterConfig
	ctx     context.Context
	cancel  context.CancelFunc
	errCh   chan error
}

// Runtime manages all runtime state for shares and protocol adapters.
// It provides thread-safe registration and lookup of all server resources.
//
// The Runtime combines:
//   - Configuration from the persistent Store
//   - Live metadata store instances
//   - Share runtime state (root handles)
//   - Active mounts from NFS clients (ephemeral)
//   - Protocol adapters (NFS, SMB)
//
// Runtime is the single entrypoint for all operations - both API handlers
// and internal code should use Runtime methods for all mutations to ensure
// both store and in-memory state stay in sync.
//
// Content Model:
// All content operations go through the PayloadService which includes
// caching and transfer management. The PayloadService is created lazily
// when the first share that uses a payload store is created.
type Runtime struct {
	mu              sync.RWMutex
	metadata        map[string]metadata.MetadataStore // Named metadata store instances
	shares          map[string]*Share                 // Share runtime state
	mounts          map[string]*MountInfo             // Active NFS mounts (key: clientAddr)
	store           store.Store                       // Persistent configuration store
	metadataService *metadata.MetadataService         // High-level metadata operations
	payloadService  *payload.PayloadService           // High-level content operations
	cacheInstance   *cache.Cache                      // WAL-backed cache for content operations

	// Cache configuration (set at startup, used for lazy PayloadService creation)
	cacheConfig *CacheConfig

	// Adapter management
	adaptersMu     sync.RWMutex
	adapters       map[string]*adapterEntry // key: adapter type (nfs, smb)
	adapterFactory AdapterFactory           // Factory to create adapters from config

	// Auxiliary servers
	metricsServer AuxiliaryServer
	apiServer     AuxiliaryServer

	// Settings hot-reload
	settingsWatcher *SettingsWatcher

	// DNS cache for netgroup hostname matching
	dnsCache     *dnsCache
	dnsCacheOnce sync.Once

	// Shutdown management
	shutdownTimeout time.Duration

	// serveOnce ensures Serve() is only called once
	serveOnce sync.Once
	served    bool
}

// New creates a new Runtime with the given persistent store.
func New(s store.Store) *Runtime {
	rt := &Runtime{
		metadata:        make(map[string]metadata.MetadataStore),
		shares:          make(map[string]*Share),
		mounts:          make(map[string]*MountInfo),
		adapters:        make(map[string]*adapterEntry),
		store:           s,
		metadataService: metadata.New(),
		shutdownTimeout: DefaultShutdownTimeout,
	}

	// Create settings watcher if store is available
	if s != nil {
		rt.settingsWatcher = NewSettingsWatcher(s, DefaultPollInterval)
	}

	return rt
}

// SetAdapterFactory sets the factory function for creating adapters.
// This must be called before CreateAdapter or LoadAdaptersFromStore.
func (r *Runtime) SetAdapterFactory(factory AdapterFactory) {
	r.adaptersMu.Lock()
	defer r.adaptersMu.Unlock()
	r.adapterFactory = factory
}

// SetShutdownTimeout sets the maximum time to wait for graceful adapter shutdown.
func (r *Runtime) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	r.shutdownTimeout = d
}

// SetMetricsServer sets the metrics HTTP server for the runtime.
// This is optional - if not set, metrics collection is disabled.
// Must be called before Serve().
func (r *Runtime) SetMetricsServer(server AuxiliaryServer) {
	if r.served {
		panic("cannot set metrics server after Serve() has been called")
	}
	r.metricsServer = server
	if server != nil {
		logger.Info("Metrics server registered", "port", server.Port())
	}
}

// SetAPIServer sets the REST API HTTP server for the runtime.
// This is optional - if not set, API endpoints are not available.
// Must be called before Serve().
func (r *Runtime) SetAPIServer(server AuxiliaryServer) {
	if r.served {
		panic("cannot set API server after Serve() has been called")
	}
	r.apiServer = server
	if server != nil {
		logger.Info("API server registered", "port", server.Port())
	}
}

// Serve starts all adapters and auxiliary servers, and blocks until shutdown.
//
// This method orchestrates the lifecycle of the entire DittoFS server:
//  1. Loads and starts adapters from the store
//  2. Starts metrics server (if configured)
//  3. Starts API server (if configured)
//  4. Waits for context cancellation
//  5. Gracefully shuts down all components
//
// Serve() should only be called once. Calling it multiple times will panic.
func (r *Runtime) Serve(ctx context.Context) error {
	var err error

	r.serveOnce.Do(func() {
		r.served = true
		err = r.serve(ctx)
	})

	return err
}

// serve is the internal implementation of Serve().
func (r *Runtime) serve(ctx context.Context) error {
	logger.Info("Starting DittoFS runtime")

	// 0. Initialize settings watcher (load initial settings before adapters start)
	if r.settingsWatcher != nil {
		if err := r.settingsWatcher.LoadInitial(ctx); err != nil {
			logger.Warn("Failed to load initial adapter settings", "error", err)
			// Non-fatal: adapters will use static config until settings are available
		}
		r.settingsWatcher.Start(ctx)
	}

	// 1. Load and start adapters from store
	if err := r.LoadAdaptersFromStore(ctx); err != nil {
		return fmt.Errorf("failed to load adapters: %w", err)
	}

	// 2. Start metrics server if configured
	metricsErrChan := make(chan error, 1)
	if r.metricsServer != nil {
		go func() {
			if err := r.metricsServer.Start(ctx); err != nil {
				logger.Error("Metrics server error", "error", err)
				metricsErrChan <- err
			}
		}()
	}

	// 3. Start API server if configured
	apiErrChan := make(chan error, 1)
	if r.apiServer != nil {
		go func() {
			if err := r.apiServer.Start(ctx); err != nil {
				logger.Error("API server error", "error", err)
				apiErrChan <- err
			}
		}()
	}

	// 4. Wait for shutdown signal or server error
	var shutdownErr error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received", "reason", ctx.Err())
		shutdownErr = ctx.Err()

	case err := <-metricsErrChan:
		logger.Error("Metrics server failed - initiating shutdown", "error", err)
		shutdownErr = fmt.Errorf("metrics server error: %w", err)

	case err := <-apiErrChan:
		logger.Error("API server failed - initiating shutdown", "error", err)
		shutdownErr = fmt.Errorf("API server error: %w", err)
	}

	// 5. Graceful shutdown
	r.shutdown()

	logger.Info("DittoFS runtime stopped")
	return shutdownErr
}

// shutdown performs graceful shutdown of all components.
func (r *Runtime) shutdown() {
	// Stop settings watcher first (no more polling)
	if r.settingsWatcher != nil {
		logger.Debug("Stopping settings watcher")
		r.settingsWatcher.Stop()
	}

	// Stop all adapters (with connection draining)
	logger.Info("Stopping all adapters")
	if err := r.StopAllAdapters(); err != nil {
		logger.Warn("Error stopping adapters", "error", err)
	}

	// Close metadata stores
	logger.Info("Closing metadata stores")
	r.CloseMetadataStores()

	// Stop metrics server
	if r.metricsServer != nil {
		logger.Debug("Stopping metrics server")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.metricsServer.Stop(ctx); err != nil {
			logger.Error("Metrics server shutdown error", "error", err)
		}
	}

	// Stop API server
	if r.apiServer != nil {
		logger.Debug("Stopping API server")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.apiServer.Stop(ctx); err != nil {
			logger.Error("API server shutdown error", "error", err)
		}
	}
}

// SetPayloadService sets the PayloadService for content operations.
// This should be called during initialization after creating the cache
// and transfer manager.
func (r *Runtime) SetPayloadService(ps *payload.PayloadService) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloadService = ps
}

// SetCacheConfig stores cache configuration for lazy initialization.
// The cache and PayloadService are not created until the first share is added.
// This saves memory when no shares are configured.
func (r *Runtime) SetCacheConfig(cfg *CacheConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheConfig = cfg
}

// GetCacheConfig returns the cache configuration.
func (r *Runtime) GetCacheConfig() *CacheConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheConfig
}

// SetCache stores the cache instance in the runtime.
// This is called after the cache is created, either at startup (if payload
// stores exist) or lazily when the first payload store is added via API.
func (r *Runtime) SetCache(c *cache.Cache) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheInstance = c
}

// GetCache returns the cache instance, or nil if not yet created.
func (r *Runtime) GetCache() *cache.Cache {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheInstance
}

// Store returns the persistent configuration store.
func (r *Runtime) Store() store.Store {
	return r.store
}

// ============================================================================
// Metadata Store Management
// ============================================================================

// RegisterMetadataStore adds a named metadata store instance to the runtime.
// Returns an error if a store with the same name already exists.
func (r *Runtime) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	if metaStore == nil {
		return fmt.Errorf("cannot register nil metadata store")
	}
	if name == "" {
		return fmt.Errorf("cannot register metadata store with empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.metadata[name]; exists {
		return fmt.Errorf("metadata store %q already registered", name)
	}

	r.metadata[name] = metaStore
	return nil
}

// GetMetadataStore retrieves a metadata store instance by name.
func (r *Runtime) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	metaStore, exists := r.metadata[name]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found", name)
	}
	return metaStore, nil
}

// GetMetadataStoreForShare retrieves the metadata store used by the specified share.
func (r *Runtime) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	metaStore, exists := r.metadata[share.MetadataStore]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found for share %q", share.MetadataStore, shareName)
	}

	return metaStore, nil
}

// ListMetadataStores returns all registered metadata store names.
func (r *Runtime) ListMetadataStores() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.metadata))
	for name := range r.metadata {
		names = append(names, name)
	}
	return names
}

// ============================================================================
// Share Management
// ============================================================================

// AddShare creates and registers a new share with the given configuration.
// This method:
//  1. Ensures PayloadService is initialized (required for content operations)
//  2. Validates that the share doesn't already exist
//  3. Validates that the referenced metadata store exists
//  4. Creates the root directory in the metadata store
//  5. Registers the share in the runtime
func (r *Runtime) AddShare(ctx context.Context, config *ShareConfig) error {
	if config.Name == "" {
		return fmt.Errorf("cannot add share with empty name")
	}

	// Ensure PayloadService is initialized before creating shares
	// This must be called before acquiring the lock to avoid deadlock
	// Skip if:
	// 1. PayloadService is already set (e.g., in tests), OR
	// 2. Store is nil (testing mode - can't initialize PayloadService without store)
	r.mu.RLock()
	hasPayloadService := r.payloadService != nil
	hasStore := r.store != nil
	r.mu.RUnlock()

	if !hasPayloadService && hasStore {
		if err := r.EnsurePayloadService(ctx); err != nil {
			return fmt.Errorf("failed to initialize payload service: %w", err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Validate that metadata service exists
	if r.metadataService == nil {
		return fmt.Errorf("metadata service not initialized")
	}

	// Check if share already exists
	if _, exists := r.shares[config.Name]; exists {
		return fmt.Errorf("share %q already exists", config.Name)
	}

	// Validate that metadata store exists
	metadataStore, exists := r.metadata[config.MetadataStore]
	if !exists {
		return fmt.Errorf("metadata store %q not found", config.MetadataStore)
	}

	// Create root directory in metadata store
	rootAttr := config.RootAttr
	if rootAttr == nil {
		rootAttr = &metadata.FileAttr{}
	}
	if rootAttr.Type == 0 {
		rootAttr.Type = metadata.FileTypeDirectory
	}
	if rootAttr.Mode == 0 {
		rootAttr.Mode = 0777
	}
	if rootAttr.Atime.IsZero() {
		now := time.Now()
		rootAttr.Atime = now
		rootAttr.Mtime = now
		rootAttr.Ctime = now
	}

	// Create the root directory
	rootFile, err := metadataStore.CreateRootDirectory(ctx, config.Name, rootAttr)
	if err != nil {
		return fmt.Errorf("failed to create root directory: %w", err)
	}

	// Encode the root file handle
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		return fmt.Errorf("failed to encode root handle: %w", err)
	}

	// Apply security policy defaults.
	// AllowAuthSys defaults to true (standard NFS behavior) when the caller
	// does not explicitly set it. Since Go's bool zero value is false, we
	// use AllowAuthSysSet to distinguish "explicitly disabled" from "not set".
	allowAuthSys := config.AllowAuthSys
	if !config.AllowAuthSysSet && !allowAuthSys {
		allowAuthSys = true
	}

	// Create share struct
	share := &Share{
		Name:               config.Name,
		MetadataStore:      config.MetadataStore,
		RootHandle:         rootHandle,
		ReadOnly:           config.ReadOnly,
		DefaultPermission:  config.DefaultPermission,
		Squash:             config.Squash,
		AnonymousUID:       config.AnonymousUID,
		AnonymousGID:       config.AnonymousGID,
		DisableReaddirplus: config.DisableReaddirplus,
		AllowAuthSys:       allowAuthSys,
		RequireKerberos:    config.RequireKerberos,
		MinKerberosLevel:   config.MinKerberosLevel,
		NetgroupName:       config.NetgroupName,
		BlockedOperations:  config.BlockedOperations,
	}

	r.shares[config.Name] = share

	// Register the metadata store with the MetadataService for this share
	if err := r.metadataService.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		delete(r.shares, config.Name)
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	return nil
}

// RemoveShare removes a share from the runtime.
// Note: This does NOT close the underlying metadata store.
func (r *Runtime) RemoveShare(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, exists := r.shares[name]
	if !exists {
		return fmt.Errorf("share %q not found", name)
	}

	delete(r.shares, name)
	return nil
}

// UpdateShare updates a share's configuration in the runtime.
// Only updates fields that can be changed without reloading (ReadOnly, DefaultPermission).
func (r *Runtime) UpdateShare(name string, readOnly *bool, defaultPermission *string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	share, exists := r.shares[name]
	if !exists {
		return fmt.Errorf("share %q not found", name)
	}

	if readOnly != nil {
		share.ReadOnly = *readOnly
	}
	if defaultPermission != nil {
		share.DefaultPermission = *defaultPermission
	}

	return nil
}

// GetShare retrieves a share by name.
func (r *Runtime) GetShare(name string) (*Share, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	return share, nil
}

// GetRootHandle retrieves the root file handle for a share.
func (r *Runtime) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return share.RootHandle, nil
}

// ListShares returns all registered share names.
func (r *Runtime) ListShares() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.shares))
	for name := range r.shares {
		names = append(names, name)
	}
	return names
}

// ShareExists checks if a share exists in the runtime.
func (r *Runtime) ShareExists(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.shares[name]
	return exists
}

// ============================================================================
// Service Access
// ============================================================================

// GetMetadataService returns the MetadataService for high-level operations.
func (r *Runtime) GetMetadataService() *metadata.MetadataService {
	return r.metadataService
}

// GetPayloadService returns the PayloadService for high-level content operations.
func (r *Runtime) GetPayloadService() *payload.PayloadService {
	return r.payloadService
}

// ============================================================================
// Mount Tracking (Ephemeral)
// ============================================================================

// RecordMount registers that a client has mounted a share.
func (r *Runtime) RecordMount(clientAddr, shareName string, mountTime int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mounts[clientAddr] = &MountInfo{
		ClientAddr: clientAddr,
		ShareName:  shareName,
		MountTime:  mountTime,
	}
}

// RemoveMount removes a mount record for the given client.
func (r *Runtime) RemoveMount(clientAddr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.mounts[clientAddr]; exists {
		delete(r.mounts, clientAddr)
		return true
	}
	return false
}

// RemoveAllMounts removes all mount records.
func (r *Runtime) RemoveAllMounts() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := len(r.mounts)
	r.mounts = make(map[string]*MountInfo)
	return count
}

// ListMounts returns all active mount records.
func (r *Runtime) ListMounts() []*MountInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	mounts := make([]*MountInfo, 0, len(r.mounts))
	for _, mount := range r.mounts {
		mounts = append(mounts, &MountInfo{
			ClientAddr: mount.ClientAddr,
			ShareName:  mount.ShareName,
			MountTime:  mount.MountTime,
		})
	}
	return mounts
}

// ============================================================================
// Identity Mapping
// ============================================================================

// ApplyIdentityMapping applies share-level identity mapping rules.
//
// This implements Synology-style squash modes:
//   - none: No mapping, UIDs pass through unchanged
//   - root_to_admin: Root (UID 0) retains admin privileges (default)
//   - root_to_guest: Root (UID 0) is mapped to anonymous (root_squash)
//   - all_to_admin: All users are mapped to root (UID 0)
//   - all_to_guest: All users are mapped to anonymous (all_squash)
//
// AUTH_NULL (nil UID) is always mapped to anonymous regardless of squash mode.
func (r *Runtime) ApplyIdentityMapping(shareName string, identity *metadata.Identity) (*metadata.Identity, error) {
	r.mu.RLock()
	share, exists := r.shares[shareName]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	// Create effective identity (copy of original)
	effective := &metadata.Identity{
		UID:      identity.UID,
		GID:      identity.GID,
		GIDs:     identity.GIDs,
		Username: identity.Username,
	}

	// Handle AUTH_NULL (anonymous access) - always map to anonymous
	if identity.UID == nil {
		applyAnonymousIdentity(effective, share.AnonymousUID, share.AnonymousGID)
		return effective, nil
	}

	// Apply squash based on mode
	// Empty string ("") is treated as the default (SquashRootToAdmin)
	switch share.Squash {
	case "", models.SquashNone, models.SquashRootToAdmin:
		// No mapping - UIDs pass through (root keeps admin)

	case models.SquashRootToGuest:
		// Map root (UID 0) to anonymous
		if *identity.UID == 0 {
			applyAnonymousIdentity(effective, share.AnonymousUID, share.AnonymousGID)
		}

	case models.SquashAllToAdmin:
		// Map all users to root
		applyRootIdentity(effective)

	case models.SquashAllToGuest:
		// Map all users to anonymous
		applyAnonymousIdentity(effective, share.AnonymousUID, share.AnonymousGID)
	}

	return effective, nil
}

// applyAnonymousIdentity sets the identity to anonymous with the given UID/GID.
func applyAnonymousIdentity(identity *metadata.Identity, anonUID, anonGID uint32) {
	identity.UID = &anonUID
	identity.GID = &anonGID
	identity.GIDs = []uint32{anonGID}
	identity.Username = fmt.Sprintf("anonymous(%d)", anonUID)
}

// applyRootIdentity sets the identity to root (UID/GID 0).
func applyRootIdentity(identity *metadata.Identity) {
	rootUID, rootGID := uint32(0), uint32(0)
	identity.UID = &rootUID
	identity.GID = &rootGID
	identity.GIDs = []uint32{rootGID}
	identity.Username = "root"
}

// GetShareNameForHandle extracts the share name from a file handle.
func (r *Runtime) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return "", fmt.Errorf("failed to decode share handle: %w", err)
	}

	// Verify share exists in runtime
	r.mu.RLock()
	_, exists := r.shares[shareName]
	r.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("share %q not found in runtime", shareName)
	}

	return shareName, nil
}

// ============================================================================
// Store Interfaces (for protocol handlers)
// ============================================================================

// GetUserStore returns the store as a UserStore for SMB handlers.
func (r *Runtime) GetUserStore() models.UserStore {
	return r.store
}

// GetIdentityStore returns the store as an IdentityStore for NFS handlers.
func (r *Runtime) GetIdentityStore() models.IdentityStore {
	return r.store
}

// GetBlockService returns the PayloadService for backwards compatibility.
// Deprecated: Use GetPayloadService instead.
func (r *Runtime) GetBlockService() *payload.PayloadService {
	return r.payloadService
}

// CountMetadataStores returns the number of registered metadata stores.
func (r *Runtime) CountMetadataStores() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.metadata)
}

// CountShares returns the number of registered shares.
func (r *Runtime) CountShares() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.shares)
}

// ============================================================================
// Adapter Management
// ============================================================================

// CreateAdapter saves the adapter config to store AND starts it immediately.
// This is the method that API handlers should call - it ensures both persistent
// store and in-memory state are updated together.
func (r *Runtime) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	// 1. Save to store
	if _, err := r.store.CreateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to save adapter config: %w", err)
	}

	// 2. Start the adapter
	if err := r.startAdapter(cfg); err != nil {
		// Rollback: delete from store
		_ = r.store.DeleteAdapter(ctx, cfg.Type)
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	return nil
}

// DeleteAdapter stops the running adapter (drains connections) AND removes from store.
func (r *Runtime) DeleteAdapter(ctx context.Context, adapterType string) error {
	// 1. Stop running adapter (drains connections)
	if err := r.stopAdapter(adapterType); err != nil {
		logger.Warn("Adapter stop failed during delete", "type", adapterType, "error", err)
		// Continue with deletion even if stop fails
	}

	// 2. Delete from store
	if err := r.store.DeleteAdapter(ctx, adapterType); err != nil {
		return fmt.Errorf("failed to delete adapter from store: %w", err)
	}

	return nil
}

// UpdateAdapter restarts the adapter with new configuration.
// Updates store first, then restarts the running adapter.
func (r *Runtime) UpdateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	// 1. Update store
	if err := r.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to update adapter config: %w", err)
	}

	// 2. Stop old adapter (if running)
	_ = r.stopAdapter(cfg.Type)

	// 3. Start with new config if enabled
	if cfg.Enabled {
		if err := r.startAdapter(cfg); err != nil {
			logger.Warn("Failed to restart adapter after update", "type", cfg.Type, "error", err)
			// Don't fail the update - config was saved successfully
		}
	}

	return nil
}

// EnableAdapter enables an adapter and starts it.
func (r *Runtime) EnableAdapter(ctx context.Context, adapterType string) error {
	cfg, err := r.store.GetAdapter(ctx, adapterType)
	if err != nil {
		return fmt.Errorf("adapter not found: %w", err)
	}

	cfg.Enabled = true
	if err := r.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to enable adapter: %w", err)
	}

	if err := r.startAdapter(cfg); err != nil {
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	return nil
}

// DisableAdapter stops an adapter and disables it.
func (r *Runtime) DisableAdapter(ctx context.Context, adapterType string) error {
	cfg, err := r.store.GetAdapter(ctx, adapterType)
	if err != nil {
		return fmt.Errorf("adapter not found: %w", err)
	}

	// Stop the adapter first
	_ = r.stopAdapter(adapterType)

	// Update store
	cfg.Enabled = false
	if err := r.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to disable adapter: %w", err)
	}

	return nil
}

// startAdapter creates and starts an adapter from config.
// This is an internal method - use CreateAdapter or EnableAdapter for public API.
func (r *Runtime) startAdapter(cfg *models.AdapterConfig) error {
	r.adaptersMu.Lock()
	defer r.adaptersMu.Unlock()

	if _, exists := r.adapters[cfg.Type]; exists {
		return fmt.Errorf("adapter %s already running", cfg.Type)
	}

	if r.adapterFactory == nil {
		return fmt.Errorf("adapter factory not set")
	}

	// Create adapter instance using factory
	adp, err := r.adapterFactory(cfg)
	if err != nil {
		return fmt.Errorf("failed to create adapter: %w", err)
	}

	// Create per-adapter context
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	// Inject runtime into adapter if it supports RuntimeSetter
	if setter, ok := adp.(RuntimeSetter); ok {
		setter.SetRuntime(r)
	}

	// Start in goroutine
	go func() {
		logger.Info("Starting adapter", "protocol", adp.Protocol(), "port", adp.Port())
		err := adp.Serve(ctx)
		if err != nil && err != context.Canceled && ctx.Err() == nil {
			logger.Error("Adapter failed", "protocol", adp.Protocol(), "error", err)
		}
		errCh <- err
	}()

	r.adapters[cfg.Type] = &adapterEntry{
		adapter: adp,
		config:  cfg,
		ctx:     ctx,
		cancel:  cancel,
		errCh:   errCh,
	}

	logger.Info("Adapter started", "type", cfg.Type, "port", cfg.Port)
	return nil
}

// stopAdapter stops a running adapter with connection draining.
// This is an internal method - use DeleteAdapter or DisableAdapter for public API.
func (r *Runtime) stopAdapter(adapterType string) error {
	r.adaptersMu.Lock()
	entry, exists := r.adapters[adapterType]
	if !exists {
		r.adaptersMu.Unlock()
		return fmt.Errorf("adapter %s not running", adapterType)
	}
	delete(r.adapters, adapterType)
	r.adaptersMu.Unlock()

	// Create timeout context for graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), r.shutdownTimeout)
	defer cancel()

	logger.Info("Stopping adapter", "type", adapterType)

	// Signal adapter to stop (triggers connection draining)
	if err := entry.adapter.Stop(ctx); err != nil {
		logger.Warn("Adapter stop error", "type", adapterType, "error", err)
	}

	// Cancel adapter's context
	entry.cancel()

	// Wait for adapter goroutine
	select {
	case <-entry.errCh:
		logger.Info("Adapter stopped", "type", adapterType)
		return nil
	case <-ctx.Done():
		logger.Warn("Adapter stop timed out", "type", adapterType)
		return fmt.Errorf("adapter %s stop timed out", adapterType)
	}
}

// StopAllAdapters stops all running adapters (for shutdown).
func (r *Runtime) StopAllAdapters() error {
	r.adaptersMu.RLock()
	types := make([]string, 0, len(r.adapters))
	for t := range r.adapters {
		types = append(types, t)
	}
	r.adaptersMu.RUnlock()

	var lastErr error
	for _, t := range types {
		if err := r.stopAdapter(t); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// LoadAdaptersFromStore loads enabled adapters from store and starts them.
// This is called during server startup.
func (r *Runtime) LoadAdaptersFromStore(ctx context.Context) error {
	adapters, err := r.store.ListAdapters(ctx)
	if err != nil {
		return fmt.Errorf("failed to list adapters: %w", err)
	}

	for _, cfg := range adapters {
		if !cfg.Enabled {
			logger.Info("Adapter disabled, skipping", "type", cfg.Type)
			continue
		}

		if err := r.startAdapter(cfg); err != nil {
			return fmt.Errorf("failed to start adapter %s: %w", cfg.Type, err)
		}
	}

	return nil
}

// ListRunningAdapters returns information about currently running adapters.
func (r *Runtime) ListRunningAdapters() []string {
	r.adaptersMu.RLock()
	defer r.adaptersMu.RUnlock()

	types := make([]string, 0, len(r.adapters))
	for t := range r.adapters {
		types = append(types, t)
	}
	return types
}

// IsAdapterRunning checks if an adapter is currently running.
func (r *Runtime) IsAdapterRunning(adapterType string) bool {
	r.adaptersMu.RLock()
	defer r.adaptersMu.RUnlock()
	_, exists := r.adapters[adapterType]
	return exists
}

// AddAdapter adds and starts a pre-created adapter directly.
// This bypasses the store and is primarily for testing.
// The adapter will be registered under its Protocol() name.
func (r *Runtime) AddAdapter(adapter ProtocolAdapter) error {
	adapterType := adapter.Protocol()

	r.adaptersMu.Lock()
	defer r.adaptersMu.Unlock()

	if _, exists := r.adapters[adapterType]; exists {
		return fmt.Errorf("adapter %s already running", adapterType)
	}

	// Create per-adapter context
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	// Inject runtime into adapter if it supports RuntimeSetter
	if setter, ok := adapter.(RuntimeSetter); ok {
		setter.SetRuntime(r)
	}

	// Start in goroutine
	go func() {
		logger.Info("Starting adapter", "protocol", adapter.Protocol(), "port", adapter.Port())
		err := adapter.Serve(ctx)
		if err != nil && err != context.Canceled && ctx.Err() == nil {
			logger.Error("Adapter failed", "protocol", adapter.Protocol(), "error", err)
		}
		errCh <- err
	}()

	r.adapters[adapterType] = &adapterEntry{
		adapter: adapter,
		config:  &models.AdapterConfig{Type: adapterType, Port: adapter.Port(), Enabled: true},
		ctx:     ctx,
		cancel:  cancel,
		errCh:   errCh,
	}

	logger.Info("Adapter added and started", "type", adapterType, "port", adapter.Port())
	return nil
}

// ============================================================================
// Settings Access (Hot-Reload)
// ============================================================================

// GetSettingsWatcher returns the settings watcher for direct access.
// Adapters can use this to access the full watcher API.
func (r *Runtime) GetSettingsWatcher() *SettingsWatcher {
	return r.settingsWatcher
}

// GetNFSSettings returns the current NFS adapter settings from the watcher cache.
// Returns nil if no NFS settings are available (watcher not initialized or NFS adapter not configured).
// Callers must NOT mutate the returned struct.
func (r *Runtime) GetNFSSettings() *models.NFSAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetNFSSettings()
}

// GetSMBSettings returns the current SMB adapter settings from the watcher cache.
// Returns nil if no SMB settings are available (watcher not initialized or SMB adapter not configured).
// Callers must NOT mutate the returned struct.
func (r *Runtime) GetSMBSettings() *models.SMBAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetSMBSettings()
}

// ============================================================================
// Metadata Store Lifecycle
// ============================================================================

// CloseMetadataStores closes all registered metadata stores.
// This should be called during graceful shutdown.
func (r *Runtime) CloseMetadataStores() {
	// Collect stores while holding lock
	r.mu.RLock()
	stores := make(map[string]metadata.MetadataStore, len(r.metadata))
	for name, store := range r.metadata {
		stores[name] = store
	}
	r.mu.RUnlock()

	// Close stores outside of lock
	for name, store := range stores {
		if closer, ok := store.(io.Closer); ok {
			logger.Debug("Closing metadata store", "store", name)
			if err := closer.Close(); err != nil {
				logger.Error("Metadata store close error", "store", name, "error", err)
			}
		}
	}
}
