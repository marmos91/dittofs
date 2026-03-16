package fs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newTestCacheWithDiskLimit creates an FSStore with a specified max disk size
// for eviction testing. Uses in-memory FileBlockStore.
func newTestCacheWithDiskLimit(t *testing.T, maxDisk int64) *FSStore {
	t.Helper()
	dir := t.TempDir()
	blockStore := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := New(dir, maxDisk, 256*1024*1024, blockStore)
	if err != nil {
		t.Fatalf("failed to create local store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bc.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = bc.Close()
	})
	return bc
}

// populateRemoteBlock creates a block on disk in Remote state (evictable),
// registers it in the FileBlockStore, and updates diskUsed.
func populateRemoteBlock(t *testing.T, bc *FSStore, payloadID string, blockIdx uint64, size int) {
	t.Helper()
	ctx := context.Background()
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	// Create block file on disk
	path := bc.blockPath(blockID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write block file: %v", err)
	}

	// Create FileBlock metadata in Remote state
	fb := blockstore.NewFileBlock(blockID, path)
	fb.State = blockstore.BlockStateRemote
	fb.DataSize = uint32(size)
	fb.BlockStoreKey = blockstore.FormatStoreKey(payloadID, blockIdx)
	fb.LastAccess = time.Now()

	if err := bc.blockStore.PutFileBlock(ctx, fb); err != nil {
		t.Fatalf("failed to put file block: %v", err)
	}

	// Update disk usage tracking
	bc.diskUsed.Add(int64(size))

	// Update file size tracking
	end := blockIdx*blockstore.BlockSize + uint64(size)
	bc.updateFileSize(payloadID, end)
}

// ============================================================================
// Access Tracker Tests
// ============================================================================

func TestAccessTracker_Touch(t *testing.T) {
	at := newAccessTracker()

	before := time.Now()
	at.Touch("file1")
	after := time.Now()

	lastAccess := at.LastAccess("file1")
	if lastAccess.Before(before) || lastAccess.After(after) {
		t.Errorf("expected lastAccess between %v and %v, got %v", before, after, lastAccess)
	}
}

func TestAccessTracker_LastAccess_ReturnsCorrectTime(t *testing.T) {
	at := newAccessTracker()

	t1 := time.Now()
	at.Touch("file1")
	time.Sleep(1 * time.Millisecond)
	at.Touch("file2")

	la1 := at.LastAccess("file1")
	la2 := at.LastAccess("file2")

	if la1.Before(t1) {
		t.Errorf("file1 lastAccess should be >= t1")
	}
	if !la2.After(la1) {
		t.Errorf("file2 should have later access time than file1")
	}
}

func TestAccessTracker_LastAccess_ZeroForUntouched(t *testing.T) {
	at := newAccessTracker()

	lastAccess := at.LastAccess("never-touched")
	if !lastAccess.IsZero() {
		t.Errorf("expected zero time for untouched file, got %v", lastAccess)
	}
}

func TestAccessTracker_Remove(t *testing.T) {
	at := newAccessTracker()

	at.Touch("file1")
	if at.LastAccess("file1").IsZero() {
		t.Fatal("expected non-zero time after Touch")
	}

	at.Remove("file1")
	if !at.LastAccess("file1").IsZero() {
		t.Error("expected zero time after Remove")
	}
}

func TestAccessTracker_FileAccessTimes(t *testing.T) {
	at := newAccessTracker()

	at.Touch("a")
	at.Touch("b")
	at.Touch("c")

	snapshot := at.FileAccessTimes()
	if len(snapshot) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(snapshot))
	}
	for _, key := range []string{"a", "b", "c"} {
		if _, ok := snapshot[key]; !ok {
			t.Errorf("missing key %q in snapshot", key)
		}
	}
}

// ============================================================================
// Eviction Policy Tests
// ============================================================================

