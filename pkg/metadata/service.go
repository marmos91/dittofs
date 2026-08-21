package metadata

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// DefaultLockGracePeriod is the fallback lock-manager grace period applied when
// no duration is configured. Mirrors the conventional NLM/NFSv4 grace window.
const DefaultLockGracePeriod = 90 * time.Second

// Service provides all metadata operations for the filesystem.
//
// It manages metadata stores and routes operations to the correct store
// based on share name. All protocol handlers should interact with Service
// rather than accessing stores directly.
//
// File Locking:
// Service owns one LockManager per share for byte-range locking (SMB/NLM).
// Locks are ephemeral (in-memory only) and lost on server restart.
// This is separate from metadata stores which handle persistent data.
//
// Usage:
//
//	metaSvc := metadata.New()
//	metaSvc.RegisterStoreForShare("/export", memoryStore)
//	metaSvc.RegisterStoreForShare("/archive", badgerStore)
//
//	// High-level operations (with business logic)
//	file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
//
//	// Low-level operations (direct store access)
//	file, err := metaSvc.GetFile(ctx, handle)
type Service struct {
	// registry holds all per-share state (stores, lock managers, unified views,
	// dir-change notifiers, quotas, writeback flags) behind its own mutex. See
	// shareRegistry for why those fields share one lock.
	registry shareRegistry

	mu sync.RWMutex

	pendingWrites  *PendingWritesTracker // deferred metadata commits for performance
	dirTimes       *DirTimesTracker      // coalesced directory mtime/ctime/atime bumps (#1573)
	deferredCommit atomic.Bool           // if true, use deferred commits (default: true); read lock-free on the write hot path
	// durableExtent answers how far a payload's bytes are on stable storage in
	// its share's block store. Installed by the control plane, which owns both
	// tiers; nil wherever there is no block store to ask.
	durableExtent atomic.Pointer[DurableExtentFunc]

	// parentLinkShards is a fixed bank of mutexes that serialize the parent
	// directory link-count read-modify-write (the ".." bump) done by mkdir (+1),
	// rmdir (-1), and cross-parent directory rename (-1 source, +1 destination).
	// That counter is the one shared key in an otherwise per-child-disjoint
	// namespace transaction; racing it made BadgerDB SSI abort the losers, and
	// under load the abort exhausted the retry budget and escaped as ErrConflict —
	// surfaced to SMB clients as a hard "mkdir failed" (#1571). Serializing the
	// counter keeps it exact and atomic with the child write while file creates
	// (which never touch it) stay fully concurrent. A fixed bank (rather than a
	// per-parent map) bounds memory: distinct parents may hash to one shard —
	// harmless extra serialization on a rare path, never an unbounded map.
	parentLinkShards [parentLinkShardCount]sync.Mutex

	// createNameShards serializes concurrent creation of the same (parent, name)
	// entry. The in-transaction existence recheck in the create path is only
	// atomic on a store that aborts read-write conflicts (BadgerDB SSI); a store
	// running the create transaction at READ COMMITTED (PostgreSQL) lets two
	// racers both read the name as absent and both commit, so the SetChild upsert
	// re-links the last writer and orphans the loser's inode — surfacing as
	// several successful exclusive creates of one name. This bank closes that
	// window uniformly. See lockCreateName.
	createNameShards [parentLinkShardCount]sync.Mutex

	cookies *CookieManager // NFS/SMB cookie to store token translation

	// identityQuotas holds hot-updatable per-user / per-group quota limits,
	// loaded from the control-plane DB and consulted on the write/create hot
	// path. Has its own mutex (does not contend with s.mu).
	identityQuotas *quotaLimits

	// quotaGracePersist, if set, is invoked when the enforcer starts or clears a
	// grace timer so the control-plane row's grace_started_at can be persisted.
	// A zero time clears the timer. Registered via SetQuotaGracePersister.
	quotaGracePersist QuotaGracePersister

	// trashPolicy, if set, supplies the per-share recycle-bin policy consulted
	// on delete. Nil (the default) disables trash entirely: deletes destroy
	// content as before. Installed via SetTrashPolicy.
	trashPolicy TrashPolicy

	// xattrStreamReader, if set, reads the content of a named-stream child File
	// so the xattr resolver can surface stream-backed xattr values (the
	// stream-entity backing). It is wired by the runtime layer, which has
	// block-store access (GetBlockStoreForHandle + engine.Store.ReadAt); the
	// metadata Service stays block-engine-agnostic. Nil (the default) leaves
	// stream NAMES enumerable via ListXattr but makes GetXattr report a
	// stream-only name as absent. Installed via SetXattrStreamReader.
	xattrStreamReader StreamContentReader
}

