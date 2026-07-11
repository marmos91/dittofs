package badger

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// shareReadCache is a lock-free cache of decoded ShareOptions keyed by share
// name, so the permission hot path (CheckReadPermissionFile → GetShareOptions)
// skips BOTH the BadgerDB read transaction AND the JSON decode of the share
// record on every NFS read. Server pprof of warm random-read showed
// GetShareOptions → decodeShareData at 17.4% of server CPU, and the badger
// read-transaction the top mutex contender at 14.5% — every read re-decodes a
// config record that changes only on a rare admin reconfigure.
//
// Shares are FEW and rarely written, so unlike fileReadCache this needs no
// prune/cap logic. The correctness discipline is identical (single-node badger
// is single-writer, cf. fileReadCache):
//   - invalidate() runs AFTER a write commits and both deletes the entry and
//     advances `gen`. A reader that observed the pre-commit value cannot leave
//     it cached because its store() is generation-guarded (below). A stale
//     entry here is a WRONG permission decision, so every share-record write
//     site must invalidate.
//   - store() writes only when `gen` is unchanged from the value snapshotted
//     before the backing read; any racing write moves `gen` and the stale
//     populate is dropped (a cache miss, never a stale hit).
type shareReadCache struct {
	m   sync.Map // shareName string -> *metadata.ShareOptions (held read-only)
	gen atomic.Uint64
}

// generation snapshots the invalidation counter; pass the result to store.
func (c *shareReadCache) generation() uint64 { return c.gen.Load() }

// get returns the cached ShareOptions for shareName, or (nil,false). The
// returned pointer is the shared cache entry — callers MUST copy before
// returning it to a mutator (see cloneShareOptions).
func (c *shareReadCache) get(shareName string) (*metadata.ShareOptions, bool) {
	v, ok := c.m.Load(shareName)
	if !ok {
		return nil, false
	}
	return v.(*metadata.ShareOptions), true
}

// store caches opts under shareName only if no write raced the backing read
// (the generation is unchanged since genAtRead). opts MUST NOT be mutated
// afterwards — it becomes the shared cache entry.
func (c *shareReadCache) store(shareName string, opts *metadata.ShareOptions, genAtRead uint64) {
	if c.gen.Load() != genAtRead {
		return
	}
	c.m.Store(shareName, opts)
	// The guard above and the Store are not atomic: a write could commit and
	// invalidate() (bump gen + delete) in between, leaving our now-stale entry
	// live. Re-check the generation; if it moved, drop our entry — but only if a
	// newer reader hasn't already replaced it (CompareAndDelete on our pointer).
	// A stale share entry is a wrong permission decision, so this must be tight.
	if c.gen.Load() != genAtRead {
		c.m.CompareAndDelete(shareName, opts)
	}
}

// invalidate drops shareName and advances the generation so any in-flight
// populate for a now-superseded value is rejected. MUST be called AFTER the
// write commits. Order matters: bump gen BEFORE delete (see fileReadCache) so a
// concurrent reader that snapshotted the old generation cannot re-insert a
// pre-write value after the delete.
func (c *shareReadCache) invalidate(shareName string) {
	c.gen.Add(1)
	c.m.Delete(shareName)
}

// cloneShareOptions returns a caller-owned deep copy of opts: the struct is
// copied and every reference-bearing field (three string slices and the
// IdentityMapping pointee, itself holding *uint32/*uint32/*string) is cloned so
// neither the caller nor a concurrent reader can mutate the shared cache entry.
// A shallow *opts would alias those slices/pointers into the cache.
func cloneShareOptions(opts *metadata.ShareOptions) *metadata.ShareOptions {
	if opts == nil {
		return nil
	}
	cp := *opts
	cp.AllowedClients = slices.Clone(opts.AllowedClients)
	cp.DeniedClients = slices.Clone(opts.DeniedClients)
	cp.AllowedAuthMethods = slices.Clone(opts.AllowedAuthMethods)
	cp.IdentityMapping = cloneIdentityMapping(opts.IdentityMapping)
	return &cp
}

// cloneIdentityMapping deep-copies the mapping and its pointer fields.
func cloneIdentityMapping(m *metadata.IdentityMapping) *metadata.IdentityMapping {
	if m == nil {
		return nil
	}
	cp := *m
	if m.AnonymousUID != nil {
		v := *m.AnonymousUID
		cp.AnonymousUID = &v
	}
	if m.AnonymousGID != nil {
		v := *m.AnonymousGID
		cp.AnonymousGID = &v
	}
	if m.AnonymousSID != nil {
		v := *m.AnonymousSID
		cp.AnonymousSID = &v
	}
	return &cp
}
