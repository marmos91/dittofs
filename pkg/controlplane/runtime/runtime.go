package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/adapters"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/identity"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/lifecycle"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// DefaultShutdownTimeout is the default timeout for graceful shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// Type aliases re-exported for backward compatibility.
type (
	ProtocolAdapter = adapters.ProtocolAdapter
	RuntimeSetter   = adapters.RuntimeSetter
	AdapterFactory  = adapters.AdapterFactory
	AuxiliaryServer = lifecycle.AuxiliaryServer
)

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
// It composes sub-services for adapters, stores, shares, mounts,
// lifecycle, and identity mapping.
type Runtime struct {
	mu    sync.RWMutex
	store store.Store

	metadataService *metadata.MetadataService

	adaptersSvc  *adapters.Service
	storesSvc    *stores.Service
	sharesSvc    *shares.Service
	lifecycleSvc *lifecycle.Service
	identitySvc  *identity.Service
	mountTracker *MountTracker

	localStoreDefaults *shares.LocalStoreDefaults
	syncerDefaults     *shares.SyncerDefaults
	settingsWatcher    *SettingsWatcher

	adapterProviders   map[string]any
	adapterProvidersMu sync.RWMutex
}

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

	rt.adaptersSvc = adapters.New(s, DefaultShutdownTimeout)
	rt.adaptersSvc.SetRuntime(rt)

	if s != nil {
		rt.settingsWatcher = NewSettingsWatcher(s, DefaultPollInterval)
	}

	return rt
}

// --- Adapter Management (delegated to adapters.Service) ---

func (r *Runtime) SetAdapterFactory(factory AdapterFactory) {
	r.adaptersSvc.SetAdapterFactory(factory)
}

func (r *Runtime) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	r.adaptersSvc.SetShutdownTimeout(d)
	r.lifecycleSvc.SetShutdownTimeout(d)
}

func (r *Runtime) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	return r.adaptersSvc.CreateAdapter(ctx, cfg)
}

func (r *Runtime) DeleteAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.DeleteAdapter(ctx, adapterType)
}

func (r *Runtime) UpdateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	return r.adaptersSvc.UpdateAdapter(ctx, cfg)
}

func (r *Runtime) EnableAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.EnableAdapter(ctx, adapterType)
}

func (r *Runtime) DisableAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.DisableAdapter(ctx, adapterType)
}

func (r *Runtime) StopAllAdapters() error {
	return r.adaptersSvc.StopAllAdapters()
}

func (r *Runtime) LoadAdaptersFromStore(ctx context.Context) error {
	return r.adaptersSvc.LoadAdaptersFromStore(ctx)
}

func (r *Runtime) ListRunningAdapters() []string {
	return r.adaptersSvc.ListRunningAdapters()
}

func (r *Runtime) IsAdapterRunning(adapterType string) bool {
	return r.adaptersSvc.IsAdapterRunning(adapterType)
}

// AddAdapter directly starts a pre-created adapter (for testing, bypasses store).
func (r *Runtime) AddAdapter(adapter ProtocolAdapter) error {
	return r.adaptersSvc.AddAdapter(adapter)
}

// --- Metadata Store Management (delegated to stores.Service) ---

func (r *Runtime) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	return r.storesSvc.RegisterMetadataStore(name, metaStore)
}

func (r *Runtime) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	return r.storesSvc.GetMetadataStore(name)
}

func (r *Runtime) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return nil, err
	}
	return r.storesSvc.GetMetadataStore(share.MetadataStore)
}

func (r *Runtime) ListMetadataStores() []string {
	return r.storesSvc.ListMetadataStores()
}

func (r *Runtime) CountMetadataStores() int {
	return r.storesSvc.CountMetadataStores()
}

func (r *Runtime) CloseMetadataStores() {
	r.storesSvc.CloseMetadataStores()
}

// --- Share Management (delegated to shares.Service) ---

func (r *Runtime) AddShare(ctx context.Context, config *ShareConfig) error {
	r.mu.RLock()
	localDefaults := r.localStoreDefaults
	syncDefaults := r.syncerDefaults
	r.mu.RUnlock()
	return r.sharesSvc.AddShare(ctx, config, r.storesSvc, r.metadataService, r.store, localDefaults, syncDefaults)
}

func (r *Runtime) RemoveShare(name string) error {
	return r.sharesSvc.RemoveShare(name)
}

func (r *Runtime) UpdateShare(name string, readOnly *bool, defaultPermission *string, retentionPolicy *blockstore.RetentionPolicy, retentionTTL *time.Duration) error {
	return r.sharesSvc.UpdateShare(name, readOnly, defaultPermission, retentionPolicy, retentionTTL)
}

func (r *Runtime) GetShare(name string) (*Share, error) {
	return r.sharesSvc.GetShare(name)
}

func (r *Runtime) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	return r.sharesSvc.GetRootHandle(shareName)
}

func (r *Runtime) ListShares() []string {
	return r.sharesSvc.ListShares()
}

func (r *Runtime) ShareExists(name string) bool {
	return r.sharesSvc.ShareExists(name)
}

func (r *Runtime) OnShareChange(callback func(shares []string)) func() {
	return r.sharesSvc.OnShareChange(callback)
}

