package fs

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"testing"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// snapshotLogIndex returns a copy of the per-payload logIndex entries
// under both bc.logsMu and the per-file mu so the snapshot is consistent
// with concurrent AppendWriters. Test-only helper.
func snapshotLogIndex(t *testing.T, bc *FSStore, payloadID string) []logEntry {
	t.Helper()
	bc.logsMu.RLock()
	idx := bc.logIndices[payloadID]
	mu := bc.logLocks[payloadID]
	bc.logsMu.RUnlock()
	if idx == nil || mu == nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	out := make([]logEntry, len(idx.entries))
	copy(out, idx.entries)
	return out
}

// TestLogIndex_PopulatedFromAppendWrite_Sequential verifies that every
// AppendWrite produces exactly one logIndex entry, with logPos pointing
// at the frame boundary the writer wrote to.
func TestLogIndex_PopulatedFromAppendWrite_Sequential(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	payload := bytes.Repeat([]byte{0x42}, 512)
	offsets := []uint64{0, 4096, 8192, 12288}
	for _, off := range offsets {
		if err := bc.AppendWrite(context.Background(), "file-seq", payload, off); err != nil {
			t.Fatalf("AppendWrite: %v", err)
		}
	}
	entries := snapshotLogIndex(t, bc, "file-seq")
	if len(entries) != len(offsets) {
		t.Fatalf("entry count: got %d want %d", len(entries), len(offsets))
	}
	// Expected logPos walk: header, then each prior frame.
	step := uint64(recordFrameOverhead) + uint64(len(payload))
	wantLogPos := uint64(logHeaderSize)
	for i, e := range entries {
		if e.logPos != wantLogPos {
			t.Fatalf("entry[%d].logPos: got %d want %d", i, e.logPos, wantLogPos)
		}
		if e.fileOff != offsets[i] {
			t.Fatalf("entry[%d].fileOff: got %d want %d", i, e.fileOff, offsets[i])
		}
		if e.payloadLen != uint32(len(payload)) {
			t.Fatalf("entry[%d].payloadLen: got %d want %d", i, e.payloadLen, len(payload))
		}
		wantLogPos += step
	}
}

// TestLogIndex_PopulatedFromAppendWrite_Concurrent fires N goroutines at
// the same payload with distinct file offsets and verifies the logIndex
// captures every record. logPos must be strictly ascending (per-file mu
// serializes the increment); the set of fileOffsets must match the set
// of writers.
func TestLogIndex_PopulatedFromAppendWrite_Concurrent(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	const goroutines = 64
	const payloadLen = 256
	payload := bytes.Repeat([]byte{0xAA}, payloadLen)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			if err := bc.AppendWrite(context.Background(), "file-conc", payload, uint64(i*8192)); err != nil {
				t.Errorf("AppendWrite: %v", err)
			}
		}(i)
	}
	wg.Wait()

	entries := snapshotLogIndex(t, bc, "file-conc")
	if len(entries) != goroutines {
		t.Fatalf("entry count: got %d want %d", len(entries), goroutines)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].logPos <= entries[i-1].logPos {
			t.Fatalf("logPos not strictly ascending at i=%d: %+v", i, entries[i-1:i+1])
		}
	}
	gotOffs := make([]uint64, len(entries))
	for i, e := range entries {
		gotOffs[i] = e.fileOff
	}
	sort.Slice(gotOffs, func(a, b int) bool { return gotOffs[a] < gotOffs[b] })
	for i, off := range gotOffs {
		if off != uint64(i*8192) {
			t.Fatalf("sorted fileOff[%d]: got %d want %d", i, off, i*8192)
		}
	}
}

// TestLogIndex_OutOfOrderArrivals_RecoverableByLookup is the end-to-end
// regression case. Several goroutines write to interleaved file offsets
// after they drain, an EntriesForInterval query against a window that
// includes the LATEST-arrived file offset (which lands in the middle of
// the log, not at its head) must surface the matching record. This is
// the failure mode that produced silent recs=0 rollups under macOS
// NFSv3 parallel writes.
func TestLogIndex_OutOfOrderArrivals_RecoverableByLookup(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	// Sequential arrivals at distinct, NON-MONOTONIC file offsets — the
	// per-file mu enforces serialized log-append order, so we exercise
	// the bug shape (file_off NOT in arrival order) with deterministic
	// arrival order.
	const payloadLen = 32 * 1024
	payload := bytes.Repeat([]byte{0xCC}, payloadLen)
	arrivals := []uint64{32768, 458752, 0, 1540096, 524288}
	for _, off := range arrivals {
		if err := bc.AppendWrite(context.Background(), "file-ooo", payload, off); err != nil {
			t.Fatalf("AppendWrite at %d: %v", off, err)
		}
	}

	bc.logsMu.RLock()
	idx := bc.logIndices["file-ooo"]
	mu := bc.logLocks["file-ooo"]
	bc.logsMu.RUnlock()
	mu.Lock()
	// Query [0, 65536) — must find both fileOff=32768 (rec#0) and
	// fileOff=0 (rec#2), the latter buried after a higher-offset record
	// in the log. This is the bug shape from the proposal example.
	hits := idx.EntriesForInterval(0, 65536)
	mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("EntriesForInterval([0,65536)): got %d entries want 2 (%+v)", len(hits), hits)
	}
	wantFileOffs := []uint64{32768, 0} // arrival order
	for i, e := range hits {
		if e.fileOff != wantFileOffs[i] {
			t.Fatalf("hit[%d].fileOff: got %d want %d", i, e.fileOff, wantFileOffs[i])
		}
	}
	if hits[0].logPos >= hits[1].logPos {
		t.Fatalf("logPos out of arrival order across hits: %+v", hits)
	}
}

