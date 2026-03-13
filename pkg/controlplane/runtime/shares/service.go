package shares

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	localmemory "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
	blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Share represents the runtime state of a configured share.
type Share struct {
	Name          string
	MetadataStore string
	RootHandle    metadata.FileHandle
	ReadOnly      bool

	// DefaultPermission for users without explicit permission: "none", "read", "read-write", "admin".
	DefaultPermission string

	// Identity mapping (Synology-style squash modes)
	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	// SMB3 encryption: when true, TREE_CONNECT returns SMB2_SHAREFLAG_ENCRYPT_DATA.
	EncryptData bool

	// NFS-specific options
	DisableReaddirplus bool

	// Security policy
	AllowAuthSys      bool
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string

	// Retention policy for local cache blocks.
	RetentionPolicy blockstore.RetentionPolicy
	RetentionTTL    time.Duration

	// BlockStore is the per-share block store orchestrator.
	// Nil only for metadata-only shares (unlikely in practice).
	BlockStore *engine.BlockStore

	// remoteConfigID tracks which remote store config this share uses (for ref counting).
	remoteConfigID string
}

// ShareConfig contains all configuration needed to create a share.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ReadOnly      bool

	DefaultPermission string

	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	EncryptData bool

	RootAttr *metadata.FileAttr

	DisableReaddirplus bool

	AllowAuthSys      bool
	AllowAuthSysSet   bool // true when AllowAuthSys was explicitly set (distinguishes false from unset)
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string

	// Retention policy for local cache blocks.
	RetentionPolicy blockstore.RetentionPolicy
	RetentionTTL    time.Duration

	// Block store config IDs resolved from the DB share model.
	LocalBlockStoreID  string // Required: references a local BlockStoreConfig
	RemoteBlockStoreID string // Optional: references a remote BlockStoreConfig (empty = local-only)
}

// LegacyMountInfo is the legacy NFS mount record format.
type LegacyMountInfo struct {
	ClientAddr string
	ShareName  string
	MountTime  int64
}

// MetadataStoreProvider looks up metadata stores by name.
type MetadataStoreProvider interface {
	GetMetadataStore(name string) (metadata.MetadataStore, error)
}

// MetadataServiceRegistrar registers metadata stores for shares.
type MetadataServiceRegistrar interface {
	RegisterStoreForShare(shareName string, store metadata.MetadataStore) error
}

// BlockStoreConfigProvider resolves block store configurations from the control plane DB.
type BlockStoreConfigProvider interface {
	GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
}

// LocalStoreDefaults holds default sizing for per-share local stores.
type LocalStoreDefaults struct {
	MaxSize   uint64 // Maximum local store size per share (0 = unlimited)
	MaxMemory int64  // Memory budget for dirty buffers per share (0 = 256MB)

	// ReadCacheBytes is the per-share L1 read cache budget in bytes (0 = disabled).
	ReadCacheBytes int64
}

// SyncerDefaults holds default syncer configuration applied to all shares.
type SyncerDefaults struct {
	ParallelUploads    int
	ParallelDownloads  int
	PrefetchBlocks     int
	SmallFileThreshold int64
	UploadInterval     time.Duration
	UploadDelay        time.Duration

	// PrefetchWorkers is the number of L1 prefetch workers per share (0 = disabled).
	PrefetchWorkers int
}

// sharedRemote holds a reference-counted remote store shared across shares.
type sharedRemote struct {
	store    remote.RemoteStore
	refCount int
	configID string
}

// nonClosingRemote wraps a remote.RemoteStore and makes Close() a no-op.
// This prevents engine.BlockStore.Close() from closing the shared remote;
// the shares.Service.releaseRemoteStore handles actual closing via ref counting.
type nonClosingRemote struct {
	remote.RemoteStore
}

func (n *nonClosingRemote) Close() error { return nil }

