package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// countingFileBlockStore wraps a blockstore.EngineFileBlockStore and
// counts calls per method. Used by the LSL-08 conformance suite (and
// neighboring eviction tests) to assert that the local store's
// synchronous hot-path / eviction paths make zero FileBlockStore calls
// (TD-02d / D-19).
//
// Counters are atomic so tests may observe them without racing the
// background SyncFileBlocks goroutine that Start() launches.
type countingFileBlockStore struct {
	inner blockstore.EngineFileBlockStore

	get               atomic.Int64 // GetFileBlock (engine-internal)
	put               atomic.Int64 // Put (was PutFileBlock)
	del               atomic.Int64 // Delete (was DeleteFileBlock)
	incrementRefCount atomic.Int64
	decrementRefCount atomic.Int64
	getByHash         atomic.Int64 // GetByHash (was FindFileBlockByHash)
	listPending       atomic.Int64 // ListPending (was ListLocalBlocks)
	listFileBlocks    atomic.Int64 // engine-internal
}

func newCountingFileBlockStore(inner blockstore.EngineFileBlockStore) *countingFileBlockStore {
	return &countingFileBlockStore{inner: inner}
}

func (c *countingFileBlockStore) GetFileBlock(ctx context.Context, id string) (*blockstore.FileBlock, error) {
	c.get.Add(1)
	return c.inner.GetFileBlock(ctx, id)
}

func (c *countingFileBlockStore) Put(ctx context.Context, block *blockstore.FileBlock) error {
	c.put.Add(1)
	return c.inner.Put(ctx, block)
}

func (c *countingFileBlockStore) Delete(ctx context.Context, id string) error {
	c.del.Add(1)
	return c.inner.Delete(ctx, id)
}

func (c *countingFileBlockStore) IncrementRefCount(ctx context.Context, id string) error {
	c.incrementRefCount.Add(1)
	return c.inner.IncrementRefCount(ctx, id)
}

func (c *countingFileBlockStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	c.decrementRefCount.Add(1)
	return c.inner.DecrementRefCount(ctx, id)
}

func (c *countingFileBlockStore) GetByHash(ctx context.Context, hash blockstore.ContentHash) (*blockstore.FileBlock, error) {
	c.getByHash.Add(1)
	return c.inner.GetByHash(ctx, hash)
}

func (c *countingFileBlockStore) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*blockstore.FileBlock, error) {
	c.listPending.Add(1)
	return c.inner.ListPending(ctx, olderThan, limit)
}

func (c *countingFileBlockStore) ListFileBlocks(ctx context.Context, payloadID string) ([]*blockstore.FileBlock, error) {
	c.listFileBlocks.Add(1)
	return c.inner.ListFileBlocks(ctx, payloadID)
}

// snapshot captures the current call counts for comparison.
type fbsCallSnapshot struct {
	get, put, del, inc, dec, find, listPending, listFile int64
}

func (c *countingFileBlockStore) snapshot() fbsCallSnapshot {
	return fbsCallSnapshot{
		get:         c.get.Load(),
		put:         c.put.Load(),
		del:         c.del.Load(),
		inc:         c.incrementRefCount.Load(),
		dec:         c.decrementRefCount.Load(),
		find:        c.getByHash.Load(),
		listPending: c.listPending.Load(),
		listFile:    c.listFileBlocks.Load(),
	}
}

func diffSnapshot(before, after fbsCallSnapshot) fbsCallSnapshot {
	return fbsCallSnapshot{
		get:         after.get - before.get,
		put:         after.put - before.put,
		del:         after.del - before.del,
		inc:         after.inc - before.inc,
		dec:         after.dec - before.dec,
		find:        after.find - before.find,
		listPending: after.listPending - before.listPending,
		listFile:    after.listFile - before.listFile,
	}
}

// ResetCount and TotalCount satisfy the FBSCounter interface declared in
// test_hooks.go so the LSL-08 conformance suite can assert no
// FileBlockStore calls happen during ensureSpace.
func (c *countingFileBlockStore) ResetCount() {
	c.get.Store(0)
	c.put.Store(0)
	c.del.Store(0)
	c.incrementRefCount.Store(0)
	c.decrementRefCount.Store(0)
	c.getByHash.Store(0)
	c.listPending.Store(0)
	c.listFileBlocks.Store(0)
}

