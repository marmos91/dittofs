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

// TestAppendWrite_DisabledByDefault verifies D-03 / D-36: when the flag is
// false (New() / NewWithOptions zero-value), AppendWrite returns
// ErrAppendLogDisabled and — as a side-observation — no log file is
// created on disk.
func TestAppendWrite_DisabledByDefault(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	err := bc.AppendWrite(context.Background(), "file1", []byte("hi"), 0)
	if err != ErrAppendLogDisabled {
		t.Fatalf("want ErrAppendLogDisabled, got %v", err)
	}
	// D-36 side-check: no log file materialized on disk.
	if _, statErr := os.Stat(filepath.Join(bc.baseDir, "logs", "file1.log")); statErr == nil {
		t.Fatal("flag=false created a log file on disk; D-36 violated")
	}
}

// TestAppendWrite_Enabled_HappyPath writes three records and verifies:
//   - the on-disk log has header + 3 records of the expected total size,
//   - logBytesTotal counts the framed-record overhead (not just payload),
//   - the interval tree gains exactly 3 entries.
func TestAppendWrite_Enabled_HappyPath(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true, MaxLogBytes: 1 << 30})
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
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
	_ = bc.Close()
	err := bc.AppendWrite(context.Background(), "file1", []byte("hi"), 0)
	if err != ErrStoreClosed {
		t.Fatalf("want ErrStoreClosed, got %v", err)
	}
}

// TestAppendWrite_CtxCanceled verifies the pre-work ctx.Err() guard.
func TestAppendWrite_CtxCanceled(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
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
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true, MaxLogBytes: 1 << 30})
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
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true, MaxLogBytes: 1})
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
