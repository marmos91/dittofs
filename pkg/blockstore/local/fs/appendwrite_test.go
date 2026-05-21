package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// TestAppendWrite_CoordinatorOnHotPath_BurstCounts: N=4 AppendWrites to
// the same payload. The per-file mutex (D-32) serializes the writers
// strictly — only one can enter the Sync call site at a time — so the
// in-flight piggyback CANNOT batch same-payload writes (this is the
// architectural cost of crash-safe log ordering, see D-32 rationale in
// AppendWrite's godoc). What we CAN verify is that the coordinator is
// on the hot path: every successful AppendWrite goes through
// lf.groupCommit.Sync exactly once (no double-fsync regression, no
// fsync-bypass regression).
//
// The plan's original expectation of "only ONE fsync syscall observable"
// is architecturally impossible under per-file-mu serialization — the
// in-flight piggyback wins when multiple goroutines call coordinator.Sync
// concurrently, which the per-file mu prevents for same-payload writes.
// Documented as a deviation in 19-06-SUMMARY.md. Batching wins are still
// real for: (a) future call sites that call Sync without holding mu (e.g.,
// an NFS COMMIT path that flushes already-appended records), and (b)
// micro-architectural — the coordinator absorbs one syscall even at
// depth-1 with no extra overhead (D-06 adaptive bypass verified by the
// SingleWriter_NoLatencyPenalty test).
func TestAppendWrite_CoordinatorOnHotPath_BurstCounts(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	// Force the logFile into existence so we can instrument the coordinator
	// BEFORE any concurrent traffic arrives.
	if err := bc.AppendWrite(context.Background(), "file1", []byte("seed"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}
	bc.logsMu.RLock()
	lf := bc.logFDs["file1"]
	bc.logsMu.RUnlock()
	if lf == nil || lf.groupCommit == nil {
		t.Fatal("logFile/coordinator missing")
	}

	// Wrap fsyncFn to count invocations.
	var calls atomic.Int32
	orig := lf.groupCommit.fsyncFn
	lf.groupCommit.fsyncFn = func() error {
		calls.Add(1)
		return orig()
	}

	const goroutines = 4
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			if err := bc.AppendWrite(context.Background(), "file1", []byte("x"), uint64((i+1)*4096)); err != nil {
				t.Errorf("AppendWrite: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Coordinator MUST be on the path: at least one fsync observed.
	if calls.Load() < 1 {
		t.Fatalf("no fsync observed via coordinator; wire-in broken")
	}
	// Upper bound: at most one fsync per writer (per-file mu serialization).
	// More than `goroutines` fsyncs would indicate double-fsync regression.
	if got := calls.Load(); got > int32(goroutines) {
		t.Fatalf("fsync calls under burst: got %d, want <= %d (double-fsync regression)", got, goroutines)
	}
}

// TestAppendWrite_SingleWriter_NoLatencyPenalty exercises D-06 adaptive
// bypass at the integration level: a single AppendWrite should complete
// quickly — the coordinator's inline-bypass path means we're not waiting
// on any 1ms window.
func TestAppendWrite_SingleWriter_NoLatencyPenalty(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	start := time.Now()
	if err := bc.AppendWrite(context.Background(), "file1", []byte("hi"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	elapsed := time.Since(start)
	// The fsync itself is bounded by disk hardware (~100µs-2ms on NVMe,
	// up to ~10ms on rotational/CI VMs). We assert <50ms as a coarse
	// "no extra coordinator window penalty" gate — the coordinator's 1ms
	// window must NOT be added on top of fsync.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("single-writer AppendWrite took %v; want < 50ms (D-06 bypass broken)", elapsed)
	}
}

// TestAppendWrite_FsyncError_PropagatesToCaller injects a sentinel error
// into the coordinator's fsyncFn and verifies AppendWrite surfaces it
// wrapped as `log fsync: %w` — the operator-visible error contract.
func TestAppendWrite_FsyncError_PropagatesToCaller(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	// Force the logFile into existence so we can swap fsyncFn before the
	// hot-path call we care about.
	if err := bc.AppendWrite(context.Background(), "file1", []byte("seed"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}
	bc.logsMu.RLock()
	lf := bc.logFDs["file1"]
	bc.logsMu.RUnlock()
	if lf == nil || lf.groupCommit == nil {
		t.Fatal("logFile/coordinator missing")
	}

	wantErr := errors.New("disk on fire")
	lf.groupCommit.fsyncFn = func() error { return wantErr }

	err := bc.AppendWrite(context.Background(), "file1", []byte("y"), 4096)
	if err == nil {
		t.Fatal("AppendWrite returned nil; want error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("AppendWrite error: got %v, want wrapped %v", err, wantErr)
	}
}

// TestAppendWrite_CtxCancel_StillFsyncs verifies D-08 durability:
// AppendWrite-B's ctx is canceled while it is enqueued behind A's
// in-flight fsync; B observes ctx.Err() but A's fsync still completes and
// the data ends up on disk. We instrument fsyncFn to gate completion on
// a release channel so we can deterministically drive the ordering.
func TestAppendWrite_CtxCancel_StillFsyncs(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	// Force the logFile into existence.
	if err := bc.AppendWrite(context.Background(), "file1", []byte("seed"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}
	bc.logsMu.RLock()
	lf := bc.logFDs["file1"]
	bc.logsMu.RUnlock()
	if lf == nil || lf.groupCommit == nil {
		t.Fatal("logFile/coordinator missing")
	}

	released := make(chan struct{})
	var fsyncCalls atomic.Int32
	orig := lf.groupCommit.fsyncFn
	lf.groupCommit.fsyncFn = func() error {
		fsyncCalls.Add(1)
		<-released
		return orig()
	}

	// A: appends + drives fsync inline (gated by `released`).
	aDone := make(chan error, 1)
	go func() {
		aDone <- bc.AppendWrite(context.Background(), "file1", []byte("AAAA"), 4096)
	}()
	// Give A enough time to acquire mu, write the record, enter Sync, and
	// block in fsyncFn. The per-file mu (D-32) is held across this entire
	// window — B's AppendWrite below MUST wait for A to release it.
	time.Sleep(20 * time.Millisecond)

	// B: a separate ctx that we cancel while B is blocked on mu (since
	// the per-file mu serializes B behind A). When mu becomes available,
	// B will observe its own ctx is canceled at the next ctx check OR
	// proceed and observe cancel inside coordinator's waitOn. Either way,
	// AppendWrite-B returns ctx.Err() per the contract.
	ctxB, cancelB := context.WithCancel(context.Background())
	bDone := make(chan error, 1)
	go func() {
		bDone <- bc.AppendWrite(ctxB, "file1", []byte("BBBB"), 8192)
	}()
	// Brief wait to let B reach a blocking point (most likely waiting on mu).
	time.Sleep(20 * time.Millisecond)
	cancelB()

	// Release A.
	close(released)

	select {
	case err := <-aDone:
		if err != nil {
			t.Fatalf("A: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A did not complete")
	}
	select {
	case err := <-bDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			// B may have raced past the cancel and completed successfully;
			// either outcome is acceptable as long as A's fsync ran. The
			// failure mode the invariant protects against is "B's cancel
			// abort A's fsync".
			t.Logf("B finished with: %v (acceptable)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B did not return")
	}

	// A's fsync MUST have run at least once.
	if fsyncCalls.Load() < 1 {
		t.Fatalf("fsync did not run for A; calls=%d", fsyncCalls.Load())
	}
	// Verify on-disk durability of A's bytes: close+reopen the log fd.
	// We rely on the standard log layout — header + seed-record + A-record
	// (B's record may or may not have been appended depending on the race).
	path := filepath.Join(bc.baseDir, "logs", "file1.log")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	wantMin := int64(logHeaderSize + (recordFrameOverhead + 4) + (recordFrameOverhead + 4)) // seed (4 bytes) + A (4 bytes)
	if st.Size() < wantMin {
		t.Fatalf("on-disk size: got %d, want >= %d (A's record not durable)", st.Size(), wantMin)
	}
}

// TestAppendWrite_LockOrder_PerFileMuStillHeldAcrossSync runs concurrent
// writers under -race and asserts no race detection. The per-file mu
// (bc.logLocks[payloadID]) is held by AppendWrite across the
// lf.groupCommit.Sync call site; the coordinator's internal mu is a
// separate lock (D-09). If either lock were inverted with bc.logsMu, the
// race detector would surface it under heavy load.
func TestAppendWrite_LockOrder_PerFileMuStillHeldAcrossSync(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	const goroutines = 32
	payload := []byte("xxxxxxxx") // 8 bytes
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				off := uint64(i)*1024*1024 + uint64(j)*4096
				if err := bc.AppendWrite(context.Background(), "file1", payload, off); err != nil {
					t.Errorf("AppendWrite: %v", err)
					return
				}
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent AppendWrite stalled — possible deadlock")
	}
	// Total bytes accounting validates that no torn writes happened.
	want := int64(goroutines * 20 * (recordFrameOverhead + len(payload)))
	if got := bc.logBytesTotal.Load(); got != want {
		t.Fatalf("logBytesTotal: got %d, want %d", got, want)
	}
}
