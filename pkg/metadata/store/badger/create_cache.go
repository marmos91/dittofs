package badger

import (
	"context"
	"sync"
	"sync/atomic"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// direntCacheCap bounds the dirent cache (positive + negative entries).
const direntCacheCap = 8192

// direntCache is a lock-free, generation-guarded cache of directory-entry
// lookups keyed by (parentID, name): a positive entry records the child handle,
// a negative entry records ABSENT. It accelerates the *pre-transaction*
// existence check on the create hot path (createEntry / CheckParentCreateAccess)
// and is usable by LOOKUP, so a directory repeatedly probed for the same
// (mostly-absent) names skips the per-probe badger View transaction.
//
// It is a read-only / populate-after-commit cache, NEVER a write-back cache: the
// authoritative c:/cn: dirent keys are always written inside the create
// transaction, and the in-transaction TOCTOU recheck (file_create.go) reads the
// real badger txn — it is NEVER served from here, so it still builds Badger's SSI
// conflict read-set and two concurrent same-name creates still conflict (one
// wins, the loser aborts). Serving that recheck from a cache would let both
// commit and orphan an inode.
//
// Correctness mirrors fileReadCache exactly (single-node badger is single-writer,
// which makes this tractable):
//   - invalidate() runs AFTER a SetChild/DeleteChild commits, bumping gen then
//     deleting, so a reader that observed the pre-commit state cannot leave it
//     cached (its generation-guarded store loses). Without the gen guard a reader
//     that missed, fell through to a not-found badger read, and cached ABSENT
//     while a concurrent create committed that name would pin a permanently-stale
//     negative entry -> spurious ENOENT for a file that exists.
//   - store() writes only when gen is unchanged since the snapshot taken before
//     the backing read; any racing dirent write moves gen and the populate drops.
//     A dropped populate is a cache miss (re-read), never a stale hit.
type direntCache struct {
	m     sync.Map // key string -> direntEntry
	n     atomic.Int64
	gen   atomic.Uint64
	prune atomic.Bool
}

// direntEntry is a cached lookup result. present=false is a negative (ABSENT)
// entry; present=true carries the resolved child handle as a string so the
// entry stays comparable (FileHandle is a []byte slice) for sync.Map's
// CompareAndDelete.
type direntEntry struct {
	handle  string
	present bool
}

// direntKey builds the map key for a (parentID, name) pair. The parent UUID
// string and name are separated by a NUL, which ValidateName forbids inside a
// name, so distinct (parent, name) pairs never collide.
func direntKey(parentID, name string) string {
	return parentID + "\x00" + name
}

// generation snapshots the invalidation counter; pass the result to store.
func (c *direntCache) generation() uint64 { return c.gen.Load() }

func (c *direntCache) get(key string) (direntEntry, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return direntEntry{}, false
	}
	return v.(direntEntry), true
}

// store caches e under key only if no dirent write raced the backing read (the
// generation is unchanged since genAtRead).
func (c *direntCache) store(key string, e direntEntry, genAtRead uint64) {
	if c.gen.Load() != genAtRead {
		return
	}
	if _, loaded := c.m.Swap(key, e); !loaded {
		if c.n.Add(1) > direntCacheCap {
			c.pruneToHalf()
		}
	}
	// The guard and the Swap are not atomic: a dirent write could commit and
	// invalidate() (bump gen + delete) in between, leaving our now-stale entry
	// live. Re-check the generation; if it moved, drop our entry — but only if a
	// newer reader hasn't already replaced it (CompareAndDelete on our value).
	if c.gen.Load() != genAtRead {
		if c.m.CompareAndDelete(key, e) {
			c.n.Add(-1)
		}
	}
}

// invalidate drops key and advances the generation so any in-flight populate for
// a now-superseded value is rejected. MUST be called AFTER the write commits.
// Order matters: bump gen BEFORE delete (see fileReadCache.invalidate).
func (c *direntCache) invalidate(key string) {
	c.gen.Add(1)
	if _, ok := c.m.LoadAndDelete(key); ok {
		c.n.Add(-1)
	}
}

// pruneToHalf best-effort trims the map back toward half the cap on overflow.
func (c *direntCache) pruneToHalf() {
	if !c.prune.CompareAndSwap(false, true) {
		return
	}
	defer c.prune.Store(false)
	target := int64(direntCacheCap / 2)
	c.m.Range(func(k, _ any) bool {
		if c.n.Load() <= target {
			return false
		}
		if _, ok := c.m.LoadAndDelete(k); ok {
			c.n.Add(-1)
		}
		return true
	})
}

