package fs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReadPayloadAt_IndexSeek_LastWriteWins exercises the index-seek read
// path directly: multiple records covering the SAME file offset must resolve
// to the LATEST record's bytes (last-write-wins), exactly as the previous
// full-log replay did. The records are NOT rolled up, so they live only in
// the append log and the read is served entirely from the logIndex seek.
func TestReadPayloadAt_IndexSeek_LastWriteWins(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	const pid = "lww"

	// Three writes at offset 0, same length: A then B then C. Last wins.
	for _, b := range []byte{'A', 'B', 'C'} {
		if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{b}, 1024), 0); err != nil {
			t.Fatalf("AppendWrite %c: %v", b, err)
		}
	}

	got := make([]byte, 1024)
	n, err := bc.ReadPayloadAt(ctx, pid, got, 0)
	if err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if n != len(got) {
		t.Fatalf("short read: got %d want %d", n, len(got))
	}
	if want := bytes.Repeat([]byte{'C'}, 1024); !bytes.Equal(got, want) {
		t.Fatalf("last-write-wins violated: got[:4]=%q want C*", got[:4])
	}
}

// TestReadPayloadAt_IndexSeek_OutOfOrderArrival mirrors the parallel-write
// case the index was designed for: records arrive in an order that is NOT
// file-offset order, with a later overwrite partially superseding an earlier
// record. The read must reflect arrival-order (logPos) last-write-wins on the
// overlapping bytes and the earlier record's bytes elsewhere.
func TestReadPayloadAt_IndexSeek_OutOfOrderArrival(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	const pid = "ooo"

	// Arrival 1: offset 4096, 'Z' x 4096  (higher file offset arrives first).
	if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{'Z'}, 4096), 4096); err != nil {
		t.Fatalf("AppendWrite hi: %v", err)
	}
	// Arrival 2: offset 0, 'X' x 4096  (lower file offset arrives second).
	if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{'X'}, 4096), 0); err != nil {
		t.Fatalf("AppendWrite lo: %v", err)
	}
	// Arrival 3: offset 2048, 'Y' x 4096 — straddles both, arrives LAST so it
	// wins on [2048, 6144).
	if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{'Y'}, 4096), 2048); err != nil {
		t.Fatalf("AppendWrite straddle: %v", err)
	}

	// Expected logical content over [0, 8192):
	//   [0, 2048)    -> 'X' (arrival 2)
	//   [2048, 6144) -> 'Y' (arrival 3, last writer)
	//   [6144, 8192) -> 'Z' (arrival 1)
	want := make([]byte, 8192)
	for i := 0; i < 2048; i++ {
		want[i] = 'X'
	}
	for i := 2048; i < 6144; i++ {
		want[i] = 'Y'
	}
	for i := 6144; i < 8192; i++ {
		want[i] = 'Z'
	}

	got := make([]byte, 8192)
	if _, err := bc.ReadPayloadAt(ctx, pid, got, 0); err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if !bytes.Equal(got, want) {
		// Report the first divergence for diagnosis.
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("byte %d: got %q want %q", i, got[i], want[i])
			}
		}
	}
}

// TestReadPayloadAt_IndexSeek_PartialWindow asserts a sub-window read (offset
// inside a record, length shorter than the record) lands the correct bytes
// via the seek path — the destIdx/srcIdx clamping must be exact.
func TestReadPayloadAt_IndexSeek_PartialWindow(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	const pid = "partial"

	// One 8 KiB record of incrementing bytes at offset 0.
	rec := make([]byte, 8192)
	for i := range rec {
		rec[i] = byte(i)
	}
	if err := bc.AppendWrite(ctx, pid, rec, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Read [3000, 5000).
	got := make([]byte, 2000)
	if _, err := bc.ReadPayloadAt(ctx, pid, got, 3000); err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if !bytes.Equal(got, rec[3000:5000]) {
		t.Fatalf("partial window mismatch: got[:4]=%v want %v", got[:4], rec[3000:5000][:4])
	}
}

// TestReadPayloadAt_ConsumedThenOverwritten is the regression for the mixed
// read/write/overwrite workload that broke the previous attempt. A record is
// written and ROLLED UP (consumed → dropped from the logIndex, but its frame
// remains physically in the on-disk log). A later in-place overwrite of the
// SAME region lands ONLY in the append log (unconsumed). A read of the region
// must return:
//   - the overwrite bytes where the unconsumed log record covers them
//     (served from the index seek), and
//   - the rolled-up CAS bytes elsewhere (served from the manifest),
//
// never decoding the stale physical frame of the consumed record (the
// previous attempt's "payloadLen ... exceeds ... cap" bug came from seeking a
// wrong/stale logPos into such a frame).
func TestReadPayloadAt_ConsumedThenOverwritten(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	fbs := newMemFileBlockStore()
	persister := func(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, _ blockstore.ObjectID) error {
		return fbs.persist(ctx, payloadID, blocks)
	}

	bc := newFSStoreForTestWithFBS(t, fbs, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   3_600_000,
		RollupStore:       rs,
		ObjectIDPersister: persister,
	})
	ctx := context.Background()
	const pid = "consumed-overwrite"

	// Write 16 KiB of 'A' at offset 0 and roll it up into CAS.
	base := bytes.Repeat([]byte{'A'}, 16*1024)
	if err := bc.AppendWrite(ctx, pid, base, 0); err != nil {
		t.Fatalf("AppendWrite base: %v", err)
	}
	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	// The base record is now consumed: the index must have been trimmed at
	// the fence so its entry is gone, even though the physical frame remains
	// on disk.
	bc.logsMu.RLock()
	idx := bc.logIndices[pid]
	bc.logsMu.RUnlock()
	if idx == nil {
		t.Fatal("logIndex missing for payload")
	}
	if got := idx.Len(); got != 0 {
		t.Fatalf("expected 0 unconsumed index entries after drain, got %d", got)
	}

	// In-place overwrite of [4096, 8192) with 'B' — lands ONLY in the log.
	over := bytes.Repeat([]byte{'B'}, 4096)
	if err := bc.AppendWrite(ctx, pid, over, 4096); err != nil {
		t.Fatalf("AppendWrite overwrite: %v", err)
	}

	// Read [0, 16384): 'A' from CAS everywhere except [4096,8192) which is
	// the unconsumed 'B' overwrite served via the index seek.
	want := bytes.Repeat([]byte{'A'}, 16*1024)
	for i := 4096; i < 8192; i++ {
		want[i] = 'B'
	}
	got := make([]byte, 16*1024)
	n, err := bc.ReadPayloadAt(ctx, pid, got, 0)
	if err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if n != len(got) {
		t.Fatalf("short read: got %d want %d", n, len(got))
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("byte %d: got %q want %q", i, got[i], want[i])
			}
		}
	}
}

