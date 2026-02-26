package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/adapters"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload"
)

// DefaultShutdownTimeout is the default timeout for graceful adapter shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// ProtocolAdapter is a type alias for adapters.ProtocolAdapter so that
// existing consumers can continue using runtime.ProtocolAdapter.
type ProtocolAdapter = adapters.ProtocolAdapter

// RuntimeSetter is a type alias for adapters.RuntimeSetter so that
// existing consumers can continue using runtime.RuntimeSetter.
type RuntimeSetter = adapters.RuntimeSetter

// AdapterFactory is a type alias for adapters.AdapterFactory so that
// existing consumers can continue using runtime.AdapterFactory.
type AdapterFactory = adapters.AdapterFactory

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

// CacheConfig contains cache configuration for the runtime.
// This is a minimal config struct to avoid import cycles with pkg/config.
type CacheConfig struct {
	// Path is the directory for the cache WAL file (e.g., /var/lib/dittofs/cache)
	Path string
	// Size is the maximum cache size in bytes
	Size uint64
}

// payloadServiceHelper implements shares.PayloadServiceEnsurer for the Runtime.
type payloadServiceHelper struct {
	rt *Runtime
}

func (h *payloadServiceHelper) EnsurePayloadService(ctx context.Context) error {
	return h.rt.EnsurePayloadService(ctx)
}

func (h *payloadServiceHelper) HasPayloadService() bool {
	h.rt.mu.RLock()
	defer h.rt.mu.RUnlock()
	return h.rt.payloadService != nil
}

func (h *payloadServiceHelper) HasStore() bool {
	return h.rt.store != nil
}

// Runtime manages all runtime state for shares and protocol adapters.
// It provides thread-safe registration and lookup of all server resources.
//
// The Runtime composes 3 focused sub-services (adapters, stores, shares)
// with cross-cutting concerns (lifecycle, identity, mounts) still inline.
// Future plans will extract mounts, lifecycle, and identity into their own
// sub-packages as well.
//
// Content Model:
// All content operations go through the PayloadService which includes
// caching and transfer management. The PayloadService is created lazily
// when the first share that uses a payload store is created.
type Runtime struct {
	mu              sync.RWMutex
	store           store.Store                       // Persistent configuration store
	metadataService *metadata.MetadataService         // High-level metadata operations
	payloadService  *payload.PayloadService           // High-level content operations
	cacheInstance   *cache.Cache                      // WAL-backed cache for content operations

	// Sub-services
	adaptersSvc *adapters.Service
	storesSvc   *stores.Service
	sharesSvc   *shares.Service

	// Unified mount tracking across all protocols (NFS, SMB)
	mountTracker *MountTracker

	// Cache configuration (set at startup, used for lazy PayloadService creation)
	cacheConfig *CacheConfig

	// Auxiliary servers
	apiServer AuxiliaryServer

	// Settings hot-reload
	settingsWatcher *SettingsWatcher

	// Adapter data providers (stored as any to avoid import cycles with internal/adapter)
	adapterProviders   map[string]any
	adapterProvidersMu sync.RWMutex

	// Shutdown management
	shutdownTimeout time.Duration

	// serveOnce ensures Serve() is only called once
	serveOnce sync.Once
	served    bool
}

// New creates a new Runtime with the given persistent store.
func New(s store.Store) *Runtime {
	rt := &Runtime{
		store:            s,
		metadataService:  metadata.New(),
		mountTracker:     NewMountTracker(),
		adapterProviders: make(map[string]any),
		shutdownTimeout:  DefaultShutdownTimeout,
		storesSvc:        stores.New(),
		sharesSvc:        shares.New(),
	}

	// Create adapter service (store may be nil in tests)
	rt.adaptersSvc = adapters.New(s, DefaultShutdownTimeout)
	rt.adaptersSvc.SetRuntime(rt)

	// Create settings watcher if store is available
	if s != nil {
		rt.settingsWatcher = NewSettingsWatcher(s, DefaultPollInterval)
	}

	return rt
}