// Service manages share registration, lookup, and configuration.
type Service struct {
	mu              sync.RWMutex
	registry        map[string]*Share
	remoteStores    map[string]*sharedRemote // configID -> shared remote
	nextCallbackID  int
	changeCallbacks map[int]func(shares []string)
}

func New() *Service {
	return &Service{
		registry:        make(map[string]*Share),
		remoteStores:    make(map[string]*sharedRemote),
		changeCallbacks: make(map[int]func(shares []string)),
	}
}

// modeLabel returns a human-readable label for logging based on whether a remote store is configured.
func modeLabel(hasRemote bool) string {
	if hasRemote {
		return "remote-backed"
	}
	return "local-only"
}

// sanitizeShareName converts a share name to a filesystem-safe directory name.
// Uses URL path-escaping to guarantee an injective mapping (no two distinct
// share names can produce the same directory name).
func sanitizeShareName(name string) string {
	name = strings.TrimPrefix(name, "/")
	return url.PathEscape(name)
}

// buildSyncerConfigFromDefaults merges SyncerDefaults into a blocksync.Config.
func buildSyncerConfigFromDefaults(defaults *SyncerDefaults) blocksync.Config {
	cfg := blocksync.DefaultConfig()
	if defaults == nil {
		return cfg
	}
	if defaults.ParallelUploads > 0 {
		cfg.ParallelUploads = defaults.ParallelUploads
	}
	if defaults.ParallelDownloads > 0 {
		cfg.ParallelDownloads = defaults.ParallelDownloads
	}
	if defaults.PrefetchBlocks > 0 {
		cfg.PrefetchBlocks = defaults.PrefetchBlocks
	}
	if defaults.SmallFileThreshold != 0 {
		cfg.SmallFileThreshold = defaults.SmallFileThreshold
	}
	if defaults.UploadInterval > 0 {
		cfg.UploadInterval = defaults.UploadInterval
	}
	if defaults.UploadDelay > 0 {
		cfg.UploadDelay = defaults.UploadDelay
	}
	return cfg
}

func (s *Service) AddShare(
	ctx context.Context,
	config *ShareConfig,
	storeProvider MetadataStoreProvider,
	metadataSvc MetadataServiceRegistrar,
	blockStoreProvider BlockStoreConfigProvider,
	localStoreDefaults *LocalStoreDefaults,
	syncerDefaults *SyncerDefaults,
) error {
	if config.Name == "" {
		return errors.New("cannot add share with empty name")
	}

	if config.LocalBlockStoreID != "" && blockStoreProvider == nil {
		return fmt.Errorf("block store provider is required when LocalBlockStoreID is set for share %q", config.Name)
	}

	if metadataSvc == nil {
		return fmt.Errorf("metadata service registrar is required for share %q", config.Name)
	}

	// Phase 1: Build share struct (resolves metadata store, creates root dir).
	// Does NOT insert into registry yet -- share is invisible to handlers.
	share, metadataStore, err := s.prepareShare(ctx, config, storeProvider)
	if err != nil {
		return err
	}

	// Phase 2: Create per-share BlockStore if local block store config is provided.
	if config.LocalBlockStoreID != "" {
		if err := s.createBlockStoreForShare(ctx, share, config, blockStoreProvider, metadataStore, localStoreDefaults, syncerDefaults); err != nil {
			return fmt.Errorf("failed to create block store for share %q: %w", config.Name, err)
		}
	}

	// cleanupShare releases resources for a share that failed to fully initialize.
	cleanupShare := func() {
		if share.BlockStore != nil {
			_ = share.BlockStore.Close()
		}
		if share.remoteConfigID != "" {
			s.releaseRemoteStore(share.remoteConfigID)
		}
	}

	// Phase 3: Register metadata store.
	if err := metadataSvc.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		cleanupShare()
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	// Phase 4: Insert fully-initialized share into registry.
	// Only now is the share visible to protocol handlers.
	s.mu.Lock()
	if _, exists := s.registry[config.Name]; exists {
		s.mu.Unlock()
		cleanupShare()
		return fmt.Errorf("share %q already exists", config.Name)
	}
	s.registry[config.Name] = share
	s.mu.Unlock()

	s.notifyShareChange()

	return nil
}

