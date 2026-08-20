package shares

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local"
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

	// AllowMFsymlink enables automatic MFsymlink-to-real-symlink conversion on
	// CLOSE. Default false. When false, 1067-byte XSym files written by
	// macOS/Windows clients are stored as regular files and never promoted to
	// symlinks (the conversion target is client-controlled, so promotion is
	// opt-in).
	AllowMFsymlink bool

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
	RetentionPolicy block.RetentionPolicy
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

	// writeback records whether this share opted into the metadata writeback
	// tier (#1757) via the local store config's "writeback" bool. When true,
	// AddShare tells the metadata service to relax the per-op FILE_SYNC flush
	// for this share's handles. Default false (durable).
	writeback bool
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

	// AllowMFsymlink mirrors the Share field above: when true, 1067-byte XSym
	// MFsymlink files are converted to real symlinks on CLOSE. Default false.
	AllowMFsymlink bool

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
	RetentionPolicy block.RetentionPolicy
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
	GetMetadataStore(name string) (metadata.Store, error)
}

// MetadataServiceRegistrar registers metadata stores for shares.
type MetadataServiceRegistrar interface {
	RegisterStoreForShare(shareName string, store metadata.Store) error
}

// MetadataServiceDeregistrar deregisters a metadata store for a share. The
// concrete *metadata.Service satisfies it. AddShare's defensive
// finalize-failure path uses it to avoid leaking a metadata registration for a
// share it refuses to finalize.
type MetadataServiceDeregistrar interface {
	RemoveStoreForShare(shareName string)
}

// MetadataWritebackSetter opts a share into the metadata writeback tier (#1757).
// The concrete *metadata.Service satisfies it. AddShare calls it after
// registering the store when the share's local config sets "writeback": true.
type MetadataWritebackSetter interface {
	SetShareWriteback(shareName string, writeback bool)
}

// MetadataDurableExtentSetter gives the metadata service a way to ask how far a
// payload's bytes are on stable storage, so a committed file size never runs
// ahead of the data it describes. Only the control plane can answer: it owns
// both the metadata store and the per-share block store. The concrete
// *metadata.Service satisfies it.
type MetadataDurableExtentSetter interface {
	SetDurableExtentResolver(fn metadata.DurableExtentFunc)
}

// ShareStore is the narrow subset of pkg/controlplane/store.ShareStore that
// DisableShare / EnableShare need. Defined here so callers can pass any store
// that satisfies it (the concrete GORMStore does) without importing the
// `store` package from this subtree and creating a cycle.
type ShareStore interface {
	GetShare(ctx context.Context, name string) (*models.Share, error)
	UpdateShare(ctx context.Context, share *models.Share) error
}

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

	// authInvalidateCallbacks fire when a share's permission state changes
	// (grant/revoke, default-permission, squash). They are kept separate from
	// changeCallbacks so a permission edit does not trigger the heavier
	// share-set listeners (pseudo-fs rebuild, NFSv4 delegation recall).
	authInvalidateCallbacks map[int]func()

	// metricsRec is the inline metrics recorder threaded into every per-share
	// block store's eviction/backpressure path. Shares are constructed before
	// the metrics registry exists, so the runtime installs it post-startup via
	// SetMetrics (which back-fills already-registered shares); createBlockStore-
	// ForShare applies it to shares added later. Guarded by mu. Nil disables
	// inline recording.
	metricsRec local.MetricsRecorder

	// rebindMu serializes RebindShareBlockStore calls. A rebind tears down and
	// rebuilds a share's per-share BlockStore over the same local dir, which is
	// not safe to overlap with another rebind of the same share. Rebinds are
	// rare (operator-initiated binding changes), so a single coarse mutex across
	// all shares is acceptable and keeps the teardown/rebuild strictly ordered.
	rebindMu sync.Mutex

	// warmJobs tracks in-flight/completed async share-warm jobs (one active per
	// share). Self-guarded; runs warm runs on detached contexts cancelled on
	// RemoveShare. See warm.go.
	warmJobs *warmRegistry

	// blockStoreCache is a lock-free mirror of registry[name].BlockStore keyed by
	// share name. Every NFS/SMB data op resolves the per-share store here without
	// touching s.mu, so the read/write hot path pays no reader-lock contention.
	// It is written only under s.mu at the three points that change a share's
	// store — AddShare (publish), RemoveShare (delete), RebindShareBlockStore
	// (swap) — so it never lags the registry at a lock-release boundary. A miss
	// falls through to the locked registry, which reproduces the exact
	// not-found/no-store errors, so the cache holds only non-nil stores.
	blockStoreCache sync.Map // shareName -> *engine.Store
}