func TestEviction_PinMode_NeverEvicts(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1024) // 1KB disk limit
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionPin, 0)

	// Populate a remote block (500 bytes)
	populateRemoteBlock(t, bc, "file1", 0, 500)

	// diskUsed is 500, maxDisk is 1024, need 600 -> over limit
	err := bc.ensureSpace(context.Background(), 600)
	if err != ErrDiskFull {
		t.Fatalf("expected ErrDiskFull for pin mode, got %v", err)
	}

	// Verify block was not evicted (LocalPath still set)
	ctx := context.Background()
	fb, err := bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "file1", blockIdx: 0}))
	if err != nil {
		t.Fatalf("block should still exist: %v", err)
	}
	if fb.LocalPath == "" {
		t.Error("pin mode block should not have been evicted")
	}
}

func TestEviction_TTL_WithinTTL_NotEvicted(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1024)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionTTL, 1*time.Hour)

	// Populate a remote block and touch it recently (30min ago)
	populateRemoteBlock(t, bc, "file1", 0, 500)
	bc.accessTracker.mu.Lock()
	bc.accessTracker.times["file1"] = time.Now().Add(-30 * time.Minute)
	bc.accessTracker.mu.Unlock()

	// Need space -> should NOT evict (within TTL).
	// Use a short-deadline context to avoid waiting the full 30s backpressure timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := bc.ensureSpace(ctx, 600)
	if err == nil {
		t.Fatal("expected error (block within TTL), got nil")
	}

	// Verify block was not evicted
	fb, fbErr := bc.blockStore.GetFileBlock(context.Background(), makeBlockID(blockKey{payloadID: "file1", blockIdx: 0}))
	if fbErr != nil {
		t.Fatalf("block should still exist: %v", fbErr)
	}
	if fb.LocalPath == "" {
		t.Error("TTL block within threshold should not be evicted")
	}
}

func TestEviction_TTL_Expired_Evicted(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1024)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionTTL, 1*time.Hour)

	// Populate a remote block and set access time to 2h ago (expired)
	populateRemoteBlock(t, bc, "file1", 0, 500)
	bc.accessTracker.mu.Lock()
	bc.accessTracker.times["file1"] = time.Now().Add(-2 * time.Hour)
	bc.accessTracker.mu.Unlock()

	// Need space -> should evict (TTL expired)
	err := bc.ensureSpace(context.Background(), 600)
	if err != nil {
		t.Fatalf("expected successful eviction of TTL-expired block, got %v", err)
	}

	// Verify block was evicted (LocalPath cleared)
	ctx := context.Background()
	fb, err := bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "file1", blockIdx: 0}))
	if err != nil {
		t.Fatalf("block metadata should still exist: %v", err)
	}
	if fb.LocalPath != "" {
		t.Error("TTL-expired block should have been evicted (LocalPath should be empty)")
	}
}

func TestEviction_LRU_OldestAccessedFirst(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1500)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

	// Create 3 blocks with different access times
	populateRemoteBlock(t, bc, "old", 0, 500)
	populateRemoteBlock(t, bc, "mid", 0, 500)
	populateRemoteBlock(t, bc, "new", 0, 500)

	now := time.Now()
	bc.accessTracker.mu.Lock()
	bc.accessTracker.times["old"] = now.Add(-3 * time.Hour)   // oldest
	bc.accessTracker.times["mid"] = now.Add(-1 * time.Hour)   // middle
	bc.accessTracker.times["new"] = now.Add(-5 * time.Minute) // newest
	bc.accessTracker.mu.Unlock()

	// diskUsed = 1500, maxDisk = 1500, need 100 -> need to evict 1 block
	err := bc.ensureSpace(context.Background(), 100)
	if err != nil {
		t.Fatalf("expected successful LRU eviction, got %v", err)
	}

	ctx := context.Background()

	// "old" (oldest access) should be evicted
	fbOld, err := bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "old", blockIdx: 0}))
	if err != nil {
		t.Fatalf("old block metadata should exist: %v", err)
	}
	if fbOld.LocalPath != "" {
		t.Error("oldest-accessed block should have been evicted")
	}

	// "mid" and "new" should still be in local store
	fbMid, err := bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "mid", blockIdx: 0}))
	if err != nil {
		t.Fatalf("mid block should exist: %v", err)
	}
	if fbMid.LocalPath == "" {
		t.Error("mid block should not have been evicted")
	}

	fbNew, err := bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "new", blockIdx: 0}))
	if err != nil {
		t.Fatalf("new block should exist: %v", err)
	}
	if fbNew.LocalPath == "" {
		t.Error("newest block should not have been evicted")
	}
}

