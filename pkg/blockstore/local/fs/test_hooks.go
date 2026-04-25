package fs

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// This file exposes a small set of test-only accessors and helpers on
// *FSStore so the shared conformance suite in localtest/ can observe /
// manipulate internal state without the localtest package needing access
// to unexported fields. Every symbol here has a `_ForTest` suffix so
// reviewers can catch production misuse (T-10-10-01 STRIDE mitigation).
//
// These helpers exist only to support the five D-22 conformance scenarios
// (plan 10-10). They are not part of the production FSStore API.

// BaseDirForTest returns the FSStore baseDir.
func (bc *FSStore) BaseDirForTest() string { return bc.baseDir }

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
func (bc *FSStore) IntervalsLenForTest(payloadID string) int {
	bc.logsMu.RLock()
	defer bc.logsMu.RUnlock()
	if t := bc.dirtyIntervals[payloadID]; t != nil {
		return t.Len()
	}
	return 0
}

// EarliestStableForTest reports whether the earliest dirty interval for
// payloadID is currently considered stable under the configured
// stabilization window. Used by tests to poll for the exact moment
// rollupFile would observe a stable interval — replacing brittle
// time.Sleep calls (FIX-10).
func (bc *FSStore) EarliestStableForTest(payloadID string) bool {
	bc.logsMu.RLock()
	tree := bc.dirtyIntervals[payloadID]
	bc.logsMu.RUnlock()
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
	return bc.rollupFile(ctx, payloadID)
}

// ReopenForTest closes no store — it constructs a fresh FSStore on
// baseDir with the append-log flag enabled and runs Recover so the
// interval tree is rebuilt and the header is reconciled against
// metadata. Caller MUST have Close()'d any prior FSStore on the same
// directory first (concurrent opens race on the log fds).
func ReopenForTest(baseDir string, rs metadata.RollupStore) (*FSStore, error) {
	bc, err := NewWithOptions(baseDir, 1<<30, 1<<30, nopFBSForTest{}, FSStoreOptions{
		UseAppendLog:    true,
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

// nopFBSForTest is a no-op FileBlockStore used by the ReopenForTest
// helper. Every read returns ErrFileBlockNotFound; every write is a
// no-op. Sufficient for the append-log conformance suite because
// AppendWrite (D-34) does not consult FileBlockStore at all, and the
// Recover walk over .blk files finds none in a test tempdir that only
// holds logs/ + blocks/.
//
// This is defined here (not in an _test.go file) so external packages
// like localtest can call ReopenForTest without exporting the
// FileBlockStore type separately.
type nopFBSForTest struct{}

func (nopFBSForTest) GetFileBlock(_ context.Context, _ string) (*blockstore.FileBlock, error) {
	return nil, blockstore.ErrFileBlockNotFound
}
func (nopFBSForTest) PutFileBlock(_ context.Context, _ *blockstore.FileBlock) error { return nil }
func (nopFBSForTest) DeleteFileBlock(_ context.Context, _ string) error {
	return blockstore.ErrFileBlockNotFound
}
func (nopFBSForTest) IncrementRefCount(_ context.Context, _ string) error { return nil }
func (nopFBSForTest) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (nopFBSForTest) FindFileBlockByHash(_ context.Context, _ blockstore.ContentHash) (*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBSForTest) ListLocalBlocks(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBSForTest) ListRemoteBlocks(_ context.Context, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBSForTest) ListUnreferenced(_ context.Context, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBSForTest) ListFileBlocks(_ context.Context, _ string) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