// ============================================================================
// Adapter Management (delegated to adapters.Service)
// ============================================================================

// SetAdapterFactory sets the factory function for creating adapters.
// This must be called before CreateAdapter or LoadAdaptersFromStore.
func (r *Runtime) SetAdapterFactory(factory AdapterFactory) {
	r.adaptersSvc.SetAdapterFactory(factory)
}

// SetShutdownTimeout sets the maximum time to wait for graceful adapter shutdown.
func (r *Runtime) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	r.shutdownTimeout = d
	r.adaptersSvc.SetShutdownTimeout(d)
}

// CreateAdapter saves the adapter config to store AND starts it immediately.
func (r *Runtime) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	return r.adaptersSvc.CreateAdapter(ctx, cfg)
}

// DeleteAdapter stops the running adapter (drains connections) AND removes from store.
func (r *Runtime) DeleteAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.DeleteAdapter(ctx, adapterType)
}

// UpdateAdapter restarts the adapter with new configuration.
func (r *Runtime) UpdateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	return r.adaptersSvc.UpdateAdapter(ctx, cfg)
}

// EnableAdapter enables an adapter and starts it.
func (r *Runtime) EnableAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.EnableAdapter(ctx, adapterType)
}

// DisableAdapter stops an adapter and disables it.
func (r *Runtime) DisableAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.DisableAdapter(ctx, adapterType)
}

// StopAllAdapters stops all running adapters (for shutdown).
func (r *Runtime) StopAllAdapters() error {
	return r.adaptersSvc.StopAllAdapters()
}

// LoadAdaptersFromStore loads enabled adapters from store and starts them.
func (r *Runtime) LoadAdaptersFromStore(ctx context.Context) error {
	return r.adaptersSvc.LoadAdaptersFromStore(ctx)
}

// ListRunningAdapters returns information about currently running adapters.
func (r *Runtime) ListRunningAdapters() []string {
	return r.adaptersSvc.ListRunningAdapters()
}

// IsAdapterRunning checks if an adapter is currently running.
func (r *Runtime) IsAdapterRunning(adapterType string) bool {
	return r.adaptersSvc.IsAdapterRunning(adapterType)
}

// AddAdapter adds and starts a pre-created adapter directly.
// This bypasses the store and is primarily for testing.
func (r *Runtime) AddAdapter(adapter ProtocolAdapter) error {
	return r.adaptersSvc.AddAdapter(adapter)
}

// ============================================================================
// Metadata Store Management (delegated to stores.Service)
// ============================================================================

// RegisterMetadataStore adds a named metadata store instance to the runtime.
func (r *Runtime) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	return r.storesSvc.RegisterMetadataStore(name, metaStore)
}

// GetMetadataStore retrieves a metadata store instance by name.
func (r *Runtime) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	return r.storesSvc.GetMetadataStore(name)
}

// GetMetadataStoreForShare retrieves the metadata store used by the specified share.
func (r *Runtime) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return r.storesSvc.GetMetadataStoreForShare(share.MetadataStore)
}

// ListMetadataStores returns all registered metadata store names.
func (r *Runtime) ListMetadataStores() []string {
	return r.storesSvc.ListMetadataStores()
}

// CountMetadataStores returns the number of registered metadata stores.
func (r *Runtime) CountMetadataStores() int {
	return r.storesSvc.CountMetadataStores()
}

// CloseMetadataStores closes all registered metadata stores.
func (r *Runtime) CloseMetadataStores() {
	r.storesSvc.CloseMetadataStores()
}

// ============================================================================
// Share Management (delegated to shares.Service)
// ============================================================================

// AddShare creates and registers a new share with the given configuration.
func (r *Runtime) AddShare(ctx context.Context, config *ShareConfig) error {
	return r.sharesSvc.AddShare(ctx, config, r.storesSvc, r.metadataService, &payloadServiceHelper{rt: r})
}