// GraceCoordinator couples the lock-manager grace period with another grace
// machine (the NFSv4 StateManager). When a share recovers persisted locks at
// registration the lock manager enters grace and OnLockGraceStart fires; when
// that grace window ends (timer, early-exit, or sweep) OnLockGraceEnd fires.
// Implementations must be safe for concurrent use and must not block.
type GraceCoordinator interface {
	// OnLockGraceStart is called when a share's lock-manager grace period begins.
	// expectedClients are the client IDs recovered from persisted locks.
	OnLockGraceStart(expectedClients []string)

	// OnLockGraceEnd is called when a share's lock-manager grace period ends.
	OnLockGraceEnd()
}

// New creates a new empty MetadataService instance.
// Use RegisterStoreForShare to configure stores for each share.
// By default, deferred commits are enabled for better write performance.
func New() *Service {
	s := &Service{
		registry:       newShareRegistry(),
		pendingWrites:  NewPendingWritesTracker(),
		dirTimes:       NewDirTimesTracker(),
		cookies:        NewCookieManager(),
		identityQuotas: newQuotaLimits(),
	}
	s.deferredCommit.Store(true) // Enable deferred commits by default
	return s
}

// QuotaGracePersister persists a per-identity quota's grace timer transition
// back to the control-plane store. t is the new grace_started_at (zero clears
// it). Implementations must be safe for concurrent use and should not block the
// caller for long (the write/create hot path invokes this when grace state
// changes).
type QuotaGracePersister interface {
	PersistQuotaGrace(shareName string, scope QuotaScope, id uint32, t time.Time)

	// PersistDefaultUserGrace durably records (zero t reaps) the per-real-user
	// grace timer for a default-user quota fallback. uid is the REAL uid (not the
	// DefaultUserID sentinel). Unlike an explicit quota — whose grace lives on its
	// own row — a default-user quota is a single shared template row that cannot
	// hold per-user grace, so the timer is stored in a side table keyed by
	// (share, uid). Persisting it makes default-user soft→grace→hard enforcement
	// survive a restart. Same best-effort, non-blocking contract as
	// PersistQuotaGrace.
	PersistDefaultUserGrace(shareName string, uid uint32, t time.Time)
}

// SetQuotaGracePersister installs the grace-timer persistence hook.
func (s *Service) SetQuotaGracePersister(p QuotaGracePersister) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaGracePersist = p
}

// SetIdentityQuota installs or replaces a single per-identity quota for a share.
func (s *Service) SetIdentityQuota(shareName string, iq IdentityQuota) {
	s.identityQuotas.set(shareName, iq)
}

// RemoveIdentityQuota deletes a single per-identity quota for a share.
func (s *Service) RemoveIdentityQuota(shareName string, scope QuotaScope, id uint32) {
	s.identityQuotas.remove(shareName, scope, id)
}

// ReplaceIdentityQuotas atomically replaces all per-identity quotas for a share.
func (s *Service) ReplaceIdentityQuotas(shareName string, quotas []IdentityQuota) {
	s.identityQuotas.replaceShare(shareName, quotas)
}

// SeedDefaultUserGrace restores the durable per-real-user default-user grace
// timers for a share (keyed by real uid), replacing any existing ephemeral
// entries. Called at share load so default-user soft→grace→hard enforcement
// survives a restart.
func (s *Service) SeedDefaultUserGrace(shareName string, byUID map[uint32]time.Time) {
	s.identityQuotas.seedDynGrace(shareName, byUID)
}

// ClearDefaultUserGrace drops the in-memory per-real-user default-user grace
// timer for a single uid on a share. Called when an explicit user quota is
// removed and the uid reverts to the default-user fallback, so it does not
// inherit a stale (possibly already-expired) grace window left over from before
// the explicit quota was installed. The caller reaps the durable side-table row
// separately.
func (s *Service) ClearDefaultUserGrace(shareName string, uid uint32) {
	s.identityQuotas.setDynGrace(shareName, QuotaScopeUser, uid, time.Time{})
}

// GetIdentityQuota returns the exact-keyed quota for (scope,id) on a share.
func (s *Service) GetIdentityQuota(shareName string, scope QuotaScope, id uint32) (IdentityQuota, bool) {
	return s.identityQuotas.get(shareName, scope, id)
}

// ListIdentityQuotas returns a snapshot of every configured per-identity quota
// across all shares. Intended for observability (metrics); the result is
// bounded by the number of explicitly-configured quota principals.
func (s *Service) ListIdentityQuotas() []ConfiguredQuota {
	return s.identityQuotas.snapshot()
}

// SetDeferredCommit enables or disables deferred metadata commits.
// When enabled, CommitWrite batches updates until FlushPendingWriteForFile is called.
// This significantly improves write performance for sequential workloads.
func (s *Service) SetDeferredCommit(enabled bool) {
	s.deferredCommit.Store(enabled)
}

// SetTrashPolicy installs the per-share recycle-bin policy. A nil policy
// (the default) disables trash: deletes destroy content as before.
func (s *Service) SetTrashPolicy(p TrashPolicy) { s.trashPolicy = p }