func (c *countingFileBlockStore) TotalCount() int {
	return int(c.get.Load() +
		c.put.Load() +
		c.del.Load() +
		c.incrementRefCount.Load() +
		c.decrementRefCount.Load() +
		c.getByHash.Load() +
		c.listPending.Load() +
		c.listFileBlocks.Load())
}

// newTestCache creates an FSStore with a temporary directory and in-memory block store.
func newTestCache(t *testing.T, maxMemory int64) *FSStore {
	t.Helper()
	dir := t.TempDir()
	blockStore := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := New(dir, 0, maxMemory, blockStore)
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

// writeSentinelForTest writes a minimal valid `.cas-migrated-v1` marker
// at the share-dir root. Mirrors pkg/blockstore/migrate.writeSentinel's
// contract (file content is opaque to the boot guard; presence is what
// matters).
func writeSentinelForTest(t *testing.T, shareDir string) {
	t.Helper()
	p := filepath.Join(shareDir, ".cas-migrated-v1")
	if err := os.WriteFile(p, []byte(`{"version":"v1"}`), 0644); err != nil {
		t.Fatalf("write sentinel %q: %v", p, err)
	}
}

// writeLegacyBlkForTest creates a non-empty `.blk` file at the legacy
// path-keyed layout depth that *fs.FSStore's flush path historically
// produced: <baseDir>/<shard>/<payloadID>/<idx>.blk.
func writeLegacyBlkForTest(t *testing.T, shareDir string) {
	t.Helper()
	payloadDir := filepath.Join(shareDir, "fi", "file-001")
	if err := os.MkdirAll(payloadDir, 0755); err != nil {
		t.Fatalf("mkdir %q: %v", payloadDir, err)
	}
	p := filepath.Join(payloadDir, "0.blk")
	if err := os.WriteFile(p, []byte("legacy bytes"), 0644); err != nil {
		t.Fatalf("write legacy blk %q: %v", p, err)
	}
}

// TestFSStoreStartCloseNoGoroutineLeak asserts that the Start/Close
// lifecycle joins its background goroutines deterministically (no
// linear leak per cycle).
func TestFSStoreStartCloseNoGoroutineLeak(t *testing.T) {
	// Warm-up: allow any package-init goroutines to settle before measuring.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	const cycles = 20
	ctx := context.Background() // never cancelled — only Close may stop the goroutine
	for i := 0; i < cycles; i++ {
		dir := t.TempDir()
		blockStore := memory.NewMemoryMetadataStoreWithDefaults()
		bc, err := New(dir, 0, 256*1024*1024, blockStore)
		if err != nil {
			t.Fatalf("cycle %d: New failed: %v", i, err)
		}
		bc.Start(ctx)
		// Close must deterministically join the Start goroutine.
		if err := bc.Close(); err != nil {
			t.Fatalf("cycle %d: Close failed: %v", i, err)
		}
	}

	// Small settle window — Close() should have joined already; this
	// accounts only for scheduler jitter, not for goroutines still
	// selecting on a ticker.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// A real leak accumulates linearly with cycles (20). A small
	// tolerance absorbs unrelated test-runner background goroutines.
	if delta := after - before; delta > 2 {
		t.Fatalf("goroutine leak: before=%d after=%d delta=%d (cycles=%d)", before, after, delta, cycles)
	}
}

// TestNewFSStore_SentinelDetection exercises the boot-time legacy-layout
// guard: a share directory with stray <share>/<shard>/<payloadID>/<idx>.blk
// files but no migration sentinel must be refused with
// ErrLegacyLayoutDetected. The matrix asserts that the sentinel takes
// precedence over the .blk presence check, and that depth-3 .blk files
// are the trigger.
func TestNewFSStore_SentinelDetection(t *testing.T) {
	type matrixCase struct {
		name          string
		writeSentinel bool
		writeBlk      bool
		wantLegacy    bool // expect ErrLegacyLayoutDetected
	}
	cases := []matrixCase{
		{name: "sentinel_present_no_blk_files", writeSentinel: true, writeBlk: false, wantLegacy: false},
		{name: "sentinel_present_with_blk_files", writeSentinel: true, writeBlk: true, wantLegacy: false},
		{name: "no_sentinel_no_blk_files", writeSentinel: false, writeBlk: false, wantLegacy: false},
		{name: "no_sentinel_with_blk_files", writeSentinel: false, writeBlk: true, wantLegacy: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shareDir := t.TempDir()
			if tc.writeSentinel {
				writeSentinelForTest(t, shareDir)
			}
			if tc.writeBlk {
				writeLegacyBlkForTest(t, shareDir)
			}
			mds := memory.NewMemoryMetadataStoreWithDefaults()
			bc, err := New(shareDir, 0, 256*1024*1024, mds)
			if tc.wantLegacy {
				if err == nil {
					_ = bc.Close()
					t.Fatalf("expected ErrLegacyLayoutDetected, got nil")
				}
				if !errors.Is(err, blockstore.ErrLegacyLayoutDetected) {
					t.Fatalf("expected errors.Is(err, ErrLegacyLayoutDetected); got %v", err)
				}
				// Share path must appear in the wrapped message so the
				// boot directive can echo it back to the operator.
				if !bytes.Contains([]byte(err.Error()), []byte(shareDir)) {
					t.Errorf("err %q missing share path %q", err, shareDir)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			t.Cleanup(func() { _ = bc.Close() })
		})
	}
}

// TestNewFSStore_DeepBlkFile asserts the gate's depth cap of 3
// directories under baseDir: legacy `.blk` at the canonical legacy
// depth (<= 3) is detected; a `.blk` planted past depth 3 is
// intentionally NOT detected. Documented as a perf optimization —
// legacy `.blk` files always lived at <share>/<shard>/<payloadID>/<idx>.blk.
func TestNewFSStore_DeepBlkFile(t *testing.T) {
	t.Run("legacy_depth_detected", func(t *testing.T) {
		shareDir := t.TempDir()
		dir := filepath.Join(shareDir, "ab", "payload-001")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0.blk"), []byte("x"), 0644); err != nil {
			t.Fatalf("write blk: %v", err)
		}
		mds := memory.NewMemoryMetadataStoreWithDefaults()
		_, err := New(shareDir, 0, 256*1024*1024, mds)
		if !errors.Is(err, blockstore.ErrLegacyLayoutDetected) {
			t.Fatalf("expected ErrLegacyLayoutDetected at legacy depth; got %v", err)
		}
	})

	t.Run("beyond_depth_cap_not_detected", func(t *testing.T) {
		shareDir := t.TempDir()
		// Plant a .blk file at depth 5 — past the legacy layout's
		// depth=3. This is a regression guard against future unbounded
		// WalkDir on huge stores.
		deep := filepath.Join(shareDir, "d1", "d2", "d3", "d4", "d5")
		if err := os.MkdirAll(deep, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(deep, "stray.blk"), []byte("x"), 0644); err != nil {
			t.Fatalf("write blk: %v", err)
		}
		mds := memory.NewMemoryMetadataStoreWithDefaults()
		bc, err := New(shareDir, 0, 256*1024*1024, mds)
		if err != nil {
			t.Fatalf("expected success (depth>3 .blk not detected); got %v", err)
		}
		t.Cleanup(func() { _ = bc.Close() })
	})
}

// TestNewFSStoreForMigration_BypassesSentinel asserts the bypass
// constructor opens an FSStore against the very state the legacy gate
// refuses (sentinel-missing, .blk-present). This is the entry point
// the `dfs migrate-to-cas` subcommand uses to process legacy data.
func TestNewFSStoreForMigration_BypassesSentinel(t *testing.T) {
	shareDir := t.TempDir()
	writeLegacyBlkForTest(t, shareDir)

	// Confirm the production constructor refuses, so we know the
	// bypass is actually being exercised by the next call.
	if _, err := New(shareDir, 0, 256*1024*1024, memory.NewMemoryMetadataStoreWithDefaults()); !errors.Is(err, blockstore.ErrLegacyLayoutDetected) {
		t.Fatalf("precondition: New should refuse legacy layout; got %v", err)
	}

	bc, err := NewFSStoreForMigration(shareDir, 0, 256*1024*1024,
		memory.NewMemoryMetadataStoreWithDefaults(), FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewFSStoreForMigration: expected success on legacy layout; got %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
}