// RemoveShare removes a share from the runtime.
func (r *Runtime) RemoveShare(name string) error {
	return r.sharesSvc.RemoveShare(name)
}

// UpdateShare updates a share's configuration in the runtime.
func (r *Runtime) UpdateShare(name string, readOnly *bool, defaultPermission *string) error {
	return r.sharesSvc.UpdateShare(name, readOnly, defaultPermission)
}

// GetShare retrieves a share by name.
func (r *Runtime) GetShare(name string) (*Share, error) {
	return r.sharesSvc.GetShare(name)
}

// GetRootHandle retrieves the root file handle for a share.
func (r *Runtime) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	return r.sharesSvc.GetRootHandle(shareName)
}

// ListShares returns all registered share names.
func (r *Runtime) ListShares() []string {
	return r.sharesSvc.ListShares()
}

// ShareExists checks if a share exists in the runtime.
func (r *Runtime) ShareExists(name string) bool {
	return r.sharesSvc.ShareExists(name)
}

// OnShareChange registers a callback to be invoked when shares are added,
// removed, or updated.
func (r *Runtime) OnShareChange(callback func(shares []string)) {
	r.sharesSvc.OnShareChange(callback)
}

// GetShareNameForHandle extracts the share name from a file handle.
func (r *Runtime) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	return r.sharesSvc.GetShareNameForHandle(ctx, handle)
}

// CountShares returns the number of registered shares.
func (r *Runtime) CountShares() int {
	return r.sharesSvc.CountShares()
}

// ============================================================================
// Lifecycle: Serve, shutdown
// ============================================================================

// SetAPIServer sets the REST API HTTP server for the runtime.
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

	// 0. Initialize settings watcher
	if r.settingsWatcher != nil {
		if err := r.settingsWatcher.LoadInitial(ctx); err != nil {
			logger.Warn("Failed to load initial adapter settings", "error", err)
		}
		r.settingsWatcher.Start(ctx)
	}

	// 1. Load and start adapters from store
	if err := r.LoadAdaptersFromStore(ctx); err != nil {
		return fmt.Errorf("failed to load adapters: %w", err)
	}

	// 2. Start API server if configured
	apiErrChan := make(chan error, 1)
	if r.apiServer != nil {
		go func() {
			if err := r.apiServer.Start(ctx); err != nil {
				logger.Error("API server error", "error", err)
				apiErrChan <- err
			}
		}()
	}

	// 3. Wait for shutdown signal or server error
	var shutdownErr error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received", "reason", ctx.Err())
		shutdownErr = ctx.Err()

	case err := <-apiErrChan:
		logger.Error("API server failed - initiating shutdown", "error", err)
		shutdownErr = fmt.Errorf("API server error: %w", err)
	}

	// 4. Graceful shutdown
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

	// Flush any pending metadata writes
	if r.metadataService != nil {
		logger.Info("Flushing pending metadata writes")
		flushed, err := r.metadataService.FlushAllPendingWritesForShutdown(10 * time.Second)
		if err != nil {
			logger.Warn("Error flushing pending writes", "error", err, "flushed", flushed)
		} else if flushed > 0 {
			logger.Info("Flushed pending metadata writes", "count", flushed)
		}
	}

	// Close metadata stores
	logger.Info("Closing metadata stores")
	r.CloseMetadataStores()

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

// ============================================================================
// Identity Mapping
// ============================================================================