// DurableExtentFunc reports how far a payload's bytes are on stable storage in
// the share's block store: bytes below the returned offset survive an unclean
// shutdown, bytes above it do not. ok is false when the block store cannot
// answer, which means "unknown" and never "nothing is durable".
type DurableExtentFunc func(shareName string, payloadID PayloadID) (int64, bool)

// SetDurableExtentResolver installs the block-store lookup flushPendingWrite
// uses to keep a committed file size from describing bytes that are not durable
// yet. Without it (unit tests, stores with no block tier) size commits behave as
// they always have.
func (s *Service) SetDurableExtentResolver(fn DurableExtentFunc) {
	s.durableExtent.Store(&fn)
}

// durableExtentFor asks the installed resolver how far the payload is durable;
// with no resolver the answer is "unknown".
func (s *Service) durableExtentFor(shareName string, payloadID PayloadID) (int64, bool) {
	if fn := s.durableExtent.Load(); fn != nil && *fn != nil {
		return (*fn)(shareName, payloadID)
	}
	return 0, false
}

// storeForHandle returns the appropriate store for a file handle.
// It extracts the share name from the handle and looks up the store.
//
// A malformed handle propagates DecodeFileHandle's ErrInvalidHandle
// StoreError; a well-formed handle naming an unknown share propagates
// GetStoreForShare's ErrStaleHandle StoreError. Both are *StoreError so the
// protocol error mappers classify them as BADHANDLE/STALE.
func (s *Service) storeForHandle(handle FileHandle) (Store, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}

	return s.GetStoreForShare(shareName)
}

// shareNameForHandle extracts the share name from a file handle.
// Returns empty string if the handle is invalid.
func shareNameForHandle(handle FileHandle) string {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return ""
	}
	return shareName
}

// lockManagerForHandle returns the lock manager for the share that owns the handle.
func (s *Service) lockManagerForHandle(handle FileHandle) (*LockManager, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}
	return s.lockManagerForShare(shareName)
}

// storeAndLockManagerForHandle resolves both the metadata store and the lock
// manager for a handle with a SINGLE DecodeFileHandle call. The store and
// lock-manager handlers (LockFile, UnlockFile, TestLock, …) need both, and
// calling storeForHandle + lockManagerForHandle separately parsed the same
// handle's UUID twice per operation. The share name is decoded once here and
// reused for both share-keyed lookups. Handle opacity is preserved: callers
// still pass an opaque FileHandle and never see the decoded components.
func (s *Service) storeAndLockManagerForHandle(handle FileHandle) (Store, *LockManager, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, nil, err
	}
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, nil, err
	}
	lm, err := s.lockManagerForShare(shareName)
	if err != nil {
		return nil, nil, err
	}
	return store, lm, nil
}

// GetLockManagerForHandle returns the lock manager for the share that owns
// the given handle. Returns an error if the handle is malformed or no lock
// manager exists for the share.
//
// Used by the SMB blocking-lock async-park path (issue #430): the handler
// needs the conflicting holders' OwnerIDs to feed the Wait-For Graph for
// deadlock detection, which requires direct access to the share's
// LockManager.ListLocks. Permission checks are not needed here — this is
// pure conflict-discovery, not a lock-state mutation.
//
// Thread safety: Safe to call concurrently.
func (s *Service) GetLockManagerForHandle(handle FileHandle) (*LockManager, error) {
	return s.lockManagerForHandle(handle)
}

// GetFile retrieves file metadata by handle.
// This is a convenience method that calls GetFile from the Base interface.
// When deferred commits are enabled, it merges pending write state (size, mtime, ctime)
// with the stored file metadata.
func (s *Service) GetFile(ctx context.Context, handle FileHandle) (*File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		return nil, err
	}

	// A misbehaving store may return (nil, nil); tolerate it (some callers, e.g.
	// the NFSv3 WCC re-fetch, rely on GetFile not panicking on a nil re-fetch).
	if file != nil {
		s.mergePendingWrites(handle, file)
		s.mergeDirTimes(handle, &file.FileAttr)
	}
	return file, nil
}

// fileForReadStore is the optional read fast path — a GetFile that skips
// deriving File.Path. Implemented only by the badger backend; other backends
// fall through to the regular GetFile in GetFileForRead.
type fileForReadStore interface {
	GetFileForRead(ctx context.Context, handle FileHandle) (*File, error)
}