// prepareShare validates config, resolves the metadata store, and creates the
// root directory. Returns the built Share (not yet in the registry) and the
// metadata store. The caller (AddShare) is responsible for inserting the share
// into the registry after all initialization (including BlockStore) succeeds.
func (s *Service) prepareShare(
	ctx context.Context,
	config *ShareConfig,
	storeProvider MetadataStoreProvider,
) (*Share, metadata.MetadataStore, error) {
	// Early duplicate check (optimistic -- AddShare rechecks under write lock).
	s.mu.RLock()
	if _, exists := s.registry[config.Name]; exists {
		s.mu.RUnlock()
		return nil, nil, fmt.Errorf("share %q already exists", config.Name)
	}
	s.mu.RUnlock()

	if storeProvider == nil {
		return nil, nil, errors.New("metadata store provider not initialized")
	}

	metadataStore, err := storeProvider.GetMetadataStore(config.MetadataStore)
	if err != nil {
		return nil, nil, err
	}

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

	rootFile, err := metadataStore.CreateRootDirectory(ctx, config.Name, rootAttr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create root directory: %w", err)
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode root handle: %w", err)
	}

	allowAuthSys := config.AllowAuthSys
	if !config.AllowAuthSysSet && !allowAuthSys {
		allowAuthSys = true
	}

	share := &Share{
		Name:               config.Name,
		MetadataStore:      config.MetadataStore,
		RootHandle:         rootHandle,
		ReadOnly:           config.ReadOnly,
		EncryptData:        config.EncryptData,
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
		RetentionPolicy:    config.RetentionPolicy,
		RetentionTTL:       config.RetentionTTL,
	}

	return share, metadataStore, nil
}

// createBlockStoreForShare creates and starts a per-share BlockStore.
func (s *Service) createBlockStoreForShare(
	ctx context.Context,
	share *Share,
	config *ShareConfig,
	blockStoreProvider BlockStoreConfigProvider,
	fileBlockStore blockstore.FileBlockStore,
	localStoreDefaults *LocalStoreDefaults,
	syncerDefaults *SyncerDefaults,
) error {
	// Resolve local block store config from DB.
	localCfg, err := blockStoreProvider.GetBlockStoreByID(ctx, config.LocalBlockStoreID)
	if err != nil {
		return fmt.Errorf("failed to resolve local block store config %q: %w", config.LocalBlockStoreID, err)
	}
	if localCfg.Kind != models.BlockStoreKindLocal {
		return fmt.Errorf("block store config %q has kind %q, expected %q", config.LocalBlockStoreID, localCfg.Kind, models.BlockStoreKindLocal)
	}

	localStore, err := CreateLocalStoreFromConfig(ctx, localCfg.Type, localCfg, config.Name, localStoreDefaults, fileBlockStore)
	if err != nil {
		return fmt.Errorf("failed to create local store: %w", err)
	}

	var remoteStore remote.RemoteStore
	var remoteConfigID string
	if config.RemoteBlockStoreID != "" {
		remoteStore, remoteConfigID, err = s.acquireRemoteStore(ctx, config.RemoteBlockStoreID, blockStoreProvider)
		if err != nil {
			_ = localStore.Close()
			return fmt.Errorf("failed to create remote store: %w", err)
		}
	}

	// Eviction requires a remote store (so evicted blocks can be re-fetched) and
	// must not be pin mode (pin keeps blocks cached indefinitely).
	localStore.SetEvictionEnabled(remoteStore != nil && config.RetentionPolicy != blockstore.RetentionPin)
	localStore.SetSkipFsync(remoteStore != nil)
	localStore.SetRetentionPolicy(config.RetentionPolicy, config.RetentionTTL)

	syncerCfg := buildSyncerConfigFromDefaults(syncerDefaults)

	// Wrap shared remote in nonClosingRemote so engine.Close() doesn't close it;
	// releaseRemoteStore handles actual closing via ref counting.
	var engineRemote remote.RemoteStore
	if remoteStore != nil {
		engineRemote = &nonClosingRemote{remoteStore}
	}

	syncer := blocksync.New(localStore, engineRemote, fileBlockStore, syncerCfg)

	cleanup := func() {
		_ = syncer.Close()
		_ = localStore.Close()
		if remoteConfigID != "" {
			s.releaseRemoteStore(remoteConfigID)
		}
	}

	engineCfg := engine.Config{
		Local:  localStore,
		Remote: engineRemote,
		Syncer: syncer,
	}
	if localStoreDefaults != nil {
		engineCfg.ReadCacheBytes = localStoreDefaults.ReadCacheBytes
	}
	if syncerDefaults != nil {
		engineCfg.PrefetchWorkers = syncerDefaults.PrefetchWorkers
	}

	bs, err := engine.New(engineCfg)
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to create BlockStore: %w", err)
	}

	if err := bs.Start(ctx); err != nil {
		cleanup()
		return fmt.Errorf("failed to start BlockStore: %w", err)
	}

	// Safe without lock: share is not yet in the registry.
	share.BlockStore = bs
	share.remoteConfigID = remoteConfigID

	logger.Info("Per-share BlockStore initialized",
		"share", config.Name,
		"mode", modeLabel(remoteStore != nil),
		"local_type", localCfg.Type,
		"retention", config.RetentionPolicy,
		"retention_ttl", config.RetentionTTL)

	return nil
}

