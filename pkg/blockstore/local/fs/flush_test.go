package fs

import (
	"bytes"
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newFSStoreForFlushTest builds a barebones FSStore suitable for flush concurrency tests.
func newFSStoreForFlushTest(t *testing.T) *FSStore {
	t.Helper()
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := New(dir, 0, 0, mds)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// stageMemBlockDirectly bypasses tryDirectDiskWrite by manipulating the
// memBlock buffer directly. WriteAt routes sub-block writes through
// tryDirectDiskWrite when an on-disk file exists, which would defeat
// memBlock-focused tests.
func stageMemBlockDirectly(t *testing.T, bc *FSStore, payloadID string, blockIdx uint64, data []byte) *memBlock {
	t.Helper()
	mb := bc.getOrCreateMemBlock(blockKey{payloadID: payloadID, blockIdx: blockIdx})
	mb.mu.Lock()
	if mb.data == nil {
		mb.data = getBlockBuf()
		bc.memUsed.Add(int64(blockstore.BlockSize))
	}
	copy(mb.data, data)
	mb.dataSize = uint32(len(data))
	mb.dirty = true
	mb.lastWrite = time.Now()
	mb.mu.Unlock()
	bc.updateFileSize(payloadID, blockIdx*blockstore.BlockSize+uint64(len(data)))
	return mb
}

// TestFlushBlock_DoesNotBlockReaderDuringDiskIO asserts that a concurrent
// reader holding mb.mu.RLock() for a brief check is NOT blocked for the full
// duration of the flush's disk I/O. With stage-and-release the lock is held
// only for bytes.Clone + the short post-write flag flip.
func TestFlushBlock_DoesNotBlockReaderDuringDiskIO(t *testing.T) {
	bc := newFSStoreForFlushTest(t)
	// Use a small payload so bytes.Clone time is negligible vs the fsync.
	// With stage-and-release, lock-held time ≈ clone time; disk fsync
	// dominates. A 4 KiB clone is sub-microsecond — any meaningful reader
	// wait must come from the disk I/O happening under the lock (the bug).
	data := bytes.Repeat([]byte{0xAB}, 4096)
	mb := stageMemBlockDirectly(t, bc, "p1", 0, data)

	// Reader goroutine repeatedly grabs mb.mu.RLock() and records max wait.
	var maxWait atomic.Int64 // nanoseconds
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			mb.mu.RLock()
			elapsed := time.Since(start).Nanoseconds()
			mb.mu.RUnlock()
			if elapsed > maxWait.Load() {
				maxWait.Store(elapsed)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()

	flushStart := time.Now()
	if _, _, err := bc.flushBlock(context.Background(), "p1", 0, mb, true); err != nil {
		t.Fatalf("flushBlock: %v", err)
	}
	flushTotal := time.Since(flushStart)

	close(stop)
	wg.Wait()

	// With stage-and-release, the cumulative lock-held time is bounded by
	// the bytes.Clone copy of an 8MB buffer — far less than the disk fsync.
	// We require maxWait <= flushTotal/2 as a generous bound that still
	// fails when disk I/O happens under the lock.
	if flushTotal > 2*time.Millisecond && maxWait.Load() > flushTotal.Nanoseconds()/2 {
		t.Fatalf("concurrent reader was blocked for %v out of total flush %v — disk I/O likely held mb.mu (TD-09)",
			time.Duration(maxWait.Load()), flushTotal)
	}
}

// TestFlushBlock_OnDiskMatchesStagedSnapshot asserts the on-disk file
// matches the bytes captured at flush-time, even when a concurrent writer
// mutates the in-memory buffer DURING the disk I/O. Exercises the
// bytes.Clone snapshot in stage-and-release.
func TestFlushBlock_OnDiskMatchesStagedSnapshot(t *testing.T) {
	bc := newFSStoreForFlushTest(t)
	original := bytes.Repeat([]byte{0xAA}, 4096)
	mb := stageMemBlockDirectly(t, bc, "p1", 0, original)

	// Goroutine that hammers mb.data with 0xBB once flushBlock is running.
	// We can't synchronize precisely without an injection hook, so we
	// busy-wait and mutate as fast as possible — if stage-and-release is
	// correct, the staged clone is unaffected.
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		mutated := bytes.Repeat([]byte{0xBB}, 4096)
		for !stop.Load() {
			mb.mu.Lock()
			if mb.data != nil && mb.dirty {
				copy(mb.data, mutated)
			}
			mb.mu.Unlock()
		}
	}()

	path, sz, err := bc.flushBlock(context.Background(), "p1", 0, mb, true)
	stop.Store(true)
	wg.Wait()

	if err != nil {
		t.Fatalf("flushBlock: %v", err)
	}
	if sz != 4096 {
		t.Fatalf("expected dataSize 4096, got %d", sz)
	}

	// Read the on-disk .blk file directly. The bytes must be internally
	// consistent — all bytes match either the original or the mutated value,
	// with no torn mix.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read on-disk file: %v", err)
	}
	allOriginal := bytes.Equal(onDisk[:4096], original)
	allMutated := bytes.Equal(onDisk[:4096], bytes.Repeat([]byte{0xBB}, 4096))
	if !allOriginal && !allMutated {
		t.Fatalf("on-disk bytes are torn (mix of 0xAA and 0xBB) — stage clone failed")
	}
}

// TestFlushBlock_StateCoherenceAfterSuccess asserts that a successful flush
// of a memBlock with no concurrent writers leaves mb.dirty=false, mb.data=nil.
func TestFlushBlock_StateCoherenceAfterSuccess(t *testing.T) {
	bc := newFSStoreForFlushTest(t)
	data := bytes.Repeat([]byte{0xCC}, 4096)
	mb := stageMemBlockDirectly(t, bc, "p1", 0, data)

	if _, _, err := bc.flushBlock(context.Background(), "p1", 0, mb, true); err != nil {
		t.Fatalf("flushBlock: %v", err)
	}

	mb.mu.RLock()
	defer mb.mu.RUnlock()
	if mb.dirty {
		t.Errorf("mb.dirty should be false after successful flush, got true")
	}
	if mb.data != nil {
		t.Errorf("mb.data should be nil after successful flush, got non-nil")
	}
	if mb.dataSize != 0 {
		t.Errorf("mb.dataSize should be 0 after successful flush, got %d", mb.dataSize)
	}
}

// TestFlushBlock_DiskUsedUpdatedOnSuccess asserts that bc.diskUsed reflects
// the new on-disk size after a successful flush.
func TestFlushBlock_DiskUsedUpdatedOnSuccess(t *testing.T) {
	bc := newFSStoreForFlushTest(t)
	data := bytes.Repeat([]byte{0xDD}, 4096)
	mb := stageMemBlockDirectly(t, bc, "p1", 0, data)

	beforeDisk := bc.diskUsed.Load()
	if _, _, err := bc.flushBlock(context.Background(), "p1", 0, mb, true); err != nil {
		t.Fatalf("flushBlock: %v", err)
	}
	afterDisk := bc.diskUsed.Load()
	if afterDisk-beforeDisk != 4096 {
		t.Fatalf("diskUsed delta = %d, want 4096", afterDisk-beforeDisk)
	}
}

// TestFlushBlock_NoOpOnCleanBlock asserts flushBlock is a no-op when the
// block is not dirty.
func TestFlushBlock_NoOpOnCleanBlock(t *testing.T) {
	bc := newFSStoreForFlushTest(t)
	data := bytes.Repeat([]byte{0xEE}, 4096)
	mb := stageMemBlockDirectly(t, bc, "p1", 0, data)

	// First flush: dirty -> on disk.
	if _, _, err := bc.flushBlock(context.Background(), "p1", 0, mb, true); err != nil {
		t.Fatalf("flushBlock 1: %v", err)
	}
	// Second flush: clean (data nil, dirty false) -> no-op.
	path, sz, err := bc.flushBlock(context.Background(), "p1", 0, mb, true)
	if err != nil {
		t.Fatalf("flushBlock 2: %v", err)
	}
	if path != "" || sz != 0 {
		t.Fatalf("expected no-op, got path=%q size=%d", path, sz)
	}
}

// TestFlushBlock_PressureDrivenDoesNotFsync asserts that
// flushOldestDirtyBlock (the memory-pressure path) flushes a block to disk
// without invoking fsync, while the explicit Flush (NFS COMMIT) path does.
// Regression guard for the Copilot review on PR #453: previously
// flushBlock unconditionally fsynced, which made every pressure-driven
// flush pay a per-block durability cost on the write hot path.
func TestFlushBlock_PressureDrivenDoesNotFsync(t *testing.T) {
	bc := newFSStoreForFlushTest(t)
	data := bytes.Repeat([]byte{0xAB}, 4096)

	// Pressure-driven path: stage a dirty memBlock and run
	// flushOldestDirtyBlock directly. Counter must not advance.
	_ = stageMemBlockDirectly(t, bc, "p-pressure", 0, data)
	before := bc.FlushFsyncCountForTest()
	if !bc.flushOldestDirtyBlock(context.Background()) {
		t.Fatalf("flushOldestDirtyBlock returned false; expected a flush")
	}
	if got := bc.FlushFsyncCountForTest(); got != before {
		t.Fatalf("pressure-driven flush bumped fsync counter %d -> %d; expected no fsync",
			before, got)
	}

	// COMMIT path: same payload, fresh dirty memBlock. Counter must advance.
	_ = stageMemBlockDirectly(t, bc, "p-commit", 0, data)
	before = bc.FlushFsyncCountForTest()
	if _, err := bc.Flush(context.Background(), "p-commit"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := bc.FlushFsyncCountForTest(); got <= before {
		t.Fatalf("COMMIT-driven flush did not bump fsync counter (before=%d after=%d)",
			before, got)
	}
}