// GetFileForRead loads a file for the handle-addressed hot paths (NFS
// READ/WRITE/GETATTR) that never read File.Path. When the backend implements
// fileForReadStore it skips the derivePath parent-edge walk; otherwise it
// falls back to GetFile. Pending write state is merged identically to GetFile.
func (s *Service) GetFileForRead(ctx context.Context, handle FileHandle) (*File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

	var file *File
	if r, ok := store.(fileForReadStore); ok {
		file, err = r.GetFileForRead(ctx, handle)
	} else {
		file, err = store.GetFile(ctx, handle)
	}
	if err != nil {
		return nil, err
	}

	// A misbehaving store may return (nil, nil); tolerate it (some callers, e.g.
	// the NFSv3 WCC re-fetch, rely on GetFile not panicking on a nil re-fetch).
	if file != nil {
		s.mergePendingWrites(handle, file)
		s.mergeDirTimes(handle, &file.FileAttr)
	}
	return file, nil
}

// mergePendingWrites overlays deferred-commit state (size, mtime/ctime,
// setuid/setgid clearing) onto a freshly loaded file, so a read sees its own
// not-yet-persisted writes.
func (s *Service) mergePendingWrites(handle FileHandle, file *File) {
	pending, ok := s.pendingWrites.GetPending(handle)
	if !ok {
		return
	}
	if pending.MaxSize > file.Size {
		file.Size = pending.MaxSize
	}
	if pending.LastMtime.After(file.Mtime) {
		file.Mtime = pending.LastMtime
		file.Ctime = pending.LastMtime
	}
	if pending.ClearSetuidSetgid {
		file.Mode &= ^uint32(0o6000)
	}
}

// parentLinkShardCount is the size of the parentLinkShards bank. 256 is ample:
// the lock is held only for a rare namespace op (mkdir/rmdir/dir-rename), so
// collisions between two distinct parents cost at most a little extra
// serialization, never correctness.
const parentLinkShardCount = 256

// parentLinkShard maps a handle key to its shard via FNV-1a. Inlined to avoid a
// hash/fnv allocation on this hot-ish namespace path.
func parentLinkShard(key string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return h % parentLinkShardCount
}

// lockParentLink locks the shard guarding handle's parent link-count
// read-modify-write (the ".." bump done by mkdir/rmdir) and returns its unlock;
// callers defer it. See the parentLinkShards field for why this exists (#1571).
func (s *Service) lockParentLink(handle FileHandle) func() {
	mu := &s.parentLinkShards[parentLinkShard(handleKey(handle))]
	mu.Lock()
	return mu.Unlock
}

// lockParentLinks locks the shards for a directory rename's two parents (source
// and destination) and returns a single unlock. Shards are taken in index order
// so opposite-direction renames can't invert and deadlock; if both parents map
// to the same shard it is locked once (the bank's mutexes are not reentrant).
func (s *Service) lockParentLinks(a, b FileHandle) func() {
	ia := parentLinkShard(handleKey(a))
	ib := parentLinkShard(handleKey(b))
	if ia == ib {
		mu := &s.parentLinkShards[ia]
		mu.Lock()
		return mu.Unlock
	}
	if ia > ib {
		ia, ib = ib, ia
	}
	lo, hi := &s.parentLinkShards[ia], &s.parentLinkShards[ib]
	lo.Lock()
	hi.Lock()
	return func() { hi.Unlock(); lo.Unlock() }
}

// createNameShard maps a (parent, name) pair to a shard via FNV-1a over the
// parent handle key, a separator byte, and the name — computed without
// allocating the concatenated string. Uses the same FNV constants as
// parentLinkShard so the distribution is identical.
func createNameShard(parentHandle FileHandle, name string) uint32 {
	key := handleKey(parentHandle)
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	h *= 16777619 // separator byte 0x00: XOR is a no-op, only the FNV mix applies
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	return h % parentLinkShardCount
}

// lockCreateName serializes creation of one (parent, name) so two concurrent
// creates of the same name in the same directory cannot both pass the
// in-transaction existence recheck and each insert a distinct inode. Creates of
// different names hash to (likely) different shards and stay fully concurrent.
// See the createNameShards field for why the recheck alone is insufficient on a
// READ COMMITTED store. The returned unlock is released right after the create
// transaction commits (covering the recheck and commit), not at function end.
func (s *Service) lockCreateName(parentHandle FileHandle, name string) func() {
	mu := &s.createNameShards[createNameShard(parentHandle, name)]
	mu.Lock()
	return mu.Unlock
}

// mergeDirTimes overlays a directory's coalesced (not-yet-persisted) mtime/
// ctime/atime bumps onto a freshly loaded directory's attributes, so a read
// always sees the latest create/remove timestamp even though the parent inode
// write was deferred out of the hot transaction (#1573). No-op for a nil attr
// or a non-directory. Takes *FileAttr so it also serves READDIR entry attrs,
// not just whole *File reads.
func (s *Service) mergeDirTimes(handle FileHandle, attr *FileAttr) {
	if attr == nil || attr.Type != FileTypeDirectory {
		return
	}
	if mtime, ctime, atime, ok := s.dirTimes.GetPending(handle); ok {
		applyDirTimes(attr, mtime, ctime, atime)
	}
}

