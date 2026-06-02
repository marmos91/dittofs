package shares

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/compression"
	"github.com/marmos91/dittofs/pkg/blockstore/encryption"
	"github.com/marmos91/dittofs/pkg/blockstore/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	localmemory "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Share represents the runtime state of a configured share.
type Share struct {
	Name          string
	MetadataStore string
	RootHandle    metadata.FileHandle
	ReadOnly      bool
	// Enabled reflects the DB-row `shares.enabled` flag. Disabled shares
	// reject new MOUNT / TREE_CONNECT and in-flight operations.
	// Default true when populated from DB via AddShare.
	Enabled bool

	// DefaultPermission for users without explicit permission: "none", "read", "read-write", "admin".
	DefaultPermission string

	// Identity mapping (Synology-style squash modes)
	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	// SMB3 encryption: when true, TREE_CONNECT returns SMB2_SHAREFLAG_ENCRYPT_DATA.
	EncryptData bool

	// AclFlagInheritedCanonicalization controls whether the SMB CREATE /
	// SET_INFO Security path canonicalizes the SE_DACL_AUTO_INHERITED control
	// bit per MS-DTYP §2.5.3.4.2 (clearing it when AUTO_INHERIT_REQ is unset).
	// Default true matches Windows behavior; false opts into the Samba
	// extension where the bit survives without AUTO_INHERIT_REQ (refs #514).
	AclFlagInheritedCanonicalization bool

	// AccessBasedEnumeration enables Windows access-based enumeration on the
	// share (SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM per MS-SRVS). When
	// true, TREE_CONNECT sets SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM in
	// ShareFlags (MS-SMB2 §2.2.10) and the SMB QUERY_DIRECTORY handler
	// filters out entries the caller cannot read. Default false (refs #532,
	// #549).
	AccessBasedEnumeration bool

	// ChangeNotifyDisabled rejects SMB2 CHANGE_NOTIFY on this share with
	// STATUS_NOT_IMPLEMENTED. Mirrors Samba `kernel change notify = no`
	// and the smb2.change_notify_disabled torture test.
	ChangeNotifyDisabled bool

	// StreamsDisabled rejects SMB2 CREATE requests that reference an
	// Alternate Data Stream with STATUS_OBJECT_NAME_INVALID. Mirrors the
	// Samba `smbd:streams = no` config and the
	// smb2.create_no_streams.no_stream torture test.
	StreamsDisabled bool

	// ContinuousAvailability advertises SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY
	// in TREE_CONNECT and allows SMB3 persistent durable handles (DH2Q
	// SMB2_DHANDLE_FLAG_PERSISTENT) on this share. See the models.Share field
	// for semantics (refs #739).
	ContinuousAvailability bool

	// TrashEnabled turns on the per-share recycle bin (#190). Default false.
	// Read per-delete via the locked TrashSettingsForShare accessor (NOT off a
	// shared *Share pointer) so it is concurrency-safe and takes effect live.
	TrashEnabled bool
	// TrashRetentionDays auto-empties bin entries older than N days (0 = keep forever).
	TrashRetentionDays int
	// TrashRestrictToAdmin limits empty/force-delete to admins (users may still restore).
	TrashRestrictToAdmin bool
	// TrashMaxBytes caps total bin bytes (0 = unbounded); over-cap evicts oldest.
	TrashMaxBytes int64
	// TrashExcludePatterns are globs that bypass the bin (immediate delete).
	TrashExcludePatterns []string

	// NFS-specific options
	DisableReaddirplus bool

	// Security policy
	AllowAuthSys      bool
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string

	// Retention policy for local blocks.
	RetentionPolicy blockstore.RetentionPolicy
	RetentionTTL    time.Duration

	// BlockStore is the per-share block store orchestrator.
	// Nil only for metadata-only shares (unlikely in practice).
	BlockStore *engine.Store

	// remoteConfigID tracks which remote store config this share uses (for ref counting).
	remoteConfigID string

	// gcStateRoot is the on-disk directory under which the GC engine
	// persists per-run gc-state and `last-run.json`.
	// Populated for fs-backed local stores at share creation; empty for
	// in-memory stores (no persistent gc-state then — last-run.json is
	// skipped, matching engine.PersistLastRunSummary's empty-root contract).
	gcStateRoot string

	// localStoreDir is the on-disk per-share local data directory used by
	// the migration tool to locate `.migration-state.jsonl` and by the
	// REST status handler to read it back. Populated for fs-backed local
	// stores at share creation; empty for in-memory backends — the status
	// handler treats "" as "no journal available" rather than an error.
	localStoreDir string
}

// GCStateRoot returns the per-share gc-state directory used by the GC
// engine to persist last-run.json. Empty when the share's local store
// has no persistent root (in-memory backend).
func (s *Share) GCStateRoot() string { return s.gcStateRoot }

// ShareConfig contains all configuration needed to create a share.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ReadOnly      bool
	// Enabled is the persisted `shares.enabled` flag. Callers pass the
	// DB value; AddShare copies it onto the runtime Share.
	Enabled bool

	DefaultPermission string

	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	EncryptData bool

	// AclFlagInheritedCanonicalization mirrors models.Share's per-share toggle
	// for MS-DTYP §2.5.3.4.2 canonicalization of SE_DACL_AUTO_INHERITED. See
	// the runtime Share field for semantics (refs #514).
	AclFlagInheritedCanonicalization bool

	// AccessBasedEnumeration mirrors models.Share's per-share toggle for
	// Windows access-based enumeration. See the runtime Share field for
	// semantics (refs #532).
	AccessBasedEnumeration bool

	// ChangeNotifyDisabled mirrors models.Share's per-share toggle that
	// rejects SMB2 CHANGE_NOTIFY with STATUS_NOT_IMPLEMENTED.
	ChangeNotifyDisabled bool

	// StreamsDisabled mirrors models.Share's per-share toggle that rejects
	// SMB2 CREATE on Alternate Data Streams with STATUS_OBJECT_NAME_INVALID
	// (named ADS, `::$DATA`, or any stream-type suffix). When set, the SMB
	// handler also strips FILE_NAMED_STREAMS from the
	// FileFsAttributeInformation FileSystemAttributes mask so the
	// filesystem advertises no ADS support.
	StreamsDisabled bool

	// ContinuousAvailability mirrors models.Share's per-share toggle that
	// advertises SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY and enables SMB3
	// persistent durable handles (refs #739).
	ContinuousAvailability bool

	// TrashEnabled turns on the per-share recycle bin (#190). Default false.
	// Read per-delete via the locked TrashSettingsForShare accessor (NOT off a
	// shared *Share pointer) so it is concurrency-safe and takes effect live.
	TrashEnabled bool
	// TrashRetentionDays auto-empties bin entries older than N days (0 = keep forever).
	TrashRetentionDays int
	// TrashRestrictToAdmin limits empty/force-delete to admins (users may still restore).
	TrashRestrictToAdmin bool
	// TrashMaxBytes caps total bin bytes (0 = unbounded); over-cap evicts oldest.
	TrashMaxBytes int64
	// TrashExcludePatterns are globs that bypass the bin (immediate delete).
	TrashExcludePatterns []string

	RootAttr *metadata.FileAttr

	DisableReaddirplus bool

	AllowAuthSys      bool
	AllowAuthSysSet   bool // true when AllowAuthSys was explicitly set (distinguishes false from unset)
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string

	// Retention policy for local blocks.
	RetentionPolicy blockstore.RetentionPolicy
	RetentionTTL    time.Duration

	// Per-share block store size overrides (0 = use system default).
	LocalStoreSize int64
	ReadBufferSize int64

	// Per-share byte quota (0 = unlimited).
	QuotaBytes int64

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