// TestLogIndex_ClearedByDeleteAppendLog verifies the DeleteAppendLog
// step-5 cleanup wipes the logIndex map entry alongside the rest of
// per-payload state.
func TestLogIndex_ClearedByDeleteAppendLog(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	if err := bc.AppendWrite(context.Background(), "file-del", []byte("hello"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	bc.logsMu.RLock()
	_, exists := bc.logIndices["file-del"]
	bc.logsMu.RUnlock()
	if !exists {
		t.Fatalf("logIndex not populated before delete")
	}

	if err := bc.DeleteAppendLog(context.Background(), "file-del"); err != nil {
		t.Fatalf("DeleteAppendLog: %v", err)
	}
	bc.logsMu.RLock()
	_, exists = bc.logIndices["file-del"]
	bc.logsMu.RUnlock()
	if exists {
		t.Fatalf("logIndex still present after DeleteAppendLog")
	}
}

// TestLogIndex_LogPosMatchesEofPosProgress cross-checks that the
// captured logPos in the LAST entry equals lf.eofPos minus the last
// record's framed size — i.e. the entry points at the frame boundary
// the writer wrote to, NOT at the post-advance cursor.
func TestLogIndex_LogPosMatchesEofPosProgress(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	payload := bytes.Repeat([]byte{0xEE}, 333)
	for _, off := range []uint64{0, 4096} {
		if err := bc.AppendWrite(context.Background(), "file-pos", payload, off); err != nil {
			t.Fatalf("AppendWrite: %v", err)
		}
	}
	bc.logsMu.RLock()
	lf := bc.logFDs["file-pos"]
	idx := bc.logIndices["file-pos"]
	mu := bc.logLocks["file-pos"]
	bc.logsMu.RUnlock()
	mu.Lock()
	defer mu.Unlock()
	last := idx.entries[len(idx.entries)-1]
	wantLogPos := lf.eofPos - uint64(recordFrameOverhead) - uint64(len(payload))
	if last.logPos != wantLogPos {
		t.Fatalf("last entry logPos: got %d want %d (eofPos=%d)", last.logPos, wantLogPos, lf.eofPos)
	}
}

// TestLogIndex_SeededByRecovery writes several records, closes the
// FSStore, and reopens via Recover. The reconstructed logIndex must
// carry one entry per on-disk record with logPos pointing at the frame
// boundary and fence pinned at the persisted rollup_offset (no
// pre-existing offset in this test => header boundary).
func TestLogIndex_SeededByRecovery(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	payload := bytes.Repeat([]byte{0x99}, 128)
	offsets := []uint64{0, 4096, 8192}
	for _, off := range offsets {
		if err := bc.AppendWrite(context.Background(), "file-rec-idx", payload, off); err != nil {
			t.Fatalf("AppendWrite: %v", err)
		}
	}

	bc2 := reopenFSStore(t, bc, rs)
	bc2.logsMu.RLock()
	idx := bc2.logIndices["file-rec-idx"]
	mu := bc2.logLocks["file-rec-idx"]
	bc2.logsMu.RUnlock()
	if idx == nil || mu == nil {
		t.Fatalf("logIndex / mu missing after recovery")
	}
	mu.Lock()
	defer mu.Unlock()

	if len(idx.entries) != len(offsets) {
		t.Fatalf("entry count after recovery: got %d want %d", len(idx.entries), len(offsets))
	}
	step := uint64(recordFrameOverhead) + uint64(len(payload))
	wantLogPos := uint64(logHeaderSize)
	for i, e := range idx.entries {
		if e.logPos != wantLogPos {
			t.Fatalf("entry[%d].logPos: got %d want %d", i, e.logPos, wantLogPos)
		}
		if e.fileOff != offsets[i] {
			t.Fatalf("entry[%d].fileOff: got %d want %d", i, e.fileOff, offsets[i])
		}
		wantLogPos += step
	}
	if idx.Fence() != logHeaderSize {
		t.Fatalf("fence after recovery (no prior rollup): got %d want %d", idx.Fence(), logHeaderSize)
	}
}