// applyDirTimes overlays coalesced mtime/ctime/atime onto attr (latest wins per
// field) and reports whether any field advanced. Shared by the read overlay
// (mergeDirTimes) and the durable flush (#1573).
func applyDirTimes(attr *FileAttr, mtime, ctime, atime time.Time) (changed bool) {
	if mtime.After(attr.Mtime) {
		attr.Mtime = mtime
		changed = true
	}
	if ctime.After(attr.Ctime) {
		attr.Ctime = ctime
		changed = true
	}
	if atime.After(attr.Atime) {
		attr.Atime = atime
		changed = true
	}
	return changed
}

// recordDirTimes coalesces a directory-timestamp bump from a create/remove and
// triggers a durable flush when the flush interval has elapsed. Called after
// the mutating transaction commits: on a crash between commit and this call the
// child is durable and only the directory's timestamp bump is lost.
func (s *Service) recordDirTimes(ctx context.Context, dirHandle FileHandle, t time.Time) {
	if s.dirTimes.RecordBump(dirHandle, t) {
		s.flushDirTimes(ctx, dirHandle)
	}
}

// flushDirTimes durably persists a directory's coalesced timestamps in a
// dedicated transaction, serialized per-directory. Because create/remove no
// longer touch the parent inode, this write never conflicts with concurrent
// same-dir mutations. Best-effort: a failure (e.g. the directory was removed)
// leaves the bump to be retried or dropped, never failing the caller.
func (s *Service) flushDirTimes(ctx context.Context, dirHandle FileHandle) {
	lock := s.dirTimes.FlushLock(dirHandle)
	lock.Lock()
	defer lock.Unlock()

	mtime, ctime, atime, ok := s.dirTimes.GetPending(dirHandle)
	if !ok {
		return // already flushed by a concurrent caller
	}

	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return
	}

	err = store.WithTransaction(ctx, func(tx Transaction) error {
		dir, gErr := tx.GetFile(ctx, dirHandle)
		if gErr != nil {
			return gErr
		}
		if dir == nil {
			return nil // dir was concurrently removed or handle is stale; nothing to flush
		}
		if dir.Type != FileTypeDirectory {
			return nil
		}
		if !applyDirTimes(&dir.FileAttr, mtime, ctime, atime) {
			return nil
		}
		return tx.UpdateAttrs(ctx, dir)
	})
	if err != nil {
		return // leave pending in place; a later bump/flush will retry
	}
	s.dirTimes.ClearIfFlushed(dirHandle, mtime)
}

// GetFileCached returns file metadata, trying the pending-writes cache first
// to avoid a BadgerDB read. Used on the COMMIT path where WRITE has already
// validated and cached the file. Falls back to the full GetFile path if there
// is no cached entry (e.g., COMMIT without prior WRITE, or cache evicted).
func (s *Service) GetFileCached(ctx context.Context, handle FileHandle) (*File, error) {
	if cached := s.pendingWrites.GetCachedFile(handle); cached != nil {
		// Merge pending state into the cached copy (same logic as GetFile)
		if pending, ok := s.pendingWrites.GetPending(handle); ok {
			if pending.MaxSize > cached.Size {
				cached.Size = pending.MaxSize
			}
			if pending.LastMtime.After(cached.Mtime) {
				cached.Mtime = pending.LastMtime
				cached.Ctime = pending.LastMtime
			}
			if pending.ClearSetuidSetgid {
				cached.Mode &= ^uint32(0o6000)
			}
		}
		return cached, nil
	}
	return s.GetFile(ctx, handle)
}

// CheckPermissions performs file-level permission checking.
// Returns granted permissions (subset of requested).
//
// This implements Unix-style permission checking:
//   - Root (UID 0): Bypass all checks except on read-only shares
//   - Owner: Check owner permission bits
//   - Group member: Check group permission bits
//   - Other: Check other permission bits
//   - Anonymous: Only world permissions
func (s *Service) CheckPermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error) {
	return s.checkFilePermissions(ctx, handle, requested)
}

// GetChild retrieves a child's handle from a directory.
func (s *Service) GetChild(ctx context.Context, dirHandle FileHandle, name string) (FileHandle, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}
	return store.GetChild(ctx, dirHandle, name)
}

// GetRootHandle returns the root handle for a share.
func (s *Service) GetRootHandle(ctx context.Context, shareName string) (FileHandle, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GetRootHandle(ctx, shareName)
}

// GenerateHandle generates a new file handle for a path.
func (s *Service) GenerateHandle(ctx context.Context, shareName, path string) (FileHandle, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GenerateHandle(ctx, shareName, path)
}

// GetFilesystemStatistics returns filesystem statistics.
// When a quota is configured for the share, the returned TotalBytes and
// AvailableBytes are overlaid with quota-adjusted values. This convenience
// form has no caller identity, so per-user/per-group quotas are not reflected;
// use GetFilesystemStatisticsForIdentity from protocol FSSTAT handlers.
func (s *Service) GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error) {
	return s.GetFilesystemStatisticsForIdentity(ctx, handle, nil)
}

