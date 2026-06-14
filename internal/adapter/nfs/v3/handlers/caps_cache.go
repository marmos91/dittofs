package handlers

import "sync/atomic"

// Filesystem capability cache.
//
// The advertised maximum WRITE size (wtmax) is a static, store-level limit. The
// WRITE handler needs it on every RPC to short-write over-large requests to the
// value the client was told in FSINFO. Calling GetFilesystemCapabilities (a
// Badger db.View on the BadgerDB backend) on every WRITE is wasteful, so the
// value is cached here and refreshed whenever it is observed (FSINFO, or the
// first WRITE on a cold cache).
//
// This mirrors the NFSv4 attrs package, which caches the same store
// capabilities in atomic globals updated via SetFilesystemCapabilities.
//
// INVARIANT (process-global is safe): wtmax is a server-wide value, not a
// per-share one. GetFilesystemCapabilities ignores the file handle and every
// metadata backend (memory/badger/postgres) returns the same hardcoded
// MaxWriteSize (1 MiB). FSINFO advertises this same value to every client on
// every share, so a single global cannot diverge from what any one client was
// told. If a future change makes wtmax per-share (a per-share override in
// GetFilesystemCapabilities), this cache MUST become handle-keyed to avoid
// clamping one share's WRITE with another share's limit.
//
// Uses an atomic to stay race-free with concurrent WRITE/FSINFO handlers.

// fsMaxWriteSize holds the cached wtmax. Zero means "not yet observed".
var fsMaxWriteSize atomic.Uint32

// setMaxWriteSize records the advertised wtmax so subsequent WRITEs can clamp
// without a per-RPC store lookup. A zero value is ignored (treated as unknown).
func setMaxWriteSize(maxWriteSize uint32) {
	if maxWriteSize > 0 {
		fsMaxWriteSize.Store(maxWriteSize)
	}
}

// cachedMaxWriteSize returns the cached wtmax, or 0 if it has not been observed
// yet. A zero result means the caller must fall back to a one-time store lookup.
func cachedMaxWriteSize() uint32 {
	return fsMaxWriteSize.Load()
}

// ResetCapsCacheForTest clears the process-global capability cache so a test
// starts from a cold cache. Because the cache is process-wide, tests that set
// different wtmax values must reset it to avoid bleed-through from earlier runs.
func ResetCapsCacheForTest() {
	fsMaxWriteSize.Store(0)
}
