package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// writeSentinelForTest writes a minimal valid `.cas-migrated-v1` marker
// at the share-dir root. Mirrors pkg/block/migrate.writeSentinel's
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
		bc, err := NewWithOptions(dir, 0, blockStore, FSStoreOptions{})
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
			bc, err := NewWithOptions(shareDir, 0, mds, FSStoreOptions{})
			if tc.wantLegacy {
				if err == nil {
					_ = bc.Close()
					t.Fatalf("expected ErrLegacyLayoutDetected, got nil")
				}
				if !errors.Is(err, block.ErrLegacyLayoutDetected) {
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
		_, err := NewWithOptions(shareDir, 0, mds, FSStoreOptions{})
		if !errors.Is(err, block.ErrLegacyLayoutDetected) {
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
		bc, err := NewWithOptions(shareDir, 0, mds, FSStoreOptions{})
		if err != nil {
			t.Fatalf("expected success (depth>3 .blk not detected); got %v", err)
		}
		t.Cleanup(func() { _ = bc.Close() })
	})
}

// ---: FSStoreOptions OnChunkComplete. ---
//
// These tests assert the OnChunkComplete callback is stored on the
// FSStore struct without altering pre-Phase-19 lruTouch behavior
// (nil-safety contract — see 19-CONTEXT.md).

// TestFSStore_NilOnChunkComplete_LruTouchUnchanged is the
// nil-safety gate: with no OnChunkComplete configured, StoreChunk must
// still succeed and lruTouch must behave identically to pre-Phase-19
// (no panic; the absent callback is unreferenced). chunkstore.go is
// unmodified in — this is a regression guard, not an active
// wire-in test.
func TestFSStore_NilOnChunkComplete_LruTouchUnchanged(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	if cb := bc.onChunkComplete.Load(); cb == nil || cb.fn != nil {
		t.Fatalf("bc.onChunkComplete.fn must be nil when option is unset; holder=%v", cb)
	}
	h := hashFromHex(t, strings.Repeat("19", 32))
	data := bytes.Repeat([]byte{0x19}, 256)
	ctx := context.Background()
	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk with nil OnChunkComplete: %v", err)
	}
	// Confirm the chunk was recorded (nil callback must not affect storage).
	exists, err := bc.HasChunk(ctx, h)
	if err != nil || !exists {
		t.Fatalf("chunk missing after StoreChunk: exists=%v err=%v", exists, err)
	}
}

// TestFSStore_OnChunkComplete_StoredOnConstruction asserts a non-nil
// callback passed via FSStoreOptions lands on the FSStore struct.
// will fire it from chunkstore.lruTouch; only stores.
func TestFSStore_OnChunkComplete_StoredOnConstruction(t *testing.T) {
	var calls atomic.Int64
	cb := func(_ block.ContentHash, _ []byte, _ string) {
		calls.Add(1)
	}
	bc := newFSStoreForTest(t, FSStoreOptions{OnChunkComplete: cb})
	holder := bc.onChunkComplete.Load()
	if holder == nil || holder.fn == nil {
		t.Fatal("bc.onChunkComplete.fn must be non-nil after construction with explicit callback")
	}
	// Fire the stored callback directly to confirm it is the value we
	// passed in (function identity check — Go does not permit ==
	// comparison of func values, so invoke and observe the counter).
	holder.fn(block.ContentHash{}, nil, "")
	if got := calls.Load(); got != 1 {
		t.Fatalf("stored callback fired %d times; want 1", got)
	}
}

// TestFSStore_SetOnChunkComplete_PostHocInstall asserts the setter
// installs the callback after construction. Engine wiring order
// (Cache materializes in BlockStore.Start, AFTER cfg.Local was
// already constructed) requires post-hoc install through this setter
// — see PATTERNS.md "Lifecycle: callback installed via ... settable
// via setter (mirror SetObjectIDPersister)".
func TestFSStore_SetOnChunkComplete_PostHocInstall(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	if cb := bc.onChunkComplete.Load(); cb == nil || cb.fn != nil {
		t.Fatal("precondition: onChunkComplete.fn must start nil")
	}
	var calls atomic.Int64
	bc.SetOnChunkComplete(func(_ block.ContentHash, _ []byte, _ string) {
		calls.Add(1)
	})
	holder := bc.onChunkComplete.Load()
	if holder == nil || holder.fn == nil {
		t.Fatal("SetOnChunkComplete must populate bc.onChunkComplete.fn")
	}
	holder.fn(block.ContentHash{}, nil, "")
	if got := calls.Load(); got != 1 {
		t.Fatalf("installed callback fired %d times; want 1", got)
	}
}