// TestReadPayloadAt_OverCapRecordSkipped is the exact regression for the
// previous attempt's failure: a single AppendWrite larger than
// maxRecordPayload (e.g. the bench's 32 MiB mixed-rw seed) lands as ONE log
// frame at logPos=64. readRecordAt cannot decode a frame above the cap, so
// the seek path must SKIP it — matching the pre-index full-log replay, which
// likewise skipped over-cap frames and let coverage fall through to a miss —
// rather than returning the hard "payloadLen ... exceeds ... cap" error that
// broke mixed-rw. A read of the unrolled large record therefore reports a
// local miss (ErrFileBlockNotFound), never wrong bytes and never an error.
func TestReadPayloadAt_OverCapRecordSkipped(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	const pid = "overcap"

	big := make([]byte, maxRecordPayload+4096) // exceeds the read-path cap
	if err := bc.AppendWrite(ctx, pid, big, 0); err != nil {
		t.Fatalf("AppendWrite big: %v", err)
	}

	// Sanity: the index has the over-cap entry (the WRITE path accepts it).
	bc.logsMu.RLock()
	idx := bc.logIndices[pid]
	bc.logsMu.RUnlock()
	if idx == nil || idx.Len() != 1 {
		t.Fatalf("expected 1 index entry for the over-cap write")
	}

	// Read must not error and must not partially fill; it reports a local
	// miss so the engine falls back (exactly as the old full-log replay did).
	got := make([]byte, 4096)
	_, err := bc.ReadPayloadAt(ctx, pid, got, 0)
	if !errors.Is(err, blockstore.ErrFileBlockNotFound) {
		t.Fatalf("ReadPayloadAt over-cap: err = %v, want ErrFileBlockNotFound (skip, not hard error)", err)
	}
}

// TestReadPayloadAt_IndexSeek_NoLogPosScaling proves the per-read cost no
// longer scales with the on-disk log size. We write many DISTINCT records at
// increasing offsets, then read only the FIRST record. The index-seek path
// must touch exactly one frame regardless of how many records precede or
// follow it on disk — verified by asserting the index returns a single entry
// for the queried window even though the log holds many.
func TestReadPayloadAt_IndexSeek_NoLogPosScaling(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	const pid = "noscale"
	const recLen = 4096
	const nRecs = 256

	for i := 0; i < nRecs; i++ {
		if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{byte(i)}, recLen), uint64(i)*recLen); err != nil {
			t.Fatalf("AppendWrite %d: %v", i, err)
		}
	}

	bc.logsMu.RLock()
	idx := bc.logIndices[pid]
	bc.logsMu.RUnlock()
	if idx == nil {
		t.Fatal("logIndex missing")
	}
	if total := idx.Len(); total != nRecs {
		t.Fatalf("index entry count: got %d want %d", total, nRecs)
	}
	// A read of the first record's window must map to exactly ONE index
	// entry — the lookup does not depend on the total record count.
	hits := idx.EntriesForInterval(0, recLen, nil)
	if len(hits) != 1 {
		t.Fatalf("EntriesForInterval(first record) returned %d entries, want 1 (read cost must not scale with log size)", len(hits))
	}

	got := make([]byte, recLen)
	if _, err := bc.ReadPayloadAt(ctx, pid, got, 0); err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if want := bytes.Repeat([]byte{0}, recLen); !bytes.Equal(got, want) {
		t.Fatalf("first record read mismatch: got[:4]=%v", got[:4])
	}
}