func New() *Service {
	return &Service{
		registry:                make(map[string]*Share),
		reservations:            make(map[string]struct{}),
		remoteStores:            make(map[string]*sharedRemote),
		changeCallbacks:         make(map[int]func(shares []string)),
		authInvalidateCallbacks: make(map[int]func()),
		warmJobs:                newWarmRegistry(),
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
func (s *Service) GetShare(name string) (*Share, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	// Return a snapshot, not the live registry pointer: UpdateShare /
	// SetShare* mutate the stored *Share under s.mu, so handing out the live
	// pointer lets a caller race those writes (torn read of ReadOnly, Squash,
	// Enabled, …) once the RLock is released. Copying every scalar field under
	// the lock makes the read atomic. Pointer/slice fields (BlockStore,
	// RootHandle) are shared by design — BlockStore is independently
	// thread-safe and the slices are never mutated in place after AddShare.
	snapshot := *share
	return &snapshot, nil
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

// SetMetrics installs the inline metrics recorder and back-fills every
// already-registered share's block store with it. Called once by the runtime
// after the metrics registry is built (which happens after startup share
// loading). Shares added afterwards pick the recorder up in
// createBlockStoreForShare. Idempotent and nil-tolerant.
func (s *Service) SetMetrics(rec local.MetricsRecorder) {
	s.mu.Lock()
	s.metricsRec = rec
	stores := make([]*engine.Store, 0, len(s.registry))
	for _, sh := range s.registry {
		if sh.BlockStore != nil {
			stores = append(stores, sh.BlockStore)
		}
	}
	s.mu.Unlock()
	// Apply outside the lock — SetMetrics only swaps an atomic pointer in the
	// store, but keeping store calls off s.mu avoids any future lock-ordering
	// surprise.
	for _, bs := range stores {
		bs.SetMetrics(rec)
	}
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

// OnAuthCacheInvalidate registers a callback fired when a share's permission
// state changes (grant/revoke, default-permission, squash) so adapters can drop
// any cached per-identity authorization. It returns an unsubscribe function that
// removes the callback; callers should invoke it (e.g. in their Stop method) to
// prevent stale callbacks from accumulating across adapter restarts.
func (s *Service) OnAuthCacheInvalidate(callback func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextCallbackID
	s.nextCallbackID++
	s.authInvalidateCallbacks[id] = callback
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.authInvalidateCallbacks, id)
	}
}

// InvalidateAuthCache fires the registered auth-cache-invalidate callbacks. It
// must NOT be called while holding s.mu.
func (s *Service) InvalidateAuthCache() {
	s.mu.RLock()
	callbacks := make([]func(), 0, len(s.authInvalidateCallbacks))
	for _, cb := range s.authInvalidateCallbacks {
		callbacks = append(callbacks, cb)
	}
	s.mu.RUnlock()

	for _, cb := range callbacks {
		cb()
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

// XattrStreamReader returns a metadata.StreamContentReader that materialises a
// named-stream child's content as an xattr value via the per-share block store.
// It is wired into metadata.Service.SetXattrStreamReader so the unified xattr
// resolver can surface stream-entity-backed xattrs (the read half of
// cross-protocol parity for SMB-created named streams). The metadata layer
// stays block-engine-agnostic; this wrapper is the single seam where xattr
// resolution reaches the block store (GetBlockStoreForHandle + ReadAt).
//
// A zero-size stream yields an empty value. Streams large enough to exceed an
// int slice are rejected; named-stream xattr values are expected to be small.
func (s *Service) XattrStreamReader() metadata.StreamContentReader {
	return func(ctx context.Context, streamHandle metadata.FileHandle, attr *metadata.FileAttr) ([]byte, error) {
		if attr == nil || attr.Size == 0 || attr.PayloadID == "" {
			return []byte{}, nil
		}
		if attr.Size > math.MaxInt32 {
			return nil, fmt.Errorf("stream-backed xattr too large: %d bytes", attr.Size)
		}
		bs, err := s.GetBlockStoreForHandle(ctx, streamHandle)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, int(attr.Size))
		n, rerr := bs.ReadAt(ctx, string(attr.PayloadID), buf, 0)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return nil, rerr
		}
		return buf[:n], nil
	}
}