// acquireRemoteStore returns a shared remote store, creating it if needed.
// Uses double-checked locking to avoid holding s.mu during potentially slow
// network/DB I/O (config resolution, S3 client initialization).
// Returns the store, its config ID, and any error.
func (s *Service) acquireRemoteStore(ctx context.Context, configID string, provider BlockStoreConfigProvider) (remote.RemoteStore, string, error) {
	s.mu.Lock()
	if sr, ok := s.remoteStores[configID]; ok {
		sr.refCount++
		s.mu.Unlock()
		return sr.store, configID, nil
	}
	s.mu.Unlock()

	// Resolve config and create store without holding the lock.
	remoteCfg, err := provider.GetBlockStoreByID(ctx, configID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve remote block store config %q: %w", configID, err)
	}
	if remoteCfg.Kind != models.BlockStoreKindRemote {
		return nil, "", fmt.Errorf("block store config %q has kind %q, expected %q", configID, remoteCfg.Kind, models.BlockStoreKindRemote)
	}

	newStore, err := CreateRemoteStoreFromConfig(ctx, remoteCfg.Type, remoteCfg)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create remote store: %w", err)
	}

	// Double-check: another goroutine may have created the store concurrently.
	s.mu.Lock()
	if sr, ok := s.remoteStores[configID]; ok {
		sr.refCount++
		s.mu.Unlock()
		_ = newStore.Close()
		return sr.store, configID, nil
	}

	s.remoteStores[configID] = &sharedRemote{
		store:    newStore,
		refCount: 1,
		configID: configID,
	}
	s.mu.Unlock()

	logger.Info("Created shared remote store", "config_id", configID, "type", remoteCfg.Type)
	return newStore, configID, nil
}

// releaseRemoteStore decrements the reference count and closes the remote store if no longer used.
// Close happens outside the lock to avoid blocking share operations during network I/O.
func (s *Service) releaseRemoteStore(configID string) {
	var storeToClose remote.RemoteStore

	s.mu.Lock()
	sr, ok := s.remoteStores[configID]
	if !ok {
		s.mu.Unlock()
		return
	}
	sr.refCount--
	if sr.refCount <= 0 {
		storeToClose = sr.store
		delete(s.remoteStores, configID)
	}
	s.mu.Unlock()

	if storeToClose != nil {
		_ = storeToClose.Close()
		logger.Info("Closed shared remote store", "config_id", configID)
	}
}

