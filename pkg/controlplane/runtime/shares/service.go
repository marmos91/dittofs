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
	MaxSize        uint64 // Maximum local store size per share (0 = unlimited)
	MaxPendingSize uint64 // Maximum pending (dirty) data size (0 = default 2GB)
	MaxMemory      int64  // Memory budget for dirty buffers per share (0 = 256MB)
}

// SyncerDefaults holds default syncer configuration applied to all shares.
type SyncerDefaults struct {
	ParallelUploads    int
	ParallelDownloads  int
	PrefetchBlocks     int
	SmallFileThreshold int64
	UploadInterval     time.Duration
	UploadDelay        time.Duration
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
		return fmt.Errorf("cannot add share with empty name")
	}

	if config.LocalBlockStoreID != "" && blockStoreProvider == nil {
		return fmt.Errorf("block store provider is required when LocalBlockStoreID is set for share %q", config.Name)
	}

	share, metadataStore, err := s.registerShare(ctx, config, storeProvider)
	if err != nil {
		return err
	}

	// Create per-share BlockStore if local block store config is provided.
	if config.LocalBlockStoreID != "" {
		if err := s.createBlockStoreForShare(ctx, share, config, blockStoreProvider, metadataStore, localStoreDefaults, syncerDefaults); err != nil {
			// Roll back: remove from registry
			s.mu.Lock()
			delete(s.registry, config.Name)
			s.mu.Unlock()
			return fmt.Errorf("failed to create block store for share %q: %w", config.Name, err)
		}
	}

	// Register metadata store only after BlockStore is successfully created
	// to avoid leaving stale state in MetadataService on failure.
	if err := metadataSvc.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		// Roll back: close BlockStore and remove from registry
		s.mu.Lock()
		if share.BlockStore != nil {
			_ = share.BlockStore.Close()
		}
		if share.remoteConfigID != "" {
			delete(s.registry, config.Name)
			s.mu.Unlock()
			s.releaseRemoteStore(share.remoteConfigID)
		} else {
			delete(s.registry, config.Name)
			s.mu.Unlock()
		}
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	s.notifyShareChange()

	return nil
}

// registerShare validates config, creates the root directory, and registers the
// share in the registry. Metadata service registration is deferred to AddShare
// so that rollback is clean if BlockStore creation fails.
func (s *Service) registerShare(
	ctx context.Context,
	config *ShareConfig,
	storeProvider MetadataStoreProvider,
) (*Share, metadata.MetadataStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.registry[config.Name]; exists {
		return nil, nil, fmt.Errorf("share %q already exists", config.Name)
	}

	if storeProvider == nil {
		return nil, nil, fmt.Errorf("metadata store provider not initialized")
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
	}

	s.registry[config.Name] = share

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

	// Create local store.
	localStore, err := CreateLocalStoreFromConfig(ctx, localCfg.Type, localCfg, config.Name, localStoreDefaults, fileBlockStore)
	if err != nil {
		return fmt.Errorf("failed to create local store: %w", err)
	}

	// Resolve optional remote block store.
	var remoteStore remote.RemoteStore
	var remoteConfigID string
	if config.RemoteBlockStoreID != "" {
		remoteStore, remoteConfigID, err = s.acquireRemoteStore(ctx, config.RemoteBlockStoreID, blockStoreProvider)
		if err != nil {
			return fmt.Errorf("failed to create remote store: %w", err)
		}
	}

	localOnly := remoteStore == nil

	// Configure local store behavior based on remote presence.
	localStore.SetSkipFsync(!localOnly)
	localStore.SetEvictionEnabled(!localOnly)

	// Build syncer config from defaults.
	syncerCfg := buildSyncerConfigFromDefaults(syncerDefaults)

	// Wrap shared remote in nonClosingRemote so engine.Close() doesn't close it.
	// The shares.Service.releaseRemoteStore handles closing via ref counting.
	var engineRemote remote.RemoteStore
	if remoteStore != nil {
		engineRemote = &nonClosingRemote{remoteStore}
	}

	syncer := blocksync.New(localStore, engineRemote, fileBlockStore, syncerCfg)

	// Create BlockStore.
	bs, err := engine.New(engine.Config{
		Local:  localStore,
		Remote: engineRemote,
		Syncer: syncer,
	})
	if err != nil {
		if remoteConfigID != "" {
			s.releaseRemoteStore(remoteConfigID)
		}
		return fmt.Errorf("failed to create BlockStore: %w", err)
	}

	// Start BlockStore (recovery + background goroutines).
	if err := bs.Start(ctx); err != nil {
		if remoteConfigID != "" {
			s.releaseRemoteStore(remoteConfigID)
		}
		return fmt.Errorf("failed to start BlockStore: %w", err)
	}

	// Assign to share.
	s.mu.Lock()
	share.BlockStore = bs
	share.remoteConfigID = remoteConfigID
	s.mu.Unlock()

	mode := "remote-backed"
	if localOnly {
		mode = "local-only"
	}
	logger.Info("Per-share BlockStore initialized",
		"share", config.Name,
		"mode", mode,
		"local_type", localCfg.Type)

	return nil
}

// acquireRemoteStore returns a shared remote store, creating it if needed.
// Returns the store, its config ID, and any error.
func (s *Service) acquireRemoteStore(ctx context.Context, configID string, provider BlockStoreConfigProvider) (remote.RemoteStore, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sr, ok := s.remoteStores[configID]; ok {
		sr.refCount++
		return sr.store, configID, nil
	}

	// Resolve and create.
	remoteCfg, err := provider.GetBlockStoreByID(ctx, configID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve remote block store config %q: %w", configID, err)
	}

	remoteStore, err := CreateRemoteStoreFromConfig(ctx, remoteCfg.Type, remoteCfg)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create remote store: %w", err)
	}

	s.remoteStores[configID] = &sharedRemote{
		store:    remoteStore,
		refCount: 1,
		configID: configID,
	}

	logger.Info("Created shared remote store", "config_id", configID, "type", remoteCfg.Type)
	return remoteStore, configID, nil
}

// releaseRemoteStore decrements the reference count and closes the remote store if no longer used.
func (s *Service) releaseRemoteStore(configID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr, ok := s.remoteStores[configID]
	if !ok {
		return
	}
	sr.refCount--
	if sr.refCount <= 0 {
		_ = sr.store.Close()
		delete(s.remoteStores, configID)
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

	// Close BlockStore outside lock (may block on drain).
	if bs != nil {
		if err := bs.Close(); err != nil {
			logger.Warn("Failed to close BlockStore for share", "share", name, "error", err)
		}
	}

	// Decrement remote store ref count.
	if remoteConfigID != "" {
		s.releaseRemoteStore(remoteConfigID)
	}

	s.notifyShareChange()

	return nil
}

func (s *Service) UpdateShare(name string, readOnly *bool, defaultPermission *string) error {
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
		return "", fmt.Errorf("failed to decode share handle: %w", err)
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
			return nil, fmt.Errorf("fs local store requires path in config")
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
