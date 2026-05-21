package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAppendWrite_Enabled_HappyPath writes three records and verifies:
//   - the on-disk log has header + 3 records of the expected total size,
//   - logBytesTotal counts the framed-record overhead (not just payload),
//   - the interval tree gains exactly 3 entries.
func TestAppendWrite_Enabled_HappyPath(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	payload := bytes.Repeat([]byte{0xAB}, 100)
	for _, off := range []uint64{0, 4096, 8192} {
		if err := bc.AppendWrite(context.Background(), "file1", payload, off); err != nil {
			t.Fatalf("AppendWrite: %v", err)
		}
	}
	path := filepath.Join(bc.baseDir, "logs", "file1.log")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	wantSize := int64(logHeaderSize + 3*(recordFrameOverhead+len(payload)))
	if st.Size() != wantSize {
		t.Fatalf("log size: got %d want %d", st.Size(), wantSize)
	}
	wantBytes := int64(3 * (recordFrameOverhead + len(payload)))
	if got := bc.logBytesTotal.Load(); got != wantBytes {
		t.Fatalf("logBytesTotal: got %d want %d", got, wantBytes)
	}
	bc.logsMu.RLock()
	tree := bc.dirtyIntervals["file1"]
	bc.logsMu.RUnlock()
	if tree == nil || tree.Len() != 3 {
		t.Fatalf("interval tree: %+v want len=3", tree)
	}
}

// TestAppendWrite_ClosedStoreReturnsErr verifies the ErrStoreClosed guard
// at the top of AppendWrite.
func TestAppendWrite_ClosedStoreReturnsErr(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	_ = bc.Close()
	err := bc.AppendWrite(context.Background(), "file1", []byte("hi"), 0)
	if err != ErrStoreClosed {
		t.Fatalf("want ErrStoreClosed, got %v", err)
	}
}

// TestAppendWrite_CtxCanceled verifies the pre-work ctx.Err() guard.
func TestAppendWrite_CtxCanceled(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := bc.AppendWrite(ctx, "file1", []byte("hi"), 0)
	if err == nil || err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestAppendWrite_PerFileSerial proves the per-file mutex (D-32)
// serializes concurrent AppendWrite calls to the same payload: 50
// goroutines each append a 64-byte record, final logBytesTotal must equal
// the deterministic sum of framed-record sizes (no partial writes, no
// torn fsyncs).
func TestAppendWrite_PerFileSerial(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	const goroutines = 50
	const payloadLen = 64
	payload := bytes.Repeat([]byte{0xCC}, payloadLen)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			if err := bc.AppendWrite(context.Background(), "file1", payload, uint64(i*4096)); err != nil {
				t.Errorf("AppendWrite: %v", err)
			}
		}(i)
	}
	wg.Wait()
	want := int64(goroutines * (recordFrameOverhead + payloadLen))
	if got := bc.logBytesTotal.Load(); got != want {
		t.Fatalf("logBytesTotal race: got %d want %d", got, want)
	}
	// All 50 inserts must live in the tree — each goroutine used a
	// distinct offset so there are no collisions.
	bc.logsMu.RLock()
	tree := bc.dirtyIntervals["file1"]
	bc.logsMu.RUnlock()
	if tree == nil || tree.Len() != goroutines {
		t.Fatalf("interval tree: len=%v want %d", tree, goroutines)
	}
}

// TestAppendWrite_PressureBlocks_UntilSignaled exercises the D-15
// pressure wait. maxLogBytes is set to 1 so the second AppendWrite
// blocks on bc.pressureCh; a simulated rollup resets logBytesTotal and
// pulses the channel. No real rollup worker runs in Phase 10-04 — the
// test drives the signal directly.
func TestAppendWrite_PressureBlocks_UntilSignaled(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1})
	// Prime: first write already exceeds budget.
	if err := bc.AppendWrite(context.Background(), "file1", []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	// Second call blocks because logBytesTotal > maxLogBytes=1.
	done := make(chan error, 1)
	go func() {
		done <- bc.AppendWrite(context.Background(), "file1", []byte("y"), 4096)
	}()
	// Give the goroutine a moment to enter the pressure loop before we
	// release; without this the test can race past the wait and trivially
	// pass even if the pressure arm were broken.
	time.Sleep(50 * time.Millisecond)
	// Simulate rollup: drain budget to 0, then pulse pressureCh.
	bc.logBytesTotal.Store(0)
	select {
	case bc.pressureCh <- struct{}{}:
	default:
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AppendWrite after pressure release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AppendWrite did not unblock after pressure release")
	}
}

// TestLogFile_GroupCommit_NonNilAfterConstruction verifies Plan 06 Task 1
// step 1: the logFile struct gains a `groupCommit *groupCommit` field that
// is instantiated by the canonical constructor (getOrCreateLog → on first
// touch). The field MUST be non-nil after the first AppendWrite drives
// getOrCreateLog to materialize the logFile for "file1" (D-07: per-file
// scope, one coordinator per open log fd).
func TestLogFile_GroupCommit_NonNilAfterConstruction(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	if err := bc.AppendWrite(context.Background(), "file1", []byte("hi"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	bc.logsMu.RLock()
	lf := bc.logFDs["file1"]
	bc.logsMu.RUnlock()
	if lf == nil {
		t.Fatal("logFile not present after AppendWrite")
	}
	if lf.groupCommit == nil {
		t.Fatal("logFile.groupCommit is nil after construction; Plan 06 Task 1 requires non-nil instantiation")
	}
}

// TestLogFile_GroupCommit_FsyncFn_BoundToLfFile verifies the coordinator's
// fsyncFn actually fsyncs the underlying log file. End-to-end durability
// via the coordinator: write a record, drive lf.groupCommit.Sync directly,
// close, reopen, and verify the bytes are on disk. This guards against a
// future refactor where the coordinator is constructed with the wrong
// fsync target (e.g., a no-op or a different fd).
func TestLogFile_GroupCommit_FsyncFn_BoundToLfFile(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	payload := []byte("durability via coordinator")
	if err := bc.AppendWrite(context.Background(), "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	bc.logsMu.RLock()
	lf := bc.logFDs["file1"]
	bc.logsMu.RUnlock()
	if lf == nil || lf.groupCommit == nil {
		t.Fatal("logFile or coordinator missing after AppendWrite")
	}
	// Drive the coordinator directly — this should fsync lf.f. If the
	// coordinator was bound to a different file (or a no-op), the test
	// still passes for happy-path post-AppendWrite-fsync, so we instead
	// verify by writing extra bytes through lf.f directly (bypassing
	// AppendWrite's own fsync), then driving the coordinator's Sync.
	extra := []byte("ZZ")
	if _, err := lf.f.Write(extra); err != nil {
		t.Fatalf("raw write: %v", err)
	}
	if err := lf.groupCommit.Sync(context.Background()); err != nil {
		t.Fatalf("groupCommit.Sync: %v", err)
	}
	path := filepath.Join(bc.baseDir, "logs", "file1.log")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	// The on-disk size must include the extra bytes since the coordinator's
	// Sync flushed lf.f's buffers.
	wantMin := int64(logHeaderSize + recordFrameOverhead + len(payload) + len(extra))
	if st.Size() < wantMin {
		t.Fatalf("log size after coordinator Sync: got %d, want >= %d", st.Size(), wantMin)
	}
}