// MetadataServiceDeregistrar deregisters a metadata store for a share. The
// concrete *metadata.MetadataService satisfies it. AddShare's defensive
// finalize-failure path uses it to avoid leaking a metadata registration for a
// share it refuses to finalize.
type MetadataServiceDeregistrar interface {
	RemoveStoreForShare(shareName string)
}

// BlockStoreConfigProvider resolves block store configurations from the control plane DB.
type BlockStoreConfigProvider interface {
	GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
}

// ShareStore is the narrow subset of pkg/controlplane/store.ShareStore that
// DisableShare / EnableShare need. Defined here so callers can pass any store
// that satisfies it (the concrete GORMStore does) without importing the
// `store` package from this subtree and creating a cycle.
type ShareStore interface {
	GetShare(ctx context.Context, name string) (*models.Share, error)
	UpdateShare(ctx context.Context, share *models.Share) error
}

// LocalStoreDefaults holds default sizing for per-share local stores.
type LocalStoreDefaults struct {
	MaxSize   uint64 // Maximum local store size per share (0 = unlimited)
	MaxMemory int64  // Memory budget for dirty buffers per share (0 = 256MB)

	// ReadBufferBytes is the per-share read buffer budget in bytes (0 = disabled).
	ReadBufferBytes int64

	// DedupLRUSize is the slot count for the in-memory hash dedup LRU; the
	// per-share block-store JSON config `dedup_lru_size` takes precedence
	// when set, otherwise this global YAML default (blockstore.local.dedup_lru_size)
	// is applied, otherwise FSStore falls back to its own internal default.
	DedupLRUSize int
}

// SyncerDefaults holds default syncer configuration applied to all shares.
type SyncerDefaults struct {
	ParallelUploads    int
	ParallelDownloads  int
	PrefetchBlocks     int
	SmallFileThreshold int64
	UploadInterval     time.Duration
	UploadDelay        time.Duration

	// PrefetchWorkers is the number of read buffer prefetch workers per share (0 = disabled).
	PrefetchWorkers int
}

// sharedRemote holds a reference-counted remote store shared across shares.
type sharedRemote struct {
	store    remote.RemoteStore
	refCount int
	configID string
}

// nonClosingRemote wraps a remote.RemoteStore and makes Close() a no-op.
// This prevents engine.Store.Close() from closing the shared remote;
// the shares.Service.releaseRemoteStore handles actual closing via ref counting.
type nonClosingRemote struct {
	remote.RemoteStore
}

func (n *nonClosingRemote) Close() error { return nil }

// Service manages share registration, lookup, and configuration.
type Service struct {
	mu       sync.RWMutex
	registry map[string]*Share
	// reservations holds share names that an in-flight AddShare has claimed but
	// not yet finalized into registry. It closes the AddShare(sameName) race
	// (REVIEW M2): the name is reserved under s.mu BEFORE any side-effecting
	// metadata/block-store init, so a concurrent AddShare for the same name
	// fails early — before it can RegisterStoreForShare and leave a
	// metadata-store/registry mismatch when it later loses the registry recheck.
	// A reserved name is NOT yet in registry, so handlers never observe a
	// half-built share.
	reservations    map[string]struct{}
	remoteStores    map[string]*sharedRemote // configID -> shared remote
	nextCallbackID  int
	changeCallbacks map[int]func(shares []string)
}