// ============================================================================
// Create-path cached store reads
// ============================================================================

// WarmFileReadCache seeds the shared read cache with a just-created File so the
// trailing WRITE/GETATTR/ACCESS on the new handle hit warm instead of
// re-decoding the inode from badger (#1735). Called by createEntry AFTER the
// create transaction commits — so after that commit's own dirtyFiles
// invalidation has already bumped gen — meaning the generation captured here is
// post-commit. A concurrent write to any OTHER file that races this populate
// moves gen and the store is dropped (a miss, never a stale hit); a concurrent
// write to THIS brand-new inode is impossible until createEntry returns the
// handle to the caller. The entry is stored path-less: readCache is shared with
// every handle-addressed reader, which derive Path fresh (#1166), so a baked
// path must never leak in.
func (s *BadgerMetadataStore) WarmFileReadCache(file *metadata.File) {
	if file == nil {
		return
	}
	gen := s.readCache.generation()
	cp := copyForRead(file)
	cp.Path = "" // never cache a path in the shared read cache (#1166)
	s.readCache.store(file.ID.String(), cp, gen)
}

// GetFileForCreate loads the parent directory for a create with File.Path
// populated (createEntry needs it for the PATH_MAX check), backed by the
// dedicated path-carrying parentCache so repeated creates in one directory skip
// the per-create badger View txn + decode + parent-edge path walk (#1735).
//
// Invalidated in lockstep with readCache on the parent's own mutation (parentID
// in dirtyFiles): a chmod/chown/rename of the parent PutFiles it, so the
// security-relevant fields (Mode, ACL, GID, Type) served here are always fresh.
// Only File.Path can lag, and only when an ANCESTOR is renamed (which PutFiles
// the ancestor, not this parent). That lag is benign here: the stale path feeds
// only the soft PATH_MAX guard and newFile.Path — a stored field that is
// #1166-untrusted and re-derived on every read, never served to clients.
// ponytail: parentID-targeted invalidation; ancestor-rename path lag is benign.
func (s *BadgerMetadataStore) GetFileForCreate(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	_, fileID, decErr := metadata.DecodeFileHandle(handle)
	var key string
	if decErr == nil {
		key = fileID.String()
		if cached, ok := s.parentCache.get(key); ok {
			return copyForRead(cached), nil
		}
	}

	// Snapshot the generation BEFORE the backing read so a mutation racing this
	// read cannot leave a stale value cached (store() checks it).
	gen := s.parentCache.generation()
	var result *metadata.File
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		result, err = tx.getFile(ctx, handle, true) // withPath: createEntry needs Path
		return err
	})
	if err != nil {
		return nil, err
	}
	if key != "" {
		s.parentCache.store(key, result, gen)
		return copyForRead(result), nil
	}
	return result, nil
}

// GetChildForCreate resolves a name in a directory for the PRE-transaction
// existence check on the create path (and usable by LOOKUP), backed by the
// dirent cache so a directory repeatedly probed for the same (mostly-absent)
// names skips the per-probe badger View txn (#1735). A cached-ABSENT name
// returns ErrNotFound.
//
// NEVER call this for the in-transaction TOCTOU recheck: that MUST go through the
// transaction-level GetChild so the probe joins Badger's SSI conflict read-set
// (two concurrent same-name creates must conflict — one wins, the loser aborts).
// This store-level helper is advisory only; the in-txn recheck is authoritative.
func (s *BadgerMetadataStore) GetChildForCreate(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	_, dirID, decErr := metadata.DecodeFileHandle(dirHandle)
	var key string
	if decErr == nil {
		key = direntKey(dirID.String(), name)
		if e, ok := s.direntCache.get(key); ok {
			if e.present {
				return metadata.FileHandle(e.handle), nil
			}
			return nil, &metadata.StoreError{Code: metadata.ErrNotFound, Message: "child not found"}
		}
	}

	// Snapshot the generation BEFORE the backing read (store() checks it).
	gen := s.direntCache.generation()
	var result metadata.FileHandle
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var gErr error
		result, gErr = tx.GetChild(ctx, dirHandle, name)
		return gErr
	})
	if err != nil {
		if key != "" && metadata.IsNotFoundError(err) {
			s.direntCache.store(key, direntEntry{present: false}, gen)
		}
		return nil, err
	}
	if key != "" {
		s.direntCache.store(key, direntEntry{handle: string(result), present: true}, gen)
	}
	return result, nil
}
