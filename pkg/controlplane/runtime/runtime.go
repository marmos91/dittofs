package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/adapters"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/identity"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/lifecycle"
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

// AuxiliaryServer is a type alias for lifecycle.AuxiliaryServer so that
// existing consumers can continue using runtime.AuxiliaryServer.
type AuxiliaryServer = lifecycle.AuxiliaryServer

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

// shareIdentityProvider implements identity.ShareProvider for the Runtime.
type shareIdentityProvider struct {
	sharesSvc *shares.Service
}

func (p *shareIdentityProvider) GetShareIdentityInfo(shareName string) (*identity.ShareInfo, error) {
	share, err := p.sharesSvc.GetShare(shareName)
	if err != nil {
		return nil, err
	}
	return &identity.ShareInfo{
		Squash:       share.Squash,
		AnonymousUID: share.AnonymousUID,
		AnonymousGID: share.AnonymousGID,
	}, nil
}

// Runtime manages all runtime state for shares and protocol adapters.
// It provides thread-safe registration and lookup of all server resources.
//
// The Runtime composes 6 focused sub-services:
//   - adapters: Protocol adapter lifecycle management
//   - stores: Metadata store registry
//   - shares: Share registration and configuration
//   - mounts: Unified mount tracking (via MountTracker)
//   - lifecycle: Server startup and shutdown orchestration
//   - identity: Share-level identity mapping
//
// Content Model:
// All content operations go through the PayloadService which includes
// caching and transfer management. The PayloadService is created lazily
// when the first share that uses a payload store is created.
type Runtime struct {
	mu    sync.RWMutex
	store store.Store // Persistent configuration store

	// High-level services
	metadataService *metadata.MetadataService
	payloadService  *payload.PayloadService
	cacheInstance   *cache.Cache

	// Sub-services
	adaptersSvc  *adapters.Service
	storesSvc    *stores.Service
	sharesSvc    *shares.Service
	lifecycleSvc *lifecycle.Service
	identitySvc  *identity.Service

	// Unified mount tracking across all protocols (NFS, SMB)
	mountTracker *MountTracker

	// Cache configuration (set at startup, used for lazy PayloadService creation)
	cacheConfig *CacheConfig

	// Settings hot-reload
	settingsWatcher *SettingsWatcher

	// Adapter data providers (stored as any to avoid import cycles with internal/adapter)
	adapterProviders   map[string]any
	adapterProvidersMu sync.RWMutex
}

// New creates a new Runtime with the given persistent store.
func New(s store.Store) *Runtime {
	rt := &Runtime{
		store:            s,
		metadataService:  metadata.New(),
		mountTracker:     NewMountTracker(),
		adapterProviders: make(map[string]any),
		storesSvc:        stores.New(),
		sharesSvc:        shares.New(),
		lifecycleSvc:     lifecycle.New(DefaultShutdownTimeout),
		identitySvc:      identity.New(),
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
func (r *Runtime) SetAdapterFactory(factory AdapterFactory) {
	r.adaptersSvc.SetAdapterFactory(factory)
}

// SetShutdownTimeout sets the maximum time to wait for graceful adapter shutdown.
func (r *Runtime) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	r.adaptersSvc.SetShutdownTimeout(d)
	r.lifecycleSvc.SetShutdownTimeout(d)
}

// CreateAdapter saves the adapter config to store AND starts it immediately.
func (r *Runtime) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	return r.adaptersSvc.CreateAdapter(ctx, cfg)
}

// DeleteAdapter stops the running adapter AND removes from store.
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

// ListRunningAdapters returns currently running adapter types.
func (r *Runtime) ListRunningAdapters() []string {
	return r.adaptersSvc.ListRunningAdapters()
}

// IsAdapterRunning checks if an adapter is currently running.
func (r *Runtime) IsAdapterRunning(adapterType string) bool {
	return r.adaptersSvc.IsAdapterRunning(adapterType)
}

// AddAdapter adds and starts a pre-created adapter directly (testing).
func (r *Runtime) AddAdapter(adapter ProtocolAdapter) error {
	return r.adaptersSvc.AddAdapter(adapter)
}

// ============================================================================
// Metadata Store Management (delegated to stores.Service)
// ============================================================================

// RegisterMetadataStore adds a named metadata store instance.
func (r *Runtime) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	return r.storesSvc.RegisterMetadataStore(name, metaStore)
}

// GetMetadataStore retrieves a metadata store by name.
func (r *Runtime) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	return r.storesSvc.GetMetadataStore(name)
}