// GetFilesystemStatisticsForIdentity returns filesystem statistics with the
// quota overlay narrowed to the smallest applicable quota: the per-share quota
// AND (when identity is non-nil and a per-user/per-group quota applies) the
// caller's identity quota. This is what `df` / FSSTAT / SMB FS_FULL_SIZE report,
// so a quota'd user sees their own ceiling rather than the raw volume.
func (s *Service) GetFilesystemStatisticsForIdentity(ctx context.Context, handle FileHandle, identity *Identity) (*FilesystemStatistics, error) {
	// Decode once: the share name is reused for the quota overlay below.
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	stats, err := store.GetFilesystemStatistics(ctx, handle)
	if err != nil {
		return nil, err
	}

	// Apply per-share quota overlay if configured.
	if quotaBytes := s.GetQuotaForShare(shareName); quotaBytes > 0 {
		applyByteQuotaOverlay(stats, uint64(quotaBytes), stats.UsedBytes)
	}

	// Apply the tighter of the caller's user / group quota, if any.
	s.applyIdentityQuotaOverlay(shareName, store, stats, identity)

	return stats, nil
}

// applyByteQuotaOverlay narrows TotalBytes/AvailableBytes to a byte ceiling
// against the given used value. Only narrows (never widens) Total so the
// smallest applicable quota wins across successive overlays.
func applyByteQuotaOverlay(stats *FilesystemStatistics, ceiling, used uint64) {
	if ceiling == 0 {
		return
	}
	if stats.TotalBytes == 0 || ceiling < stats.TotalBytes {
		stats.TotalBytes = ceiling
	}
	var avail uint64
	if used < ceiling {
		avail = ceiling - used
	}
	if avail < stats.AvailableBytes {
		stats.AvailableBytes = avail
	}
}

// applyFileQuotaOverlay narrows TotalFiles/AvailableFiles to an inode ceiling.
func applyFileQuotaOverlay(stats *FilesystemStatistics, ceiling, used uint64) {
	if ceiling == 0 {
		return
	}
	if stats.TotalFiles == 0 || ceiling < stats.TotalFiles {
		stats.TotalFiles = ceiling
	}
	var avail uint64
	if used < ceiling {
		avail = ceiling - used
	}
	if avail < stats.AvailableFiles {
		stats.AvailableFiles = avail
	}
}