// ApplyIdentityMapping applies share-level identity mapping rules.
func (r *Runtime) ApplyIdentityMapping(shareName string, identity *metadata.Identity) (*metadata.Identity, error) {
	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
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
	switch share.Squash {
	case "", models.SquashNone, models.SquashRootToAdmin:
		// No mapping

	case models.SquashRootToGuest:
		if *identity.UID == 0 {
			applyAnonymousIdentity(effective, share.AnonymousUID, share.AnonymousGID)
		}

	case models.SquashAllToAdmin:
		applyRootIdentity(effective)

	case models.SquashAllToGuest:
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

// ============================================================================
// Mount Tracking (Unified)
// ============================================================================

// Mounts returns the unified mount tracker for cross-protocol mount management.
func (r *Runtime) Mounts() *MountTracker {
	return r.mountTracker
}

// RecordMount registers that a client has mounted a share.
func (r *Runtime) RecordMount(clientAddr, shareName string, mountTime int64) {
	r.mountTracker.Record(clientAddr, "nfs", shareName, mountTime)
}

// RemoveMount removes a mount record for the given client.
func (r *Runtime) RemoveMount(clientAddr string) bool {
	return r.mountTracker.RemoveByClient(clientAddr)
}

// RemoveAllMounts removes all mount records.
func (r *Runtime) RemoveAllMounts() int {
	return r.mountTracker.RemoveAll()
}

// ListMounts returns all active mount records.
func (r *Runtime) ListMounts() []*LegacyMountInfo {
	unified := r.mountTracker.List()
	mounts := make([]*LegacyMountInfo, 0, len(unified))
	for _, mount := range unified {
		var mountTime int64
		if ts, ok := mount.AdapterData.(int64); ok {
			mountTime = ts
		} else {
			mountTime = mount.MountedAt.Unix()
		}
		mounts = append(mounts, &LegacyMountInfo{
			ClientAddr: mount.ClientAddr,
			ShareName:  mount.ShareName,
			MountTime:  mountTime,
		})
	}
	return mounts
}

// ============================================================================
// Service Access (Cross-cutting)
// ============================================================================

// Store returns the persistent configuration store.
func (r *Runtime) Store() store.Store {
	return r.store
}

// GetMetadataService returns the MetadataService for high-level operations.
func (r *Runtime) GetMetadataService() *metadata.MetadataService {
	return r.metadataService
}

// GetPayloadService returns the PayloadService for high-level content operations.
func (r *Runtime) GetPayloadService() *payload.PayloadService {
	return r.payloadService
}

// SetPayloadService sets the PayloadService for content operations.
func (r *Runtime) SetPayloadService(ps *payload.PayloadService) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloadService = ps
}

// SetCacheConfig stores cache configuration for lazy initialization.
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

// ============================================================================
// Settings Access (Hot-Reload)
// ============================================================================

// GetSettingsWatcher returns the settings watcher for direct access.
func (r *Runtime) GetSettingsWatcher() *SettingsWatcher {
	return r.settingsWatcher
}

// GetNFSSettings returns the current NFS adapter settings from the watcher cache.
func (r *Runtime) GetNFSSettings() *models.NFSAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetNFSSettings()
}

// GetSMBSettings returns the current SMB adapter settings from the watcher cache.
func (r *Runtime) GetSMBSettings() *models.SMBAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetSMBSettings()
}

// ============================================================================
// Adapter Providers (Generic)
// ============================================================================

// SetAdapterProvider stores an adapter-specific provider for REST API access.
func (r *Runtime) SetAdapterProvider(key string, p any) {
	r.adapterProvidersMu.Lock()
	defer r.adapterProvidersMu.Unlock()
	r.adapterProviders[key] = p
}

// GetAdapterProvider returns an adapter-specific provider for REST API access.
func (r *Runtime) GetAdapterProvider(key string) any {
	r.adapterProvidersMu.RLock()
	defer r.adapterProvidersMu.RUnlock()
	return r.adapterProviders[key]
}

// SetNFSClientProvider stores the NFS state manager reference for REST API access.
// Deprecated: Use SetAdapterProvider("nfs", p) instead.
func (r *Runtime) SetNFSClientProvider(p any) {
	r.SetAdapterProvider("nfs", p)
}

// NFSClientProvider returns the NFS state manager reference for REST API access.
// Deprecated: Use GetAdapterProvider("nfs") instead.
func (r *Runtime) NFSClientProvider() any {
	return r.GetAdapterProvider("nfs")
}