// RemoveShare removes a share from the registry and closes its BlockStore.
// Does not close the underlying metadata store.
func (s *Service) RemoveShare(name string) error {
	s.mu.Lock()
	share, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q not found", name)
	}
	bs := share.BlockStore
	remoteConfigID := share.remoteConfigID
	delete(s.registry, name)
	s.mu.Unlock()

	if bs != nil {
		if err := bs.Close(); err != nil {
			logger.Warn("Failed to close BlockStore for share", "share", name, "error", err)
		}
	}

	if remoteConfigID != "" {
		s.releaseRemoteStore(remoteConfigID)
	}

	s.notifyShareChange()

	return nil
}

func (s *Service) UpdateShare(name string, readOnly *bool, defaultPermission *string, retentionPolicy *blockstore.RetentionPolicy, retentionTTL *time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	share, exists := s.registry[name]
	if !exists {
		return fmt.Errorf("share %q not found", name)
	}

	if readOnly != nil {
		share.ReadOnly = *readOnly
	}
	if defaultPermission != nil {
		share.DefaultPermission = *defaultPermission
	}
	if retentionPolicy != nil {
		share.RetentionPolicy = *retentionPolicy
	}
	if retentionTTL != nil {
		share.RetentionTTL = *retentionTTL
	}

	// Propagate retention policy changes to the BlockStore at runtime.
	// The policy applies lazily on the next eviction cycle.
	if (retentionPolicy != nil || retentionTTL != nil) && share.BlockStore != nil {
		share.BlockStore.SetRetentionPolicy(share.RetentionPolicy, share.RetentionTTL)

		// Pin mode disables eviction; switching away from pin re-enables it
		// (unless the share is local-only, in which case eviction stays disabled).
		if share.RetentionPolicy == blockstore.RetentionPin {
			share.BlockStore.SetEvictionEnabled(false)
		} else if share.BlockStore.HasRemoteStore() {
			share.BlockStore.SetEvictionEnabled(true)
		}
	}

	return nil
}

func (s *Service) GetShare(name string) (*Share, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	return share, nil
}

func (s *Service) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return share.RootHandle, nil
}

func (s *Service) ListShares() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.registry))
	for name := range s.registry {
		names = append(names, name)
	}
	return names
}

func (s *Service) ShareExists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.registry[name]
	return exists
}

// OnShareChange registers a callback that is invoked whenever shares are added
// or removed. It returns an unsubscribe function that removes the callback.
// Callers should call the returned function when they no longer need
// notifications (e.g., in their Stop method) to prevent stale callbacks from
// accumulating across adapter restarts.
func (s *Service) OnShareChange(callback func(shares []string)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextCallbackID
	s.nextCallbackID++
	s.changeCallbacks[id] = callback
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.changeCallbacks, id)
	}
}

// notifyShareChange must NOT be called while holding s.mu.
func (s *Service) notifyShareChange() {
	s.mu.RLock()
	callbacks := make([]func(shares []string), 0, len(s.changeCallbacks))
	for _, cb := range s.changeCallbacks {
		callbacks = append(callbacks, cb)
	}
	shareNames := make([]string, 0, len(s.registry))
	for name := range s.registry {
		shareNames = append(shareNames, name)
	}
	s.mu.RUnlock()

	for _, cb := range callbacks {
		cb(shareNames)
	}
}

func (s *Service) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return "", fmt.Errorf("failed to decode file handle: %w", err)
	}

	s.mu.RLock()
	_, exists := s.registry[shareName]
	s.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("share %q not found in runtime", shareName)
	}

	return shareName, nil
}

func (s *Service) CountShares() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registry)
}

// GetBlockStoreForHandle decodes a file handle and resolves the per-share
// BlockStore in a single mutex acquisition, avoiding the two-RLock overhead of
// calling GetShareNameForHandle followed by GetBlockStoreForShare separately.
func (s *Service) GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("failed to decode file handle: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	if share.BlockStore == nil {
		return nil, fmt.Errorf("share %q has no block store configured", shareName)
	}
	return share.BlockStore, nil
}