func (r *Runtime) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	return r.sharesSvc.GetShareNameForHandle(ctx, handle)
}

func (r *Runtime) CountShares() int {
	return r.sharesSvc.CountShares()
}

// GetBlockStoreForHandle resolves the per-share BlockStore from a file handle.
func (r *Runtime) GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error) {
	return r.sharesSvc.GetBlockStoreForHandle(ctx, handle)
}

// --- Lifecycle (delegated to lifecycle.Service) ---

func (r *Runtime) SetAPIServer(server AuxiliaryServer) {
	r.lifecycleSvc.SetAPIServer(server)
}

func (r *Runtime) Serve(ctx context.Context) error {
	return r.lifecycleSvc.Serve(ctx, r.settingsWatcher, r.adaptersSvc, r.metadataService, r.storesSvc, r.store)
}

// --- Identity Mapping (delegated to identity.Service) ---

func (r *Runtime) ApplyIdentityMapping(shareName string, ident *metadata.Identity) (*metadata.Identity, error) {
	return r.identitySvc.ApplyIdentityMapping(shareName, ident, &shareIdentityProvider{sharesSvc: r.sharesSvc})
}

// --- Mount Tracking (delegated to MountTracker) ---

func (r *Runtime) Mounts() *MountTracker {
	return r.mountTracker
}

func (r *Runtime) RecordMount(clientAddr, shareName string, mountTime int64) {
	r.mountTracker.Record(clientAddr, "nfs", shareName, mountTime)
}

func (r *Runtime) RemoveMount(clientAddr string) bool {
	return r.mountTracker.RemoveByClient(clientAddr)
}

func (r *Runtime) RemoveAllMounts() int {
	return r.mountTracker.RemoveAll()
}

// ListMounts converts unified mount records to the legacy NFS format.
func (r *Runtime) ListMounts() []*LegacyMountInfo {
	unified := r.mountTracker.List()
	result := make([]*LegacyMountInfo, 0, len(unified))
	for _, m := range unified {
		ts, ok := m.AdapterData.(int64)
		if !ok {
			ts = m.MountedAt.Unix()
		}
		result = append(result, &LegacyMountInfo{
			ClientAddr: m.ClientAddr,
			ShareName:  m.ShareName,
			MountTime:  ts,
		})
	}
	return result
}

// --- Service Access ---

func (r *Runtime) Store() store.Store                            { return r.store }
func (r *Runtime) GetMetadataService() *metadata.MetadataService { return r.metadataService }

// SIDMapper returns the machine SID mapper for Windows identity mapping.
// Returns nil if the runtime has not been started yet (Serve not called).
func (r *Runtime) SIDMapper() *sid.SIDMapper { return r.lifecycleSvc.SIDMapper() }

// SetLocalStoreDefaults sets the default sizing for per-share local stores.
func (r *Runtime) SetLocalStoreDefaults(cfg *shares.LocalStoreDefaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.localStoreDefaults = cfg
}

// SetSyncerDefaults sets the default syncer configuration for per-share BlockStores.
func (r *Runtime) SetSyncerDefaults(cfg *shares.SyncerDefaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncerDefaults = cfg
}

// DrainAllUploads waits for all in-flight uploads across all per-share BlockStores to complete.
func (r *Runtime) DrainAllUploads(ctx context.Context) error {
	return r.sharesSvc.DrainAllBlockStores(ctx)
}

// GetCacheStats returns cache statistics, optionally filtered by share name.
func (r *Runtime) GetCacheStats(shareName string) (*shares.CacheStatsResponse, error) {
	return r.sharesSvc.GetCacheStats(shareName)
}

// EvictCache evicts cache data for the given share (or all shares).
func (r *Runtime) EvictCache(ctx context.Context, shareName string, opts shares.EvictOptions) (*shares.EvictResult, error) {
	return r.sharesSvc.EvictCache(ctx, shareName, opts)
}

func (r *Runtime) GetUserStore() models.UserStore         { return r.store }
func (r *Runtime) GetIdentityStore() models.IdentityStore { return r.store }

// --- Settings Access ---

func (r *Runtime) GetSettingsWatcher() *SettingsWatcher { return r.settingsWatcher }

func (r *Runtime) GetNFSSettings() *models.NFSAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetNFSSettings()
}

func (r *Runtime) GetSMBSettings() *models.SMBAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetSMBSettings()
}

// --- Adapter Providers ---

func (r *Runtime) SetAdapterProvider(key string, p any) {
	r.adapterProvidersMu.Lock()
	defer r.adapterProvidersMu.Unlock()
	r.adapterProviders[key] = p
}

func (r *Runtime) GetAdapterProvider(key string) any {
	r.adapterProvidersMu.RLock()
	defer r.adapterProvidersMu.RUnlock()
	return r.adapterProviders[key]
}

// SetNFSClientProvider is deprecated; use SetAdapterProvider("nfs", p).
func (r *Runtime) SetNFSClientProvider(p any) { r.SetAdapterProvider("nfs", p) }

// NFSClientProvider is deprecated; use GetAdapterProvider("nfs").
func (r *Runtime) NFSClientProvider() any { return r.GetAdapterProvider("nfs") }