// applyIdentityQuotaOverlay narrows the stats to the caller's per-user and
// per-group quota (bytes + inodes), using that identity's live usage rather
// than the share-wide used total. No-op when identity is nil or has no UID, or
// when no quota applies.
func (s *Service) applyIdentityQuotaOverlay(shareName string, store Store, stats *FilesystemStatistics, identity *Identity) {
	if identity == nil || identity.UID == nil || !s.identityQuotas.hasAny(shareName) {
		return
	}
	uid := *identity.UID

	if iq, ok := s.identityQuotas.resolveUser(shareName, uid); ok {
		if usage, err := store.GetQuotaUsage(shareName, QuotaScopeUser, uid); err == nil {
			if iq.LimitBytes > 0 {
				applyByteQuotaOverlay(stats, uint64(iq.LimitBytes), uint64(max64(usage.Bytes, 0)))
			}
			if iq.LimitFiles > 0 {
				applyFileQuotaOverlay(stats, uint64(iq.LimitFiles), uint64(max64(usage.Files, 0)))
			}
		}
	}
	if identity.GID != nil {
		gid := *identity.GID
		if iq, ok := s.identityQuotas.get(shareName, QuotaScopeGroup, gid); ok {
			if usage, err := store.GetQuotaUsage(shareName, QuotaScopeGroup, gid); err == nil {
				if iq.LimitBytes > 0 {
					applyByteQuotaOverlay(stats, uint64(iq.LimitBytes), uint64(max64(usage.Bytes, 0)))
				}
				if iq.LimitFiles > 0 {
					applyFileQuotaOverlay(stats, uint64(iq.LimitFiles), uint64(max64(usage.Files, 0)))
				}
			}
		}
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// GetFilesystemCapabilities returns filesystem capabilities.
func (s *Service) GetFilesystemCapabilities(ctx context.Context, handle FileHandle) (*FilesystemCapabilities, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	return store.GetFilesystemCapabilities(ctx, handle)
}

// CheckLockForIO checks if an I/O operation is blocked by locks.
//
// This is a lightweight operation that doesn't verify file existence,
// allowing fast path for I/O operations.
// openID identifies the specific open performing the I/O (empty string falls back to sessionID).
func (s *Service) CheckLockForIO(ctx context.Context, handle FileHandle, openID string, sessionID uint64, offset, length uint64, isWrite bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	handleKey := string(handle)
	conflict := lm.CheckForIO(handleKey, openID, sessionID, offset, length, isWrite)
	if conflict != nil {
		return NewLockedError("", conflict)
	}
	return nil
}

// LockFile acquires a byte-range lock on a file.
//
// Business logic:
//   - Verifies file exists
//   - Verifies file is not a directory (directories cannot be locked)
//   - Checks user has appropriate permission (read for shared, write for exclusive)
func (s *Service) LockFile(ctx *AuthContext, handle FileHandle, lock FileLock) error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// Verify file exists and is not a directory
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	if file.Type == FileTypeDirectory {
		return NewIsDirectoryError("")
	}

	// Check permissions
	var requiredPerm Permission
	if lock.Exclusive {
		requiredPerm = PermissionWrite
	} else {
		requiredPerm = PermissionRead
	}

	// Route through the shared permission funnel rather than calling
	// calculatePermissions directly: checkFilePermissions applies the per-user
	// read-only ceiling (#1276 — a read-only user must not take an exclusive
	// write lock) and the SMB handle-based write authorization, keeping lock
	// authorization consistent with every other write path.
	granted, err := s.checkFilePermissions(ctx, handle, requiredPerm)
	if err != nil {
		return err
	}
	if granted&requiredPerm == 0 {
		return NewPermissionDeniedError("")
	}

	// Acquire the lock via LockManager
	handleKey := string(handle)
	return lm.Lock(handleKey, lock)
}

// UnlockFile releases a byte-range lock on a file.
//
// Note: Takes context.Context instead of *AuthContext because:
// - Open/Session ID identifies the lock owner (you can only unlock your own locks)
// - No permission checking needed for unlock operations
// openID identifies the specific open that owns the lock (empty string falls back to sessionID).
func (s *Service) UnlockFile(ctx context.Context, handle FileHandle, openID string, sessionID uint64, offset, length uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// Verify file exists
	_, err = store.GetFile(ctx, handle)
	if err != nil {
		return err
	}

	handleKey := string(handle)
	return lm.Unlock(handleKey, openID, sessionID, offset, length)
}

// UnlockAllForSession releases all locks held by a session on a file.
func (s *Service) UnlockAllForSession(ctx context.Context, handle FileHandle, sessionID uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// No file existence check - file may have been deleted
	handleKey := string(handle)
	lm.UnlockAllForSession(handleKey, sessionID)
	return nil
}

// UnlockAllForOpen releases all locks held by a specific open on a file.
func (s *Service) UnlockAllForOpen(ctx context.Context, handle FileHandle, openID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// No file existence check - file may have been deleted
	handleKey := string(handle)
	lm.UnlockAllForOpen(handleKey, openID)
	return nil
}

// TestLock tests if a lock would conflict with existing locks.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - bool: true if lock would succeed, false if conflict exists
//   - *LockConflict: Details of conflicting lock if bool is false
func (s *Service) TestLock(ctx *AuthContext, handle FileHandle, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict, error) {
	if err := ctx.Context.Err(); err != nil {
		return false, nil, err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return false, nil, err
	}

	// Verify file exists
	_, err = store.GetFile(ctx.Context, handle)
	if err != nil {
		return false, nil, err
	}

	handleKey := string(handle)
	ok, conflict := lm.TestLockByParams(handleKey, sessionID, offset, length, exclusive)
	return ok, conflict, nil
}

// ListLocks lists all locks on a file.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - []FileLock: All active locks on the file (empty slice if none)
func (s *Service) ListLocks(ctx *AuthContext, handle FileHandle) ([]FileLock, error) {
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Verify file exists
	_, err = store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	handleKey := string(handle)
	locks := lm.ListLocks(handleKey)
	if locks == nil {
		return []FileLock{}, nil
	}
	return locks, nil
}

// RemoveFileLocks removes all locks for a file.
// Called when a file is deleted to clean up stale lock entries.
func (s *Service) RemoveFileLocks(handle FileHandle) {
	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return // No lock manager means no locks to remove
	}

	handleKey := string(handle)
	lm.RemoveFileLocks(handleKey)
}

// CreateShare creates a new share with its root directory.
func (s *Service) CreateShare(ctx context.Context, shareName string, share *Share) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}
	return store.CreateShare(ctx, share)
}

// GetShareOptions returns the options for a share.
func (s *Service) GetShareOptions(ctx context.Context, shareName string) (*ShareOptions, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GetShareOptions(ctx, shareName)
}

