package handlers

import "sync/atomic"

// Filesystem capability cache.
//
// wtmax and rtmax are static, store-level limits, but WRITE and READ need them
// on every RPC to clamp over-large requests to what FSINFO told the client.
// GetFilesystemCapabilities is a store call (a Badger db.View on the BadgerDB
// backend), so the values are cached here and refreshed whenever they are
// observed: in FSINFO, or by the first WRITE/READ to find a cold cache. This
// mirrors the NFSv4 attrs package, which caches the same store capabilities in
// atomic globals. Atomics keep concurrent handlers race-free.
//
// Zero means "not yet observed": a caller must fall back to a store lookup and
// must not clamp, since clamping to zero would truncate every request.
//
// INVARIANT (process-global is safe): both are server-wide values, not
// per-share ones. GetFilesystemCapabilities ignores the file handle and every
// metadata backend (memory/badger/postgres) returns the same hardcoded
// MaxWriteSize/MaxReadSize (1 MiB), and FSINFO advertises those same values to
// every client on every share, so a single global cannot diverge from what any
// one client was told. If a future change makes them per-share (a per-share
// override in GetFilesystemCapabilities), this cache MUST become handle-keyed
// to avoid clamping one share's request with another share's limit.
var (
	fsMaxWriteSize atomic.Uint32
	fsMaxReadSize  atomic.Uint32
)

// cacheMax records an advertised wtmax/rtmax so subsequent RPCs can clamp
// without a per-RPC store lookup. Zero is ignored (treated as unknown).
func cacheMax(cached *atomic.Uint32, size uint32) {
	if size > 0 {
		cached.Store(size)
	}
}

// ResetCapsCacheForTest clears the process-global capability cache so a test
// starts from a cold cache. Because the cache is process-wide, tests that set
// different wtmax/rtmax values must reset it to avoid bleed-through from
// earlier runs.
func ResetCapsCacheForTest() {
	fsMaxWriteSize.Store(0)
	fsMaxReadSize.Store(0)
}