func TestEviction_LRU_RecentlySurvives(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1000)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

	// Create 2 blocks
	populateRemoteBlock(t, bc, "old-file", 0, 500)
	populateRemoteBlock(t, bc, "recent-file", 0, 500)

	now := time.Now()
	bc.accessTracker.mu.Lock()
	bc.accessTracker.times["old-file"] = now.Add(-2 * time.Hour)
	bc.accessTracker.times["recent-file"] = now.Add(-1 * time.Second) // very recent
	bc.accessTracker.mu.Unlock()

	// Need 100 -> evict old, keep recent
	err := bc.ensureSpace(context.Background(), 100)
	if err != nil {
		t.Fatalf("expected successful LRU eviction, got %v", err)
	}

	ctx := context.Background()

	// old-file should be evicted
	fb, err := bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "old-file", blockIdx: 0}))
	if err != nil {
		t.Fatalf("old-file block should exist: %v", err)
	}
	if fb.LocalPath != "" {
		t.Error("old-file should have been evicted")
	}

	// recent-file should survive
	fb, err = bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "recent-file", blockIdx: 0}))
	if err != nil {
		t.Fatalf("recent-file block should exist: %v", err)
	}
	if fb.LocalPath == "" {
		t.Error("recent-file should not have been evicted")
	}
}

func TestEviction_TTL_ReadResetsAccess(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1000)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionTTL, 1*time.Hour)

	// Create a block with old access time (expired)
	populateRemoteBlock(t, bc, "file1", 0, 500)
	bc.accessTracker.mu.Lock()
	bc.accessTracker.times["file1"] = time.Now().Add(-2 * time.Hour) // expired
	bc.accessTracker.mu.Unlock()

	// Simulate reading file -> touch resets access time
	bc.accessTracker.Touch("file1")

	// Now the block should not be evicted (just touched, within TTL).
	// Use a short-deadline context to avoid waiting the full 30s backpressure timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := bc.ensureSpace(ctx, 600)
	if err == nil {
		t.Fatal("expected error (TTL reset by read), got nil")
	}

	fb, err := bc.blockStore.GetFileBlock(context.Background(), makeBlockID(blockKey{payloadID: "file1", blockIdx: 0}))
	if err != nil {
		t.Fatalf("block should exist: %v", err)
	}
	if fb.LocalPath == "" {
		t.Error("block should not be evicted after access time reset")
	}
}

func TestEviction_PolicySwitch_PinToLRU(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1024)
	bc.SetEvictionEnabled(true)

	// Start with pin mode
	bc.SetRetentionPolicy(blockstore.RetentionPin, 0)

	// Populate a remote block
	populateRemoteBlock(t, bc, "file1", 0, 500)
	bc.accessTracker.mu.Lock()
	bc.accessTracker.times["file1"] = time.Now().Add(-2 * time.Hour)
	bc.accessTracker.mu.Unlock()

	// Pin mode -> should not evict
	err := bc.ensureSpace(context.Background(), 600)
	if err != ErrDiskFull {
		t.Fatalf("expected ErrDiskFull in pin mode, got %v", err)
	}

	// Switch to LRU mode -> should now evict
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

	err = bc.ensureSpace(context.Background(), 600)
	if err != nil {
		t.Fatalf("expected successful eviction after switch to LRU, got %v", err)
	}

	ctx := context.Background()
	fb, err := bc.blockStore.GetFileBlock(ctx, makeBlockID(blockKey{payloadID: "file1", blockIdx: 0}))
	if err != nil {
		t.Fatalf("block metadata should exist: %v", err)
	}
	if fb.LocalPath != "" {
		t.Error("block should have been evicted after switch to LRU")
	}
}