// GetMetadataStoreForShare retrieves the metadata store used by a share.
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

// UpdateShare updates a share's configuration.
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

// ShareExists checks if a share exists.
func (r *Runtime) ShareExists(name string) bool {
	return r.sharesSvc.ShareExists(name)
}

// OnShareChange registers a callback for share changes.
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
// Lifecycle (delegated to lifecycle.Service)
// ============================================================================

// SetAPIServer sets the REST API HTTP server.
func (r *Runtime) SetAPIServer(server AuxiliaryServer) {
	r.lifecycleSvc.SetAPIServer(server)
}

// Serve starts all adapters and auxiliary servers, and blocks until shutdown.
func (r *Runtime) Serve(ctx context.Context) error {
	return r.lifecycleSvc.Serve(ctx, r.settingsWatcher, r.adaptersSvc, r.metadataService, r.storesSvc)
}

// ============================================================================
// Identity Mapping (delegated to identity.Service)
// ============================================================================

// ApplyIdentityMapping applies share-level identity mapping rules.
func (r *Runtime) ApplyIdentityMapping(shareName string, ident *metadata.Identity) (*metadata.Identity, error) {
	return r.identitySvc.ApplyIdentityMapping(shareName, ident, &shareIdentityProvider{sharesSvc: r.sharesSvc})
}

// ============================================================================
// Mount Tracking (delegated to MountTracker)
// ============================================================================

// Mounts returns the unified mount tracker.
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

// ListMounts returns all active mount records (legacy format).
func (r *Runtime) ListMounts() []*LegacyMountInfo {
	unified := r.mountTracker.List()
	result := make([]*LegacyMountInfo, 0, len(unified))
	for _, mount := range unified {
		var mountTime int64
		if ts, ok := mount.AdapterData.(int64); ok {
			mountTime = ts
		} else {
			mountTime = mount.MountedAt.Unix()
		}
		result = append(result, &LegacyMountInfo{
			ClientAddr: mount.ClientAddr,
			ShareName:  mount.ShareName,
			MountTime:  mountTime,
		})
	}
	return result
}

// ============================================================================
// Service Access (Cross-cutting)
// ============================================================================

// Store returns the persistent configuration store.
func (r *Runtime) Store() store.Store {
	return r.store
}

// GetMetadataService returns the MetadataService.
func (r *Runtime) GetMetadataService() *metadata.MetadataService {
	return r.metadataService
}

// GetPayloadService returns the PayloadService.
func (r *Runtime) GetPayloadService() *payload.PayloadService {
	return r.payloadService
}

// SetPayloadService sets the PayloadService.
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

// SetCache stores the cache instance.
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

// GetUserStore returns the store as a UserStore.
func (r *Runtime) GetUserStore() models.UserStore {
	return r.store
}

// GetIdentityStore returns the store as an IdentityStore.
func (r *Runtime) GetIdentityStore() models.IdentityStore {
	return r.store
}

// GetBlockService returns the PayloadService (deprecated).
func (r *Runtime) GetBlockService() *payload.PayloadService {
	return r.payloadService
}

// ============================================================================
// Settings Access (Hot-Reload)
// ============================================================================

// GetSettingsWatcher returns the settings watcher.
func (r *Runtime) GetSettingsWatcher() *SettingsWatcher {
	return r.settingsWatcher
}

// GetNFSSettings returns the current NFS adapter settings.
func (r *Runtime) GetNFSSettings() *models.NFSAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetNFSSettings()
}

// GetSMBSettings returns the current SMB adapter settings.
func (r *Runtime) GetSMBSettings() *models.SMBAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetSMBSettings()
}

// ============================================================================
// Adapter Providers (Generic)
// ============================================================================

// SetAdapterProvider stores an adapter-specific provider.
func (r *Runtime) SetAdapterProvider(key string, p any) {
	r.adapterProvidersMu.Lock()
	defer r.adapterProvidersMu.Unlock()
	r.adapterProviders[key] = p
}

// GetAdapterProvider returns an adapter-specific provider.
func (r *Runtime) GetAdapterProvider(key string) any {
	r.adapterProvidersMu.RLock()
	defer r.adapterProvidersMu.RUnlock()
	return r.adapterProviders[key]
}

// SetNFSClientProvider stores the NFS state manager (deprecated).
func (r *Runtime) SetNFSClientProvider(p any) {
	r.SetAdapterProvider("nfs", p)
}

// NFSClientProvider returns the NFS state manager (deprecated).
func (r *Runtime) NFSClientProvider() any {
	return r.GetAdapterProvider("nfs")
}
