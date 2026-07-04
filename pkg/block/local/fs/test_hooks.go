package fs

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// This file exposes a small set of test-only accessors and helpers on
// *FSStore so the fs-internal conformance scenarios in
// eviction_lsl08_conformance_test.go and appendlog_internals_test.go
// can observe / manipulate internal state. Every symbol here has a
// `_ForTest` suffix so reviewers can catch production misuse
// (STRIDE mitigation).
//
// These helpers exist only to support the conformance scenarios
// (10) and the eviction scenarios. They are
// not part of the production FSStore API. The -06 mega-PR
// retired the legacy pkg/block/local/localtest/ package that
// originally consumed these hooks; the scenarios now live inside the
// fs package as external `_test.go` files.

// BaseDirForTest returns the FSStore baseDir.
func (bc *FSStore) BaseDirForTest() string { return bc.baseDir }

// FlushFsyncCountForTest returns the cumulative number of fsyncs issued by
// flushBlock since FSStore startup. Used by tests that assert the
// pressure-driven and block-fill flush paths do NOT fsync (only Flush /
// NFS COMMIT does). Reset is not provided; tests should sample twice and
// compute the delta.
func (bc *FSStore) FlushFsyncCountForTest() int64 { return bc.flushFsyncCount.Load() }

// LogBlobRollupSyncCountForTest returns the cumulative number of times a
// rollup pass called logBlob.Sync() before advancing the fence. Used by
// tests to assert the durability invariant: chunk bytes fsynced before
// rollup_offset advances.
func (bc *FSStore) LogBlobRollupSyncCountForTest() int64 {
	return bc.logBlobRollupSyncCount.Load()
}

// RollupOffsetForTest returns the metadata rollup_offset for payloadID.
// Returns (0, nil) when no RollupStore is configured.
func (bc *FSStore) RollupOffsetForTest(ctx context.Context, payloadID string) (uint64, error) {
	if bc.rollupStore == nil {
		return 0, nil
	}
	return bc.rollupStore.GetRollupOffset(ctx, payloadID)
}

// SetMaxLogBytesForTest overrides the pressure budget post-construction.
// Used by the pressure-channel scenario to force contention with minimal
// data.
func (bc *FSStore) SetMaxLogBytesForTest(n int64) { bc.maxLogBytes = n }

// LogBytesTotalForTest exposes the current in-memory accounting of
// un-rolled-up log bytes.
func (bc *FSStore) LogBytesTotalForTest() int64 { return bc.logBytesTotal.Load() }

// RollupStoreForTest returns the installed RollupStore (may be nil).
func (bc *FSStore) RollupStoreForTest() metadata.RollupStore { return bc.rollupStore }

// IntervalsLenForTest returns the number of dirty intervals currently
// tracked for payloadID, or 0 when the payload has no interval tree.
//
// Acquires the per-file mutex to serialize against rollupFile's
// ConsumeUpTo (which mutates the btree under mu). The shard lock alone is
// insufficient because rollupFile snapshots the tree pointer under the
// shard RLock and then mutates the btree later under the per-file mu.
func (bc *FSStore) IntervalsLenForTest(payloadID string) int {
	sh := bc.shardFor(payloadID)
	sh.mu.RLock()
	t := sh.dirtyIntervals[payloadID]
	mu := sh.logLocks[payloadID]
	sh.mu.RUnlock()
	if t == nil {
		return 0
	}
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return t.Len()
}

// EarliestStableForTest reports whether the earliest dirty interval for
// payloadID is currently considered stable under the configured
// stabilization window. Used by tests to poll for the exact moment
// rollupFile would observe a stable interval — replacing brittle
// time.Sleep calls (FIX-10).
func (bc *FSStore) EarliestStableForTest(payloadID string) bool {
	sh := bc.shardFor(payloadID)
	sh.mu.RLock()
	tree := sh.dirtyIntervals[payloadID]
	sh.mu.RUnlock()
	if tree == nil {
		return false
	}
	stabilization := time.Duration(bc.stabilizationMS) * time.Millisecond
	_, ok := tree.EarliestStable(time.Now(), stabilization)
	return ok
}

// HeaderRollupOffsetForTest reads the on-disk header's rollup_offset for
// payloadID. Returns 0 when the log file is missing or the header is
// unreadable.
func (bc *FSStore) HeaderRollupOffsetForTest(payloadID string) uint64 {
	path := filepath.Join(bc.baseDir, "logs", payloadID+".log")
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	h, err := readLogHeader(f)
	if err != nil {
		return 0
	}
	return h.RollupOffset
}

// ForceRollupForTest runs one rollup pass synchronously on payloadID. The
// conformance suite uses this when the ambient worker pool is disabled or
// timing would otherwise be flaky.
func (bc *FSStore) ForceRollupForTest(ctx context.Context, payloadID string) error {
	return bc.rollupFile(ctx, payloadID, false)
}