// notifyDirChange dispatches a directory change notification for a share.
//
// This is fire-and-forget: notifications do NOT affect the success/failure
// of the mutation that triggered them. If the notifier is nil or not
// registered for the share, the call is silently ignored.
//
// The originClientID is extracted from the AuthContext's LockClientID field
// (falling back to ClientAddr) to identify the originating client so their
// own leases aren't broken.
func (s *Service) notifyDirChange(shareName string, parentHandle FileHandle, changeType lock.DirChangeType, ctx *AuthContext) {
	notifier, ok := s.registry.dirChangeNotifier(shareName)

	if !ok || notifier == nil {
		return
	}

	originClient := ""
	var excludeParentKey [16]byte
	var hasExcludeKey bool
	if ctx != nil {
		originClient = ctx.LockClientID
		if originClient == "" {
			originClient = ctx.ClientAddr
		}
		// Thread the originating handle's RqLs ParentLeaseKey into the
		// notifier so the dir-lease parent-key suppression rule (MS-SMB2
		// §3.3.4.20, #470 C2) can skip the matching parent dir lease.
		// NFS callers leave HasParentLeaseKey=false.
		if ctx.HasParentLeaseKey {
			excludeParentKey = ctx.ParentLeaseKey
			hasExcludeKey = true
		}
	}

	// Fire-and-forget: notifier handles dispatch; recover from panics
	defer func() {
		if r := recover(); r != nil {
			logger.Error("notifyDirChange: panic in notifier", "share", shareName, "error", r)
		}
	}()
	notifier.OnDirChange(lock.FileHandle(parentHandle), changeType, originClient, excludeParentKey, hasExcludeKey)
}

// --- per-share registry forwarders -------------------------------------------
// These keep the public Service surface unchanged while the state and the
// invariants that govern it live on shareRegistry.

// SetLockGracePeriod sets the grace period applied to per-share lock managers
// that recover persisted locks at registration. A non-positive duration falls
// back to DefaultLockGracePeriod. Must be called before RegisterStoreForShare
// to affect a given share.
func (s *Service) SetLockGracePeriod(d time.Duration) { s.registry.setGracePeriod(d) }

// SetGraceCoordinator registers the coordinator that couples lock-manager grace
// with the NFSv4 StateManager grace machine.
func (s *Service) SetGraceCoordinator(c GraceCoordinator) { s.registry.setGraceCoordinator(c) }

// SetByteRangeReleaseHook installs the cross-protocol byte-range release
// notification stamped onto every per-share lock manager at creation.
func (s *Service) SetByteRangeReleaseHook(fn func(handleKey string)) {
	s.registry.setByteRangeReleaseHook(fn)
}

// SetShareWriteback marks a share as writeback-tiered.
func (s *Service) SetShareWriteback(shareName string, writeback bool) {
	s.registry.setWriteback(shareName, writeback)
}

// shareWriteback reports whether a share is writeback-tiered.
func (s *Service) shareWriteback(shareName string) bool { return s.registry.writeback(shareName) }

// RegisterStoreForShare registers a metadata store for a share, recovering and
// publishing its lock manager atomically. See shareRegistry.register.
func (s *Service) RegisterStoreForShare(shareName string, store Store) error {
	return s.registry.register(shareName, store)
}

// RemoveStoreForShare deregisters a share and every per-share entry it owns.
// See shareRegistry.remove.
func (s *Service) RemoveStoreForShare(shareName string) { s.registry.remove(shareName) }

// GetStoreForShare returns the metadata store for a specific share.
func (s *Service) GetStoreForShare(shareName string) (Store, error) {
	return s.registry.storeForShare(shareName)
}

// GetLockManagerForShare returns the lock manager for a specific share, or nil
// if the share has none.
//
// Thread safety: Safe to call concurrently.
func (s *Service) GetLockManagerForShare(shareName string) *LockManager {
	return s.registry.lockManagerOrNil(shareName)
}

// GetUnifiedLockView returns the UnifiedLockView for a specific share.
func (s *Service) GetUnifiedLockView(shareName string) *UnifiedLockView {
	return s.registry.unifiedView(shareName)
}

// SetUnifiedLockView installs the UnifiedLockView for a specific share.
func (s *Service) SetUnifiedLockView(shareName string, view *UnifiedLockView) {
	s.registry.setUnifiedView(shareName, view)
}

// SetQuotaForShare sets the byte quota for a share (0 = unlimited).
func (s *Service) SetQuotaForShare(shareName string, quotaBytes int64) {
	s.registry.setQuota(shareName, quotaBytes)
}

// GetQuotaForShare returns the byte quota for a share (0 = unlimited).
func (s *Service) GetQuotaForShare(shareName string) int64 { return s.registry.quota(shareName) }

// SetDirChangeNotifier registers the directory-change notifier for a share.
//
// Thread safety: Safe to call concurrently.
func (s *Service) SetDirChangeNotifier(shareName string, n lock.DirChangeNotifier) {
	s.registry.setDirChangeNotifier(shareName, n)
}

// lockManagerForShare returns the lock manager for an already-decoded share
// name, or a stale-handle error when the share has none.
func (s *Service) lockManagerForShare(shareName string) (*LockManager, error) {
	return s.registry.lockManagerForShare(shareName)
}