func New() *Service {
	return &Service{
		registry:        make(map[string]*Share),
		reservations:    make(map[string]struct{}),
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

// buildSyncerConfigFromDefaults merges SyncerDefaults into a engine.SyncerConfig.
func buildSyncerConfigFromDefaults(defaults *SyncerDefaults) engine.SyncerConfig {
	cfg := engine.DefaultConfig()
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
	// File handles are encoded as "<shareName>:<uuid>" and decoded by
	// splitting on the FIRST ':'. A share name containing ':' yields handles
	// whose UUID component fails to parse, silently bricking every file in the
	// share. Reject it up front with an ErrInvalidArgument StoreError.
	if strings.Contains(config.Name, ":") {
		return metadata.NewInvalidArgumentError(
			fmt.Sprintf("share name %q must not contain ':'", config.Name))
	}

	if config.LocalBlockStoreID != "" && blockStoreProvider == nil {
		return fmt.Errorf("block store provider is required when LocalBlockStoreID is set for share %q", config.Name)
	}

	if metadataSvc == nil {
		return fmt.Errorf("metadata service registrar is required for share %q", config.Name)
	}

	// Phase 0: Reserve the share name under s.mu BEFORE any side-effecting init
	// (root-dir create, block-store start, metadata RegisterStoreForShare). This
	// closes the AddShare(sameName) race (REVIEW M2): the OLD ordering let two
	// racing callers both run RegisterStoreForShare (last writer wins on the
	// MetadataService store map) and only THEN recheck the registry under the
	// lock — so the loser would tear down its block store yet leave its metadata
	// store registered, pointing the MetadataService at a different store than
	// the registry exposes. Reserving here serializes on the name first: the
	// loser fails before touching any shared store, so no mismatch can form.
	//
	// The reservation is NOT inserted into registry, so a half-built share is
	// never visible to handlers; it is converted to a registry entry only in
	// Phase 4 after all init succeeds.
	if err := s.reserveShareName(config.Name); err != nil {
		return err
	}
	// Track whether the reservation still needs releasing. Phase 4 hands the
	// reservation off to the registry entry (under the same lock) and clears
	// this flag; every failure path releases it.
	reservationHeld := true
	defer func() {
		if reservationHeld {
			s.releaseShareName(config.Name)
		}
	}()

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

	// Phase 3: Register metadata store. Safe to call unconditionally now: the
	// Phase-0 reservation guarantees no other AddShare for this name is in
	// flight, so this RegisterStoreForShare cannot be raced into a
	// last-writer-wins mismatch.
	if err := metadataSvc.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		cleanupShare()
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	// Phase 4: Convert the reservation into a registry entry under s.mu.
	// Only now is the share visible to protocol handlers. The reservation has
	// held the name exclusively since Phase 0, so registry[name] cannot already
	// exist here; we assert it defensively and hand off the reservation.
	s.mu.Lock()
	if _, exists := s.registry[config.Name]; exists {
		// Should be unreachable while the reservation is held, but stay
		// fail-safe: tear down rather than overwrite an existing share.
		s.mu.Unlock()
		cleanupShare()
		// Deregister the metadata store we just published so we do not leak a
		// registration for a share we are refusing to finalize.
		if remover, ok := metadataSvc.(MetadataServiceDeregistrar); ok {
			remover.RemoveStoreForShare(config.Name)
		}
		return fmt.Errorf("share %q already exists", config.Name)
	}
	s.registry[config.Name] = share
	delete(s.reservations, config.Name)
	reservationHeld = false
	s.mu.Unlock()

	s.notifyShareChange()

	return nil
}

// reserveShareName claims a share name for an in-flight AddShare. It fails if
// the name is already registered or already reserved by a concurrent AddShare.
func (s *Service) reserveShareName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.registry[name]; exists {
		return fmt.Errorf("share %q already exists", name)
	}
	if _, reserved := s.reservations[name]; reserved {
		return fmt.Errorf("share %q is already being added", name)
	}
	s.reservations[name] = struct{}{}
	return nil
}

// releaseShareName drops an in-flight AddShare reservation. Idempotent.
func (s *Service) releaseShareName(name string) {
	s.mu.Lock()
	delete(s.reservations, name)
	s.mu.Unlock()
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
		Name:                             config.Name,
		MetadataStore:                    config.MetadataStore,
		RootHandle:                       rootHandle,
		ReadOnly:                         config.ReadOnly,
		Enabled:                          config.Enabled,
		EncryptData:                      config.EncryptData,
		AclFlagInheritedCanonicalization: config.AclFlagInheritedCanonicalization,
		AccessBasedEnumeration:           config.AccessBasedEnumeration,
		ChangeNotifyDisabled:             config.ChangeNotifyDisabled,
		StreamsDisabled:                  config.StreamsDisabled,
		ContinuousAvailability:           config.ContinuousAvailability,
		TrashEnabled:                     config.TrashEnabled,
		TrashRetentionDays:               config.TrashRetentionDays,
		TrashRestrictToAdmin:             config.TrashRestrictToAdmin,
		TrashMaxBytes:                    config.TrashMaxBytes,
		TrashExcludePatterns:             config.TrashExcludePatterns,
		DefaultPermission:                config.DefaultPermission,
		Squash:                           config.Squash,
		AnonymousUID:                     config.AnonymousUID,
		AnonymousGID:                     config.AnonymousGID,
		DisableReaddirplus:               config.DisableReaddirplus,
		AllowAuthSys:                     allowAuthSys,
		RequireKerberos:                  config.RequireKerberos,
		MinKerberosLevel:                 config.MinKerberosLevel,
		NetgroupName:                     config.NetgroupName,
		BlockedOperations:                config.BlockedOperations,
		RetentionPolicy:                  config.RetentionPolicy,
		RetentionTTL:                     config.RetentionTTL,
	}

	return share, metadataStore, nil
}

// mergeLocalStoreDefaults returns a copy of the system defaults with per-share
// overrides applied. Non-zero ShareConfig values take precedence.
func mergeLocalStoreDefaults(defaults *LocalStoreDefaults, config *ShareConfig) *LocalStoreDefaults {
	if defaults == nil {
		defaults = &LocalStoreDefaults{}
	}
	merged := *defaults // shallow copy
	if config.LocalStoreSize > 0 {
		merged.MaxSize = uint64(config.LocalStoreSize)
	}
	if config.ReadBufferSize > 0 {
		merged.ReadBufferBytes = config.ReadBufferSize
	}
	return &merged
}