// ReopenForTest closes no store — it constructs a fresh FSStore on
// baseDir with the append-log flag enabled and runs Recover so the
// interval tree is rebuilt and the header is reconciled against
// metadata. Caller MUST have Close()'d any prior FSStore on the same
// directory first (concurrent opens race on the log fds).
func ReopenForTest(baseDir string, rs metadata.RollupStore) (*FSStore, error) {
	bc, err := NewWithOptions(baseDir, 1<<30, nopFBSForTest{}, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 50,
		RollupStore:     rs,
	})
	if err != nil {
		return nil, err
	}
	if err := bc.Recover(context.Background()); err != nil {
		_ = bc.Close()
		return nil, err
	}
	return bc, nil
}

// RecomputeHeaderCRCForTest rewrites bytes [28..32) of header based on
// bytes [0..28). Used by crash-simulation tests that zero the
// rollup_offset field and need a valid header CRC so recovery treats the
// log as well-formed (rather than as a hard-corrupt header).
func RecomputeHeaderCRCForTest(header []byte) {
	if len(header) < 32 {
		return
	}
	crc := crc32.Checksum(header[0:28], crcTable)
	binary.LittleEndian.PutUint32(header[28:32], crc)
}

// EnsureSpaceForTest invokes ensureSpace from external test packages.
// Used by the conformance suite (RunEvictionLSL08Suite).
func (bc *FSStore) EnsureSpaceForTest(ctx context.Context, needed int64) error {
	return bc.ensureSpace(ctx, needed)
}

// fbsCallCounterForTest hooks for the "no FileChunkStore calls
// during ensureSpace" assertion. Backends that wrap FBS in a counting
// wrapper expose ResetFBSCallCounterForTest / FBSCallCountForTest
// otherwise these helpers are no-ops returning 0.
//
// FSStore implements them via the *countingFileChunkStore wrapper
// installed by the factory in eviction_lsl08_conformance_test.go.
// When the field is nil (not-counted), both helpers are no-ops.

// FBSCounter is implemented by counting wrappers around FileChunkStore.
// Used by the conformance suite to assert ensureSpace makes zero
// FileChunkStore calls. Exported so cross-package test wrappers can
// satisfy it.
type FBSCounter interface {
	ResetCount()
	TotalCount() int
}

// ResetFBSCallCounterForTest zeroes the counter on a counting wrapper
// around FileChunkStore. No-op when the underlying store is not counted.
func (bc *FSStore) ResetFBSCallCounterForTest() {
	if c, ok := bc.blockStore.(FBSCounter); ok {
		c.ResetCount()
	}
}

// FBSCallCountForTest returns the number of FileChunkStore method calls
// recorded since the last ResetFBSCallCounterForTest. Returns 0 if the
// underlying store is not counted.
func (bc *FSStore) FBSCallCountForTest() int {
	if c, ok := bc.blockStore.(FBSCounter); ok {
		return c.TotalCount()
	}
	return 0
}

// nopFBSForTest is a no-op FileChunkStore used by the ReopenForTest
// helper. Every read returns ErrFileChunkNotFound; every write is a
// no-op. Sufficient for the append-log conformance suite because
// AppendWrite does not consult FileChunkStore at all, and the
// Recover walk over .blk files finds none in a test tempdir that only
// holds logs/ + blocks/.
//
// This is defined here (not in an _test.go file) so external test
// packages can call ReopenForTest without exporting the
// FileChunkStore type separately.
type nopFBSForTest struct{}

// nopFBSForTest satisfies the wider block.EngineFileChunkStore (the
// 6 narrowed FileChunkStore methods plus the engine-internal GetFileChunk
// and ListFileChunks). All operations are no-ops or return
// ErrFileChunkNotFound.
func (nopFBSForTest) GetByHash(_ context.Context, _ block.ContentHash) (*block.FileChunk, error) {
	return nil, nil
}
func (nopFBSForTest) Put(_ context.Context, _ *block.FileChunk) error { return nil }
func (nopFBSForTest) Delete(_ context.Context, _ string) error {
	return block.ErrFileChunkNotFound
}
func (nopFBSForTest) IncrementRefCount(_ context.Context, _ string) error { return nil }
func (nopFBSForTest) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (nopFBSForTest) DecrementRefCountAndReap(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (nopFBSForTest) AddRef(_ context.Context, _ block.ContentHash, _ string, _ block.ChunkRef) error {
	// no-op test stub. Every hash is "unknown" so the
	// LRU hit path in production would always fall back to the full
	// Put path — but this stub is only used by ReopenForTest scenarios
	// that don't exercise AddRef at all.
	return block.ErrUnknownHash
}

// Legacy engine-internal surface (kept off the
// public FileChunkStore interface but required by EngineFileChunkStore).
func (nopFBSForTest) GetFileChunk(_ context.Context, _ string) (*block.FileChunk, error) {
	return nil, block.ErrFileChunkNotFound
}
func (nopFBSForTest) ListFileChunks(_ context.Context, _ string) ([]*block.FileChunk, error) {
	return nil, nil
}
func (nopFBSForTest) EnumeratePayloads(_ context.Context, _ func(payloadID string) error) error {
	return nil
}
