package fs_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newFSStoreForConformance builds a fresh *fs.FSStore with the production
// wiring used across conformance scenarios: memory-backed RollupStore,
// rollup pool started. Returns the store and a cleanup closure that closes
// it. Shared between the BlockStore and BlockStoreAppend factories so the
// wiring is identical across both contracts.
func newFSStoreForConformance(t *testing.T) *fs.FSStore {
	t.Helper()
	dir := t.TempDir()
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.NewWithOptions(dir, 1<<30, nil, fs.FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 50,
		RollupStore:     rs,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	return bc
}

// TestFSStore_BlockStoreConformance runs the unified
// BlockStoreConformance suite from pkg/block/blockstoretest against
// *fs.FSStore. The fs backend satisfies the BlockStore contract by way
// of its embedded BlockStoreAppend implementation (07 lands the
// missing Put/Has/Walk/etc. methods that complete the interface).
func TestFSStore_BlockStoreConformance(t *testing.T) {
	factory := func(t *testing.T) (block.Store, func()) {
		t.Helper()
		bc := newFSStoreForConformance(t)
		cleanup := func() { _ = bc.Close() }
		return bc, cleanup
	}
	blockstoretest.BlockStoreConformance(t, factory)
}

// TestFSStore_BlockStoreAppendConformance runs the random-write
// absorber suite from pkg/block/blockstoretest against
// *fs.FSStore. The fs backend implements BlockStoreAppend in full
// (s3 and memory-remote implement only BlockStore).
//
// Note: three of the five scenarios (PressureChannel_INV05,
// TornWriteRecovery_LSL06, RollupOffsetMonotone_INV03) `t.Skip` on
// the interface-only surface because they require fs-internal probes
// that intentionally do not appear on BlockStoreAppend. The fs
// backend continues to exercise those scenarios in
// appendlog_test.go / recovery_test.go via internal test hooks.
func TestFSStore_BlockStoreAppendConformance(t *testing.T) {
	factory := func(t *testing.T) (block.BlockStoreAppend, func()) {
		t.Helper()
		bc := newFSStoreForConformance(t)
		cleanup := func() { _ = bc.Close() }
		return bc, cleanup
	}
	blockstoretest.BlockStoreAppendConformance(t, factory)
}