// GetBlockStoreForShare returns the BlockStore for a named share.
func (s *Service) GetBlockStoreForShare(name string) (*engine.BlockStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	if share.BlockStore == nil {
		return nil, fmt.Errorf("share %q has no block store configured", name)
	}
	return share.BlockStore, nil
}

// DrainAllBlockStores drains all pending uploads across all per-share BlockStores.
func (s *Service) DrainAllBlockStores(ctx context.Context) error {
	s.mu.RLock()
	blockStores := make([]*engine.BlockStore, 0, len(s.registry))
	for _, share := range s.registry {
		if share.BlockStore != nil {
			blockStores = append(blockStores, share.BlockStore)
		}
	}
	s.mu.RUnlock()

	var errs []error
	for _, bs := range blockStores {
		if err := bs.DrainAllUploads(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ShareCacheStats holds cache statistics for a single share.
type ShareCacheStats struct {
	ShareName string            `json:"share_name"`
	Stats     engine.CacheStats `json:"stats"`
}

// CacheStatsResponse holds aggregated and per-share cache statistics.
type CacheStatsResponse struct {
	Totals   engine.CacheStats `json:"totals"`
	PerShare []ShareCacheStats `json:"per_share,omitempty"`
}

// EvictOptions controls which cache tiers to evict.
type EvictOptions struct {
	L1Only    bool `json:"l1_only"`
	LocalOnly bool `json:"local_only"`
}

// EvictResult holds the result of a cache eviction operation.
type EvictResult struct {
	L1EntriesCleared  int   `json:"l1_entries_cleared"`
	LocalFilesEvicted int   `json:"local_files_evicted"`
	BytesFreed        int64 `json:"bytes_freed"`
}

// GetCacheStats returns cache statistics, optionally filtered by share name.
// If shareName is empty, returns aggregated stats across all shares with per-share breakdown.
func (s *Service) GetCacheStats(shareName string) (*CacheStatsResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if shareName != "" {
		share, exists := s.registry[shareName]
		if !exists {
			return nil, fmt.Errorf("share %q not found", shareName)
		}
		if share.BlockStore == nil {
			return nil, fmt.Errorf("share %q has no block store configured", shareName)
		}
		stats := share.BlockStore.GetCacheStats()
		return &CacheStatsResponse{
			Totals:   stats,
			PerShare: []ShareCacheStats{{ShareName: shareName, Stats: stats}},
		}, nil
	}

	var totals engine.CacheStats
	var perShare []ShareCacheStats

	for name, share := range s.registry {
		if share.BlockStore == nil {
			continue
		}
		stats := share.BlockStore.GetCacheStats()
		perShare = append(perShare, ShareCacheStats{ShareName: name, Stats: stats})
		addCacheStats(&totals, stats)
	}

	return &CacheStatsResponse{
		Totals:   totals,
		PerShare: perShare,
	}, nil
}

// addCacheStats accumulates src into dst (field-by-field summation).
func addCacheStats(dst *engine.CacheStats, src engine.CacheStats) {
	dst.FileCount += src.FileCount
	dst.BlocksDirty += src.BlocksDirty
	dst.BlocksLocal += src.BlocksLocal
	dst.BlocksRemote += src.BlocksRemote
	dst.BlocksTotal += src.BlocksTotal
	dst.LocalDiskUsed += src.LocalDiskUsed
	dst.LocalDiskMax += src.LocalDiskMax
	dst.LocalMemUsed += src.LocalMemUsed
	dst.LocalMemMax += src.LocalMemMax
	dst.L1Entries += src.L1Entries
	dst.L1CurBytes += src.L1CurBytes
	dst.L1MaxBytes += src.L1MaxBytes
	dst.PendingSyncs += src.PendingSyncs
	dst.PendingUploads += src.PendingUploads
	dst.CompletedSyncs += src.CompletedSyncs
	dst.FailedSyncs += src.FailedSyncs
	if src.HasRemote {
		dst.HasRemote = true
	}
}

// EvictCache evicts cache data for the given share (or all shares if shareName is empty).
// Returns an error if trying to evict local blocks without a remote store (safety check).
func (s *Service) EvictCache(ctx context.Context, shareName string, opts EvictOptions) (*EvictResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var targets []*Share
	if shareName != "" {
		share, exists := s.registry[shareName]
		if !exists {
			return nil, fmt.Errorf("share %q not found", shareName)
		}
		if share.BlockStore == nil {
			return nil, fmt.Errorf("share %q has no block store configured", shareName)
		}
		targets = []*Share{share}
	} else {
		for _, share := range s.registry {
			if share.BlockStore != nil {
				targets = append(targets, share)
			}
		}
	}

	var result EvictResult

	for _, share := range targets {
		bs := share.BlockStore

		if !opts.L1Only && !bs.HasRemoteStore() {
			return nil, fmt.Errorf("cannot evict local blocks for share %q: no remote store configured (data would be lost)", share.Name)
		}

		if !opts.LocalOnly {
			result.L1EntriesCleared += bs.EvictL1Cache()
		}

		if !opts.L1Only {
			beforeDisk := bs.LocalStats().DiskUsed

			files := bs.ListFiles()
			for _, payloadID := range files {
				_ = bs.EvictLocal(ctx, payloadID)
				result.LocalFilesEvicted++
			}

			result.BytesFreed += beforeDisk - bs.LocalStats().DiskUsed
		}
	}

	return &result, nil
}

// CreateLocalStoreFromConfig creates a local store instance from a block store config.
func CreateLocalStoreFromConfig(
	ctx context.Context,
	storeType string,
	cfg interface {
		GetConfig() (map[string]any, error)
	},
	shareName string,
	defaults *LocalStoreDefaults,
	fileBlockStore blockstore.FileBlockStore,
) (local.LocalStore, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	var maxDisk int64
	var maxMemory int64
	if defaults != nil {
		maxDisk = int64(defaults.MaxSize)
		maxMemory = defaults.MaxMemory
	}

	switch storeType {
	case "fs":
		basePath, ok := config["path"].(string)
		if !ok || basePath == "" {
			return nil, errors.New("fs local store requires path in config")
		}
		sanitized := sanitizeShareName(shareName)
		cacheDir := filepath.Join(basePath, "shares", sanitized, "blocks")
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create cache directory: %w", err)
		}
		return fs.New(cacheDir, maxDisk, maxMemory, fileBlockStore)

	case "memory":
		return localmemory.New(), nil

	default:
		return nil, fmt.Errorf("unsupported local store type: %s", storeType)
	}
}

// CreateRemoteStoreFromConfig creates a remote store from type and dynamic config.
func CreateRemoteStoreFromConfig(ctx context.Context, storeType string, cfg interface {
	GetConfig() (map[string]any, error)
}) (remote.RemoteStore, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	switch storeType {
	case "memory":
		return remotememory.New(), nil

	case "filesystem":
		return nil, errors.New("remote store type 'filesystem' removed in v4.0 -- use 'memory' or 's3'")

	case "s3":
		bucket, ok := config["bucket"].(string)
		if !ok || bucket == "" {
			return nil, errors.New("s3 remote store requires bucket")
		}

		region := "us-east-1"
		if r, ok := config["region"].(string); ok && r != "" {
			region = r
		}

		endpoint, _ := config["endpoint"].(string)
		prefix, _ := config["prefix"].(string)
		forcePathStyle, _ := config["force_path_style"].(bool)

		return remotes3.NewFromConfig(ctx, remotes3.Config{
			Bucket:         bucket,
			Region:         region,
			Endpoint:       endpoint,
			KeyPrefix:      prefix,
			ForcePathStyle: forcePathStyle,
		})

	default:
		return nil, fmt.Errorf("unsupported remote store type: %s", storeType)
	}
}
