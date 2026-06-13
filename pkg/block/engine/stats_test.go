package engine

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestStats_EmptyStore verifies Stats() returns UsedSize==0 for an empty store.
func TestStats_EmptyStore(t *testing.T) {
	bs := newTestEngine(t, 0, 0)

	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	if stats.UsedSize != 0 {
		t.Fatalf("expected UsedSize==0 for empty store, got %d", stats.UsedSize)
	}
	if stats.ContentCount != 0 {
		t.Fatalf("expected ContentCount==0 for empty store, got %d", stats.ContentCount)
	}
}

// TestStats_UsedSizeMatchesDiskUsed verifies Stats().UsedSize == local.Stats().DiskUsed.
func TestStats_UsedSizeMatchesDiskUsed(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	// Write data to the local store.
	if _, err := bs.WriteAt(ctx, "stats-test", nil, []byte("some data for stats"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	localStats := bs.local.Stats()
	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	// Verify UsedSize is wired to local DiskUsed.
	if stats.UsedSize != uint64(localStats.DiskUsed) {
		t.Fatalf("UsedSize=%d does not match localStats.DiskUsed=%d", stats.UsedSize, localStats.DiskUsed)
	}

	// Verify ContentCount reflects the file count.
	if stats.ContentCount == 0 {
		t.Fatal("expected ContentCount > 0 after writing data")
	}
}

// TestStats_AvailableSize verifies AvailableSize == TotalSize - UsedSize.
func TestStats_AvailableSize(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	// Write data.
	if _, err := bs.WriteAt(ctx, "avail-test", nil, []byte("data"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	// When TotalSize > UsedSize, AvailableSize should be the difference.
	if stats.TotalSize > stats.UsedSize {
		expected := stats.TotalSize - stats.UsedSize
		if stats.AvailableSize != expected {
			t.Fatalf("AvailableSize=%d, expected TotalSize(%d) - UsedSize(%d) = %d",
				stats.AvailableSize, stats.TotalSize, stats.UsedSize, expected)
		}
	}

	// When TotalSize <= UsedSize, AvailableSize should be 0.
	// (Memory store has TotalSize=0 and UsedSize=0, so AvailableSize=0 is correct)
	if stats.TotalSize <= stats.UsedSize && stats.AvailableSize != 0 {
		t.Fatalf("expected AvailableSize==0 when TotalSize(%d) <= UsedSize(%d), got %d",
			stats.TotalSize, stats.UsedSize, stats.AvailableSize)
	}
}

// TestStats_AverageSize verifies AverageSize is computed correctly.
func TestStats_AverageSize(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	// Write data to two files.
	if _, err := bs.WriteAt(ctx, "avg-1", nil, []byte("data1"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bs.WriteAt(ctx, "avg-2", nil, []byte("data2data2"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	if stats.ContentCount > 0 && stats.UsedSize > 0 {
		expected := stats.UsedSize / stats.ContentCount
		if stats.AverageSize != expected {
			t.Fatalf("AverageSize=%d, expected UsedSize(%d) / ContentCount(%d) = %d",
				stats.AverageSize, stats.UsedSize, stats.ContentCount, expected)
		}
	}

	// When ContentCount == 0, AverageSize should be 0 (tested by empty store test).
}

// TestGetStats_PopulateBlockCounts_CASPendingCountedAsLocal verifies that a
// CAS-path Pending FileBlock (non-zero Hash, no LocalPath / BlockStoreKey) is
// classified as locally present rather than dirty. Before the fix the
// discriminator keyed on LocalPath/BlockStoreKey, which are never set on the
// CAS path, so every rolled-up chunk was mis-counted as dirty.
func TestGetStats_PopulateBlockCounts_CASPendingCountedAsLocal(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	fbs := bs.fileBlockStore.(*stubFileBlockStore)

	// Block A: CAS-path Pending — non-zero hash, no LocalPath, no BlockStoreKey.
	// Before fix -> BlocksDirty (wrong). After fix -> BlocksLocal (correct).
	var hashA block.ContentHash
	hashA[0] = 0xAB
	blockA := &block.FileBlock{
		ID:    "payload-test/0",
		Hash:  hashA,
		State: block.BlockStatePending,
	}

	// Block B: truly dirty/in-flight — zero hash.
	// Both before and after fix -> BlocksDirty (correct).
	blockB := &block.FileBlock{
		ID:    "payload-test/8388608",
		Hash:  block.ContentHash{}, // zero
		State: block.BlockStatePending,
	}

	// Block C: Remote — must go to BlocksRemote.
	var hashC block.ContentHash
	hashC[0] = 0xCD
	blockC := &block.FileBlock{
		ID:            "payload-test/16777216",
		Hash:          hashC,
		BlockStoreKey: "cas/cd/00/cd00",
		State:         block.BlockStateRemote,
	}

	if err := fbs.Put(ctx, blockA); err != nil {
		t.Fatalf("Put blockA: %v", err)
	}
	if err := fbs.Put(ctx, blockB); err != nil {
		t.Fatalf("Put blockB: %v", err)
	}
	if err := fbs.Put(ctx, blockC); err != nil {
		t.Fatalf("Put blockC: %v", err)
	}

	// Register the payload so ListFiles returns it and ListFileBlocks is called.
	if _, err := bs.WriteAt(ctx, "payload-test", nil, []byte("x"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	stats := bs.GetStats()

	if stats.BlocksTotal != 3 {
		t.Errorf("BlocksTotal: got %d, want 3", stats.BlocksTotal)
	}
	if stats.BlocksLocal != 1 {
		// Before fix this is 0 (block A goes to BlocksDirty).
		t.Errorf("BlocksLocal: got %d, want 1 (block A — CAS Pending with non-zero hash)", stats.BlocksLocal)
	}
	if stats.BlocksDirty != 1 {
		// Before fix this is 2 (block A is misclassified here).
		t.Errorf("BlocksDirty: got %d, want 1 (block B — truly dirty, zero hash)", stats.BlocksDirty)
	}
	if stats.BlocksRemote != 1 {
		t.Errorf("BlocksRemote: got %d, want 1 (block C)", stats.BlocksRemote)
	}
}
