package fs_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/local/localtest"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestFSStore_AppendLogConformance runs the shared D-22 append-log
// conformance suite (LSL-01..LSL-06 + INV-03/INV-05) against the
// in-tree *fs.FSStore. The factory constructs a fresh store with
// UseAppendLog=true, a memory-backed RollupStore, and the rollup pool
// started — matching the production wiring contract Phase 11 (A2) will
// flip the default on.
func TestFSStore_AppendLogConformance(t *testing.T) {
	localtest.RunAppendLogSuite(t, func(t *testing.T) *fs.FSStore {
		t.Helper()
		dir := t.TempDir()
		rs := memmeta.NewMemoryMetadataStoreWithDefaults()
		bc, err := fs.NewWithOptions(dir, 1<<30, 1<<30, nil, fs.FSStoreOptions{
			UseAppendLog:    true,
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
		t.Cleanup(func() { _ = bc.Close() })
		return bc
	})
}