// createBlockStoreForShare creates and starts a per-share BlockStore.
func (s *Service) createBlockStoreForShare(
	ctx context.Context,
	share *Share,
	config *ShareConfig,
	blockStoreProvider BlockStoreConfigProvider,
	fileBlockStore blockstore.EngineFileBlockStore,
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

	// Merge per-share size overrides into effective defaults.
	effectiveDefaults := mergeLocalStoreDefaults(localStoreDefaults, config)

	localStore, err := CreateLocalStoreFromConfig(ctx, localCfg.Type, localCfg, config.Name, effectiveDefaults, fileBlockStore)
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
	// must not be pin mode (pin keeps blocks stored locally indefinitely).
	localStore.SetEvictionEnabled(remoteStore != nil && config.RetentionPolicy != blockstore.RetentionPin)
	// Note: SetSkipFsync was removed. Local-disk durability is now
	// unconditional (the syncer will refetch from S3 on the rare crash path).
	localStore.SetRetentionPolicy(config.RetentionPolicy, config.RetentionTTL)

	syncerCfg := buildSyncerConfigFromDefaults(syncerDefaults)

	// Wrap shared remote in nonClosingRemote so engine.Close() doesn't close it;
	// releaseRemoteStore handles actual closing via ref counting.
	var engineRemote remote.RemoteStore
	if remoteStore != nil {
		engineRemote = &nonClosingRemote{remoteStore}
	}

	syncer := engine.NewSyncer(localStore, engineRemote, fileBlockStore, syncerCfg)

	cleanup := func() {
		_ = syncer.Close()
		_ = localStore.Close()
		if remoteConfigID != "" {
			s.releaseRemoteStore(remoteConfigID)
		}
	}

	// Wire the metadata coordinator so the engine can invoke RefCount
	// mutations + FileAttr.Blocks persistence without importing
	// pkg/metadata on its hot paths. The fileBlockStore on the engine
	// seam is the per-share metadata store cast to EngineFileBlockStore;
	// the coordinator wraps the same store as a metadata.MetadataStore
	// for the typed operations.
	var coordinator engine.MetadataCoordinator
	if metadataStore, ok := fileBlockStore.(metadata.MetadataStore); ok {
		coordinator = newMetadataCoordinator(metadataStore)
	}

	engineCfg := engine.BlockStoreConfig{
		Local:          localStore,
		Remote:         engineRemote,
		Syncer:         syncer,
		FileBlockStore: fileBlockStore,
		Coordinator:    coordinator,
	}
	// Thread the SyncedHashStore from the same per-share metadata
	// backend the coordinator wraps so the engine's mirror-loop Flush
	// can MarkSynced after each successful remote.Put. The interface
	// check is the standard runtime-type narrowing used elsewhere in
	// this factory (RollupStore, MetadataStore).
	if shs, ok := fileBlockStore.(metadata.SyncedHashStore); ok {
		engineCfg.SyncedHashStore = shs
	}
	if effectiveDefaults != nil {
		engineCfg.ReadBufferBytes = effectiveDefaults.ReadBufferBytes
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
	// Compute the persistent gc-state directory for this share. Only fs-backed
	// local stores produce a non-empty path; in-memory backends skip
	// last-run.json persistence entirely (engine.PersistLastRunSummary is a
	// no-op on empty rootDir).
	share.gcStateRoot = deriveGCStateRoot(localCfg, config.Name)
	// per-share local data dir for the migration journal.
	// Same source-of-truth + emptiness semantics as gcStateRoot — memory
	// backends produce "" so the status handler can short-circuit.
	share.localStoreDir = deriveLocalStoreDir(localCfg, config.Name)

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

	// Decorator order matters: encryption sits BELOW compression on the
	// data flow (caller → compression → encryption → inner). Compress
	// plaintext first so the compressor sees redundancy; encrypted bytes
	// are incompressible by design.
	//
	// Apply order in code is therefore encryption first (innermost),
	// then compression (outermost).
	encWrapped, err := maybeWrapEncryption(ctx, newStore, remoteCfg)
	if err != nil {
		_ = newStore.Close()
		return nil, "", fmt.Errorf("failed to apply encryption policy: %w", err)
	}
	newStore = encWrapped

	wrapped, err := maybeWrapCompression(newStore, remoteCfg)
	if err != nil {
		_ = newStore.Close()
		return nil, "", fmt.Errorf("failed to apply compression policy: %w", err)
	}
	newStore = wrapped

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

// maybeWrapEncryption inspects the remote config's "encryption" key and,
// when present, wraps inner with an encryption.EncryptedRemote. Returns
// inner unchanged when the key is absent.
//
// Key-provider lifetime is bound to the decorator: NewRemote captures
// the provider, and EncryptedRemote.Close calls provider.Close. The
// outer releaseRemoteStore path therefore closes the provider as part
// of the normal decorator teardown.
func maybeWrapEncryption(ctx context.Context, inner remote.RemoteStore, cfg *models.BlockStoreConfig) (remote.RemoteStore, error) {
	parsed, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("parse block store config: %w", err)
	}
	raw, ok := parsed["encryption"]
	if !ok {
		return inner, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal encryption sub-config: %w", err)
	}
	policy, err := encryption.ParsePolicy(encoded)
	if err != nil {
		return nil, err
	}
	provider, err := keyprovider.NewProvider(ctx, policy.Key)
	if err != nil {
		return nil, fmt.Errorf("create key provider: %w", err)
	}
	wrapped, err := encryption.NewRemote(inner, policy, provider)
	if err != nil {
		_ = provider.Close()
		return nil, err
	}
	return wrapped, nil
}

// maybeWrapCompression inspects the remote config's "compression" key
// and, when present, wraps inner with a compression.Decorator. Returns
// inner unchanged when the key is absent.
func maybeWrapCompression(inner remote.RemoteStore, cfg *models.BlockStoreConfig) (remote.RemoteStore, error) {
	parsed, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("parse block store config: %w", err)
	}
	raw, ok := parsed["compression"]
	if !ok {
		return inner, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal compression sub-config: %w", err)
	}
	policy, err := compression.ParsePolicy(encoded)
	if err != nil {
		return nil, err
	}
	return compression.NewRemote(inner, policy)
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
//
// Teardown is ordered best-effort (REVIEW M4): every step runs even if an
// earlier one fails, and the per-step errors are aggregated into the returned
// error so the registry / snapshot-dir / block-store / remote-ref state is
// never left half-removed by an early return. The registry entry is dropped
// first (under the lock) so the share disappears from routing immediately;
// the remaining steps are pure resource teardown.
//
// bs.Close() is now drain-safe (area-7 H-A): it takes the engine's lifecycle
// write lock, which blocks until all in-flight WriteAt/ReadAt/Flush ops on the
// store have completed, so calling it here outside s.mu can no longer race a
// client mid-transfer into a torn op or panic.
func (s *Service) RemoveShare(name string) error {
	s.mu.Lock()
	share, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q not found", name)
	}
	bs := share.BlockStore
	remoteConfigID := share.remoteConfigID
	localStoreDir := share.localStoreDir
	delete(s.registry, name)
	s.mu.Unlock()

	var errs []error

	// Cleanup per-share snapshot directories alongside registry removal.
	// The DB row is the source of truth; orphaned files left behind on a
	// removal error are operationally harmless, so we log + aggregate but
	// continue with the rest of the teardown.
	if localStoreDir != "" {
		snapsDir := filepath.Join(localStoreDir, "snapshots")
		if err := os.RemoveAll(snapsDir); err != nil {
			logger.Warn("RemoveShare: failed to remove snapshots dir",
				"share", name, "dir", snapsDir, "error", err)
			errs = append(errs, fmt.Errorf("remove snapshots dir %q: %w", snapsDir, err))
		}
	}

	if bs != nil {
		if err := bs.Close(); err != nil {
			logger.Warn("Failed to close BlockStore for share", "share", name, "error", err)
			errs = append(errs, fmt.Errorf("close block store for share %q: %w", name, err))
		}
	}

	// Always release the remote-store reference even if a prior step errored,
	// otherwise a Close failure would leak the shared remote ref-count.
	if remoteConfigID != "" {
		s.releaseRemoteStore(remoteConfigID)
	}

	s.notifyShareChange()

	return errors.Join(errs...)
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

// TrashSettings is a per-share recycle-bin policy snapshot, returned by value
// under the service lock so callers never read a mutating shared pointer.
type TrashSettings struct {
	Enabled         bool
	RetentionDays   int
	RestrictToAdmin bool
	MaxBytes        int64
	ExcludePatterns []string
}

// TrashSettingsForShare returns the recycle-bin policy for a share, read under
// the service lock. Returns ok=false if the share is unknown. Never hands out
// the *Share; the ExcludePatterns slice is copied so callers cannot observe a
// concurrent SetShareTrashConfig mutation.
//
// The recycle decision is read per-delete from many protocol goroutines while
// config can be updated live, so this accessor returns a VALUE under the lock
// (refs #190, #936 race lesson).
func (s *Service) TrashSettingsForShare(name string) (TrashSettings, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, exists := s.registry[name]
	if !exists {
		return TrashSettings{}, false
	}
	return TrashSettings{
		Enabled:         share.TrashEnabled,
		RetentionDays:   share.TrashRetentionDays,
		RestrictToAdmin: share.TrashRestrictToAdmin,
		MaxBytes:        share.TrashMaxBytes,
		ExcludePatterns: append([]string(nil), share.TrashExcludePatterns...),
	}, true
}

// EnabledTrashShares returns the names of all shares with trash enabled, read
// under the service lock. Used by the reaper loop to enumerate bins to sweep.
func (s *Service) EnabledTrashShares() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for name, share := range s.registry {
		if share.TrashEnabled {
			out = append(out, name)
		}
	}
	return out
}

// SetShareTrashConfig updates a live share's recycle-bin settings under the
// write lock and persists them to the DB. Pairs safely with the RLock
// TrashSettingsForShare accessor: the exact fields the reader copies are the
// ones this mutates while holding the write lock.
//
// Returns ErrShareNotFound if the share name is unknown in the runtime
// registry. The optional store argument persists the change; pass nil to
// update runtime state only (used by tests that bypass the DB).
func (s *Service) SetShareTrashConfig(store ShareStore, name string, cfg TrashSettings) error {
	excludes := append([]string(nil), cfg.ExcludePatterns...)

	s.mu.Lock()
	share, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.TrashEnabled = cfg.Enabled
	share.TrashRetentionDays = cfg.RetentionDays
	share.TrashRestrictToAdmin = cfg.RestrictToAdmin
	share.TrashMaxBytes = cfg.MaxBytes
	share.TrashExcludePatterns = excludes
	s.mu.Unlock()

	if store == nil {
		return nil
	}

	dbShare, err := store.GetShare(context.Background(), name)
	if err != nil {
		return fmt.Errorf("load share %q: %w", name, err)
	}
	dbShare.TrashEnabled = cfg.Enabled
	dbShare.TrashRetentionDays = cfg.RetentionDays
	dbShare.TrashRestrictToAdmin = cfg.RestrictToAdmin
	dbShare.TrashMaxBytes = cfg.MaxBytes
	dbShare.SetTrashExcludePatterns(excludes)
	if err := store.UpdateShare(context.Background(), dbShare); err != nil {
		return fmt.Errorf("persist trash config for share %q: %w", name, err)
	}
	return nil
}

// SetShareNetgroup updates the live netgroup association for a share's NFS
// export. An empty netgroupName clears the association (allow-all). The change
// takes effect immediately: CheckNetgroupAccess reads NetgroupName from this
// runtime registry, so subsequent NFS operations honour the new allowlist
// without an adapter restart. Returns an error if the share is unknown.
func (s *Service) SetShareNetgroup(name, netgroupName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	share, exists := s.registry[name]
	if !exists {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.NetgroupName = netgroupName
	return nil
}

// GetShareNetgroupName returns the live netgroup association for a share,
// read under s.mu. Callers (e.g. CheckNetgroupAccess) must use this rather
// than reading NetgroupName off a *Share returned by GetShare: GetShare hands
// back the shared registry pointer with the lock already dropped, so reading
// the field there races with SetShareNetgroup's write under s.mu. The bool is
// false when the share is unknown.
func (s *Service) GetShareNetgroupName(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return "", false
	}
	return share.NetgroupName, true
}

// DisableShare sets enabled=false on the share's DB row and runtime Share
// struct, then invokes notifyShareChange so adapters drop active sessions.
// DB-first-then-runtime ordering is crash-consistent: if the process dies
// between the two, the next boot reconciles runtime from DB.
//
// Idempotent: re-calling on an already-disabled share returns
// ErrShareAlreadyDisabled without writing to DB or disturbing adapters.
//
// Returns ErrShareNotFound if the share name is unknown at either layer.
// Timeout bound is whatever the caller provides via ctx.
//
// Requires `"enabled"` in GORMStore.UpdateShare's update whitelist —
// otherwise the store.UpdateShare call silently drops the flag.
func (s *Service) DisableShare(ctx context.Context, store ShareStore, name string) error {
	// Runtime registry must know the share before we touch the DB — prevents
	// a DB-disabled/runtime-absent inconsistency when the startup load missed
	// a share (partial boot) or the caller passed a stale name.
	s.mu.RLock()
	_, exists := s.registry[name]
	s.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}

	dbShare, err := store.GetShare(ctx, name)
	if err != nil {
		return fmt.Errorf("load share %q: %w", name, err)
	}
	if !dbShare.Enabled {
		return ErrShareAlreadyDisabled
	}
	dbShare.Enabled = false
	if err := store.UpdateShare(ctx, dbShare); err != nil {
		return fmt.Errorf("persist disabled state for share %q: %w", name, err)
	}

	s.mu.Lock()
	share, stillExists := s.registry[name]
	if !stillExists {
		s.mu.Unlock()
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}
	share.Enabled = false
	s.mu.Unlock()

	s.notifyShareChange()
	return nil
}

// EnableShare inverts DisableShare. Idempotent: re-calling on an
// already-enabled share is a no-op (returns nil, no DB write).
func (s *Service) EnableShare(ctx context.Context, store ShareStore, name string) error {
	// Registry-first check: same rationale as DisableShare — avoid a DB row
	// that moves while the runtime has no matching entry.
	s.mu.RLock()
	_, exists := s.registry[name]
	s.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}

	dbShare, err := store.GetShare(ctx, name)
	if err != nil {
		return fmt.Errorf("load share %q: %w", name, err)
	}
	if dbShare.Enabled {
		return nil
	}
	dbShare.Enabled = true
	if err := store.UpdateShare(ctx, dbShare); err != nil {
		return fmt.Errorf("persist enabled state for share %q: %w", name, err)
	}

	s.mu.Lock()
	share, stillExists := s.registry[name]
	if !stillExists {
		s.mu.Unlock()
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}
	share.Enabled = true
	s.mu.Unlock()

	s.notifyShareChange()
	return nil
}

// IsShareEnabled returns the runtime Enabled flag for the named share.
// Mirror of GetShare read-path discipline (RLock + registry lookup).
func (s *Service) IsShareEnabled(name string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, exists := s.registry[name]
	if !exists {
		return false, fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	return share.Enabled, nil
}

// ListEnabledSharesForStore returns the names of all runtime shares that
// (a) have Enabled=true AND (b) reference metadataStoreName as their
// metadata store.
func (s *Service) ListEnabledSharesForStore(metadataStoreName string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for name, share := range s.registry {
		if share.Enabled && share.MetadataStore == metadataStoreName {
			out = append(out, name)
		}
	}
	return out
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

// GetGCStateDirForShare returns the per-share gc-state directory used by
// the GC engine to persist `last-run.json`. Returns an empty string when
// the share's local store has no persistent root (in-memory backend) —
// callers should treat empty as "no run summary available". Returns an
// ErrShareNotFound-wrapped error if the share is unknown.
func (s *Service) GetGCStateDirForShare(name string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return "", fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	return share.gcStateRoot, nil
}

// LocalStoreDir returns the per-share on-disk data directory used by the
// migration tool to locate `.migration-state.jsonl` and by the REST
// status handler to read it back. Mirrors GetGCStateDirForShare's
// empty-string-for-memory-backend contract: callers should treat "" as
// "no on-disk journal available" rather than an error.
//
// Returns an ErrShareNotFound-wrapped error when the share is unknown so
// callers can map it to a deterministic 404.
func (s *Service) LocalStoreDir(name string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return "", fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	return share.localStoreDir, nil
}

// SetLocalStoreDirForTesting overrides the per-share localStoreDir field.
// Test-only — used by handler unit tests that bypass the full
// AddShare composition path (which requires a DB-backed
// BlockStoreConfig). Returns ErrShareNotFound if the share is not
// registered.
func (s *Service) SetLocalStoreDirForTesting(name, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.localStoreDir = dir
	return nil
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
func (s *Service) GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.Store, error) {
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
func (s *Service) GetBlockStoreForShare(name string) (*engine.Store, error) {
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

// RemoteStoreEntry describes a distinct remote block store that is referenced
// by one or more shares. Surface used by production block-GC enumeration
// (Runtime.RunBlockGC): we want each underlying remote store scanned exactly
// once per run, not once per share.
type RemoteStoreEntry struct {
	// Store is the underlying remote store (NOT the nonClosingRemote wrapper).
	Store remote.RemoteStore
	// ConfigID is the remote block-store config UUID this entry represents.
	// Empty string indicates a test-only binding (SetShareRemoteForTest).
	ConfigID string
	// Shares are the registered share names that reference this remote.
	Shares []string
}

// DistinctRemoteStores returns every distinct underlying remote.RemoteStore
// referenced by at least one registered share. Shares that reference the same
// remote-store config (ref-counted via remoteStores) are grouped into a
// single entry — deduped by ConfigID, NOT by the per-share nonClosingRemote
// wrapper pointer. Local-only shares (no remote) contribute nothing.
//
// Returned entries have a non-nil Store and a non-empty Shares slice. Order
// is unspecified (map iteration).
func (s *Service) DistinctRemoteStores() []RemoteStoreEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Bucket share names by the configID they reference. configID == "" means
	// "local-only share" — skipped entirely.
	sharesByConfigID := make(map[string][]string, len(s.remoteStores))
	for name, sh := range s.registry {
		if sh.remoteConfigID == "" {
			continue
		}
		sharesByConfigID[sh.remoteConfigID] = append(sharesByConfigID[sh.remoteConfigID], name)
	}

	out := make([]RemoteStoreEntry, 0, len(sharesByConfigID))
	for cid, shareNames := range sharesByConfigID {
		sr, ok := s.remoteStores[cid]
		if !ok || sr == nil || sr.store == nil {
			// Orphaned configID → skip. DistinctRemoteStores is a read-only
			// surface; we don't try to self-heal bookkeeping here.
			continue
		}
		out = append(out, RemoteStoreEntry{
			Store:    sr.store,
			ConfigID: cid,
			Shares:   shareNames,
		})
	}
	return out
}

// SetShareRemoteForTest installs a remote.RemoteStore for the named share
// and registers it under a synthetic configID derived from the store's
// pointer identity. Two calls with the same remote store for different
// shares share one configID — matching production ref-counting behavior
// — so DistinctRemoteStores() dedupes correctly.
//
// Test-only: panics if the share does not exist. Intended for runtime-
// package tests that need to exercise RunBlockGC's enumeration without
// standing up a full engine.Store. Not safe for production callers.
func (s *Service) SetShareRemoteForTest(shareName string, rs remote.RemoteStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[shareName]
	if !ok {
		panic(fmt.Sprintf("SetShareRemoteForTest: share %q not registered", shareName))
	}
	// Derive a stable configID from the remote store pointer so calls that
	// pass the same rs for different shares land in the same sharedRemote
	// bucket (mirroring production ref-count semantics).
	cid := fmt.Sprintf("test-remote-%p", rs)
	if existing, ok := s.remoteStores[cid]; ok {
		existing.refCount++
	} else {
		s.remoteStores[cid] = &sharedRemote{
			store:    rs,
			refCount: 1,
			configID: cid,
		}
	}
	share.remoteConfigID = cid
}

// DrainAllBlockStores drains all pending uploads across all per-share BlockStores.
func (s *Service) DrainAllBlockStores(ctx context.Context) error {
	s.mu.RLock()
	blockStores := make([]*engine.Store, 0, len(s.registry))
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

// ShareBlockStoreStats holds block store statistics for a single share.
type ShareBlockStoreStats struct {
	ShareName string                 `json:"share_name"`
	Stats     engine.BlockStoreStats `json:"stats"`
}

// BlockStoreStatsResponse holds aggregated and per-share block store statistics.
type BlockStoreStatsResponse struct {
	Totals   engine.BlockStoreStats `json:"totals"`
	PerShare []ShareBlockStoreStats `json:"per_share,omitempty"`
}

// EvictOptions controls which block store tiers to evict.
type EvictOptions struct {
	ReadBufferOnly bool `json:"read_buffer_only"`
	LocalOnly      bool `json:"local_only"`
}

// EvictResult holds the result of a block store eviction operation.
type EvictResult struct {
	ReadBufferEntriesCleared int   `json:"read_buffer_entries_cleared"`
	LocalFilesEvicted        int   `json:"local_files_evicted"`
	BytesFreed               int64 `json:"bytes_freed"`
}

// GetBlockStoreStats returns block store statistics, optionally filtered by share name.
// If shareName is empty, returns aggregated stats across all shares with per-share breakdown.
func (s *Service) GetBlockStoreStats(shareName string) (*BlockStoreStatsResponse, error) {
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
		stats := share.BlockStore.GetStats()
		return &BlockStoreStatsResponse{
			Totals:   stats,
			PerShare: []ShareBlockStoreStats{{ShareName: shareName, Stats: stats}},
		}, nil
	}

	var totals engine.BlockStoreStats
	var perShare []ShareBlockStoreStats

	for name, share := range s.registry {
		if share.BlockStore == nil {
			continue
		}
		stats := share.BlockStore.GetStats()
		perShare = append(perShare, ShareBlockStoreStats{ShareName: name, Stats: stats})
		addBlockStoreStats(&totals, stats)
	}

	return &BlockStoreStatsResponse{
		Totals:   totals,
		PerShare: perShare,
	}, nil
}

// addBlockStoreStats accumulates src into dst (field-by-field summation).
func addBlockStoreStats(dst *engine.BlockStoreStats, src engine.BlockStoreStats) {
	dst.FileCount += src.FileCount
	dst.BlocksDirty += src.BlocksDirty
	dst.BlocksLocal += src.BlocksLocal
	dst.BlocksRemote += src.BlocksRemote
	dst.BlocksTotal += src.BlocksTotal
	dst.LocalDiskUsed += src.LocalDiskUsed
	dst.LocalDiskMax += src.LocalDiskMax
	dst.LocalMemUsed += src.LocalMemUsed
	dst.LocalMemMax += src.LocalMemMax
	dst.ReadBufferEntries += src.ReadBufferEntries
	dst.ReadBufferUsed += src.ReadBufferUsed
	dst.ReadBufferMax += src.ReadBufferMax
	dst.PendingSyncs += src.PendingSyncs
	dst.PendingUploads += src.PendingUploads
	dst.CompletedSyncs += src.CompletedSyncs
	dst.FailedSyncs += src.FailedSyncs
	if src.HasRemote {
		dst.HasRemote = true
	}
}

// EvictBlockStore evicts block store data for the given share (or all shares if shareName is empty).
// Returns an error if trying to evict local blocks without a remote store (safety check).
func (s *Service) EvictBlockStore(ctx context.Context, shareName string, opts EvictOptions) (*EvictResult, error) {
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

		if !opts.ReadBufferOnly && !bs.HasRemoteStore() {
			return nil, fmt.Errorf("cannot evict local blocks for share %q: no remote store configured (data would be lost)", share.Name)
		}

		if !opts.LocalOnly {
			result.ReadBufferEntriesCleared += bs.DestroyCache()
		}

		if !opts.ReadBufferOnly {
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

// deriveGCStateRoot returns the per-share gc-state directory used by the
// GC engine to persist its run state and last-run.json. Mirrors the path
// layout used in CreateLocalStoreFromConfig for fs-backed local stores
// (`<basePath>/shares/<sanitized>/gc-state`). Returns "" for any non-fs
// backend or when the config does not yield a usable absolute path —
// engine.PersistLastRunSummary treats "" as "do not persist".
func deriveGCStateRoot(localCfg interface {
	GetConfig() (map[string]any, error)
}, shareName string) string {
	if localCfg == nil {
		return ""
	}
	cfg, err := localCfg.GetConfig()
	if err != nil {
		return ""
	}
	basePath, ok := cfg["path"].(string)
	if !ok || basePath == "" {
		return ""
	}
	expanded, err := pathutil.ExpandPath(basePath)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(expanded) {
		return ""
	}
	return filepath.Join(expanded, "shares", sanitizeShareName(shareName), "gc-state")
}

// deriveLocalStoreDir returns the per-share on-disk data directory the
// migration tool uses to host `.migration-state.jsonl` and the rolling
// snapshot. Mirrors deriveGCStateRoot's path-extraction logic for
// fs-backed local stores; returns "" for in-memory or unresolvable
// configs (the REST status handler treats "" as "no journal available",
// not an error).
//
// Path layout: `<basePath>/shares/<sanitized>/`. Note the absence of a
// "blocks" or "gc-state" suffix — the migration journal lives at the
// share root next to the blocks directory, not inside it. This matches
// the offline migration tool's --state-dir contract: the operator
// passes the same path the daemon would compute here.
func deriveLocalStoreDir(localCfg interface {
	GetConfig() (map[string]any, error)
}, shareName string) string {
	if localCfg == nil {
		return ""
	}
	cfg, err := localCfg.GetConfig()
	if err != nil {
		return ""
	}
	basePath, ok := cfg["path"].(string)
	if !ok || basePath == "" {
		return ""
	}
	expanded, err := pathutil.ExpandPath(basePath)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(expanded) {
		return ""
	}
	return filepath.Join(expanded, "shares", sanitizeShareName(shareName))
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
	fileBlockStore blockstore.EngineFileBlockStore,
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

	// Per-store max_size from config JSON takes precedence over defaults
	if v, ok := config["max_size"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			maxDisk = int64(n)
		} else {
			logger.Warn("block store config has max_size but it is invalid or non-positive; ignoring", "value", v)
		}
	}

	// Append is mandatory on the local tier — the use_append_log opt-out
	// flag was deleted with the legacy path-keyed writer. Budgets still
	// surface through FSStoreOptions to fs.NewWithOptions; invalid values
	// are warned and ignored.
	var fsOpts fs.FSStoreOptions
	if _, ok := config["use_append_log"]; ok {
		logger.Warn("block store config has use_append_log: append is mandatory in v0.16+, flag is ignored")
	}
	if v, ok := config["max_log_bytes"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			// FIX-15: JSON-decoded numbers land here as float64. Values above
			// 2^53 (~9 PiB) lose integer precision, and non-integer values
			// silently truncate. Warn so a misconfigured budget surfaces in
			// logs instead of producing a budget that is off by hundreds of
			// kilobytes from what the operator typed.
			// Reject out-of-range and non-integer values rather than perform
			// an implementation-defined float64->int64 cast (which on out-of-range
			// inputs can produce a negative or garbage budget).
			if n > float64(math.MaxInt64) || n != math.Trunc(n) {
				logger.Warn("config: max_log_bytes is out of range or non-integer; keeping default", "value", n)
			} else {
				fsOpts.MaxLogBytes = int64(n)
			}
		} else {
			logger.Warn("block store config has max_log_bytes but it is invalid or non-positive; ignoring", "value", v)
		}
	}
	if v, ok := config["rollup_workers"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			fsOpts.RollupWorkers = int(n)
		} else {
			logger.Warn("block store config has rollup_workers but it is invalid or non-positive; ignoring", "value", v)
		}
	}
	if v, ok := config["stabilization_ms"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			fsOpts.StabilizationMS = int(n)
		} else {
			logger.Warn("block store config has stabilization_ms but it is invalid or non-positive; ignoring", "value", v)
		}
	}
	if v, ok := config["orphan_log_min_age_seconds"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			fsOpts.OrphanLogMinAgeSeconds = int(n)
		} else {
			logger.Warn("block store config has orphan_log_min_age_seconds but it is invalid or non-positive; ignoring", "value", v)
		}
	}
	// Dedup LRU slot count. Per-share JSON config `dedup_lru_size` takes
	// precedence; falls back to the global YAML default
	// (blockstore.local.dedup_lru_size) plumbed via LocalStoreDefaults;
	// falls back to FSStore's internal default (4096) when both are zero.
	if defaults != nil && defaults.DedupLRUSize > 0 {
		fsOpts.DedupLRUSize = defaults.DedupLRUSize
	}
	if v, ok := config["dedup_lru_size"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			// JSON-decoded numbers land as float64; reject out-of-range
			// and non-integer values rather than perform an
			// implementation-defined float64->int cast (mirrors the
			// max_log_bytes pattern above).
			if n > float64(math.MaxInt) || n != math.Trunc(n) {
				logger.Warn("config: dedup_lru_size is out of range or non-integer; keeping default", "value", n)
			} else {
				fsOpts.DedupLRUSize = int(n)
			}
		} else {
			logger.Warn("block store config has dedup_lru_size but it is invalid or non-positive; ignoring", "value", v)
		}
	}

	switch storeType {
	case "fs":
		basePath, ok := config["path"].(string)
		if !ok || basePath == "" {
			return nil, errors.New("fs local store requires path in config")
		}
		expanded, err := pathutil.ExpandPath(basePath)
		if err != nil {
			return nil, fmt.Errorf("failed to expand path %q: %w", basePath, err)
		}
		// Defense-in-depth: ValidateBlockStoreConfig rejects relative paths at
		// create/update time, but pre-existing or out-of-band configs could
		// still carry them. Guard here so filepath.Join doesn't resolve
		// against the server's CWD.
		if !filepath.IsAbs(expanded) {
			return nil, fmt.Errorf("fs local store path must be absolute, got %q", basePath)
		}
		sanitized := sanitizeShareName(shareName)
		// The FSStore creates `blocks/` (CAS) and `logs/` (append log) as
		// siblings under its baseDir. A previous layout produced a doubled
		// `shares/{name}/blocks/blocks/...` path. Existing pre-v0.16 installs
		// migrate via `dfs migrate-to-cas` (which uses share-root as its
		// state-dir, already aligned with deriveLocalStoreDir).
		shareDir := filepath.Join(expanded, "shares", sanitized)
		if err := os.MkdirAll(shareDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create share directory: %w", err)
		}

		// Append is mandatory. Wire a RollupStore from the metadata
		// backend and start the rollup worker pool. The type assertion
		// couples the block-store factory to a metadata-layer interface
		// via runtime type check — accepted because memory / badger /
		// postgres all implement both FileBlockStore and RollupStore on
		// the same Store type.
		rs, ok := fileBlockStore.(metadata.RollupStore)
		if !ok {
			return nil, fmt.Errorf("fs local store: metadata backend must implement metadata.RollupStore for the mandatory append-log path")
		}
		fsOpts.RollupStore = rs
		// Wire the SyncedHashStore from the same metadata backend so
		// the local store's ListUnsynced surface can filter the Walk-
		// collected CAS hash set down to the still-unmirrored subset.
		// Required when a remote store is configured; harmless when no
		// remote is wired (mirror loop early-exits in that case).
		if shs, ok := fileBlockStore.(metadata.SyncedHashStore); ok {
			fsOpts.SyncedHashStore = shs
		}
		store, err := fs.NewWithOptions(shareDir, maxDisk, maxMemory, fileBlockStore, fsOpts)
		if err != nil {
			return nil, err
		}
		if err := store.StartRollup(ctx); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("fs local store: StartRollup: %w", err)
		}
		return store, nil

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
		accessKey, _ := config["access_key_id"].(string)
		secretKey, _ := config["secret_access_key"].(string)
		if accessKey == "" || secretKey == "" {
			return nil, errors.New("s3 remote store requires access_key_id and secret_access_key")
		}
		// When a custom endpoint is set (MinIO, Synology, etc.), default to
		// path-style addressing — virtual-hosted style rarely works on
		// non-AWS S3-compatible services. This matches v0.8.x behavior.
		// Only override when the key is absent; honor explicit false.
		forcePathStyle, hasPathStyle := config["force_path_style"].(bool)
		if endpoint != "" && !hasPathStyle {
			forcePathStyle = true
		}

		return remotes3.NewFromConfig(ctx, remotes3.Config{
			Bucket:         bucket,
			Region:         region,
			Endpoint:       endpoint,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			KeyPrefix:      prefix,
			ForcePathStyle: forcePathStyle,
		})

	default:
		return nil, fmt.Errorf("unsupported remote store type: %s", storeType)
	}
}
