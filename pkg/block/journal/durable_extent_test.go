package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

// TestDurableExtent_PredictsWhatSurvivesDeviceLoss is the proof behind the size
// invariant: whatever DurableExtent reports before an unclean shutdown is
// exactly what the store still has after one. A caller that publishes a file
// size at or below that number can never end up describing bytes the device
// never took — bytes whose absence would read back as a hole full of zeros
// instead of an error.
//
// The crash model is device loss, not process death: reopening from the same
// tmpfiles would otherwise be served the un-fsynced tail straight out of the
// page cache and prove nothing. So the segment is physically truncated back to
// the length it had at the last fsync, which is precisely what a device that
// dropped every unflushed write would have kept.
func TestDurableExtent_PredictsWhatSurvivesDeviceLoss(t *testing.T) {
	const rec = 4096
	dir := t.TempDir()
	cfg := Config{ShardCount: 1}
	s, err := Open(dir, cfg, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	id := FileID("acked.bin")

	write := func(recIdx int, fill byte) {
		t.Helper()
		if err := s.WriteAt(ctx, id, int64(recIdx)*rec, bytes.Repeat([]byte{fill}, rec)); err != nil {
			t.Fatalf("WriteAt rec %d: %v", recIdx, err)
		}
	}

	// Two records the client was told are on stable storage.
	write(0, 'a')
	write(1, 'b')
	if err := s.Commit(ctx, id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	durable, ok := s.DurableExtent(ctx, id)
	if !ok || durable != 2*rec {
		t.Fatalf("DurableExtent after Commit = (%d, %v), want (%d, true)", durable, ok, 2*rec)
	}
	durableTail := s.shards[0].active.tail.Load()
	segID := s.shards[0].active.id

	// Two more the client acknowledged but nothing ever committed. They are in
	// the page cache: readable now, gone after device loss.
	write(2, 'c')
	write(3, 'd')

	if size, ok := s.FileSize(ctx, id); !ok || size != 4*rec {
		t.Fatalf("FileSize = (%d, %v), want (%d, true) — the buffered records must be visible", size, ok, 4*rec)
	}
	if got, ok := s.DurableExtent(ctx, id); !ok || got != 2*rec {
		t.Fatalf("DurableExtent = (%d, %v) after uncommitted writes, want (%d, true) — "+
			"buffered bytes must never count as durable", got, ok, 2*rec)
	}

	// Device loss: everything appended after the last fsync never reached the
	// platter. Close first so no fd is left writing behind the truncation.
	segPath := s.segPath(segID)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Truncate(segPath, durableTail); err != nil {
		t.Fatalf("truncate segment to the last fsync: %v", err)
	}

	r, err := Open(dir, cfg, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// The prediction: what survived is exactly what DurableExtent promised.
	size, ok := r.FileSize(ctx, id)
	if !ok || size != durable {
		t.Fatalf("post-crash FileSize = (%d, %v), want (%d, true) — DurableExtent over-promised", size, ok, durable)
	}
	if got, ok := r.DurableExtent(ctx, id); !ok || got != durable {
		t.Fatalf("post-crash DurableExtent = (%d, %v), want (%d, true) — recovered data must count as durable", got, ok, durable)
	}
	got := make([]byte, 2*rec)
	if _, _, err := r.ReadAt(ctx, id, 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	want := append(bytes.Repeat([]byte{'a'}, rec), bytes.Repeat([]byte{'b'}, rec)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("committed bytes did not survive the crash intact")
	}
}

// TestDurableExtent_FrozenAfterFsyncFailure covers the write-back error case the
// group commit already treats as sticky. Once the kernel reports an fsync
// failure it drops those pages, and the very next fsync can return success
// without them ever having reached the device — so a later success is not
// evidence that the earlier bytes landed. The watermark must freeze where it
// last stood rather than sweep the failed range up with the successful one.
func TestDurableExtent_FrozenAfterFsyncFailure(t *testing.T) {
	const rec = 4096
	s := testStore(t, Config{ShardCount: 1})
	ctx := context.Background()
	id := FileID("acked.bin")

	failNext := true
	sh := s.shardFor(id)
	sh.segSync = func(seg *segmentMeta) error {
		if failNext {
			failNext = false
			return errors.New("simulated write-back failure")
		}
		return seg.fd.Sync()
	}

	if err := s.WriteAt(ctx, id, 0, bytes.Repeat([]byte{'a'}, rec)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, id); err == nil {
		t.Fatal("Commit succeeded despite a failing fsync")
	}
	if got, _ := s.DurableExtent(ctx, id); got != 0 {
		t.Fatalf("DurableExtent = %d after a failed fsync, want 0", got)
	}

	// A later fsync reports success. It says nothing about the dropped pages.
	if err := s.WriteAt(ctx, id, rec, bytes.Repeat([]byte{'b'}, rec)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, id); err != nil {
		t.Fatalf("Commit after the failure: %v", err)
	}
	if got, _ := s.DurableExtent(ctx, id); got != 0 {
		t.Fatalf("DurableExtent = %d after a success following a failed fsync, want 0 — "+
			"a later success must not vouch for pages the kernel already dropped", got)
	}
}

// TestDurableExtent_StopsAtTheFirstNonDurableRange covers writes that arrive out
// of offset order, which is ordinary for concurrent dispatch and for any client
// doing random writes. Durability is keyed on append order while the index is
// keyed on offset, so a durable interval can sit ABOVE a non-durable one. The
// extent must stop below the range that could vanish: reporting the higher one
// would place the lost range inside the committed size, which is exactly the
// hole-of-zeros this whole mechanism exists to prevent.
func TestDurableExtent_StopsAtTheFirstNonDurableRange(t *testing.T) {
	const rec = 4096
	s := testStore(t, Config{ShardCount: 1})
	ctx := context.Background()
	id := FileID("acked.bin")

	// A high-offset write that is committed, so its bytes are on stable storage.
	if err := s.WriteAt(ctx, id, 100*rec, bytes.Repeat([]byte{'a'}, rec)); err != nil {
		t.Fatalf("WriteAt high: %v", err)
	}
	if err := s.Commit(ctx, id); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// A later write to an EARLIER offset that nothing commits. It sits first in
	// the offset-ordered index but carries the higher Version.
	if err := s.WriteAt(ctx, id, 0, bytes.Repeat([]byte{'b'}, rec)); err != nil {
		t.Fatalf("WriteAt low: %v", err)
	}

	got, ok := s.DurableExtent(ctx, id)
	if !ok {
		t.Fatal("DurableExtent reported unknown for a file it has intervals for")
	}
	if got != 0 {
		t.Fatalf("DurableExtent = %d, want 0 — the durable high-offset range must not be "+
			"reported over a non-durable range beneath it, or its loss reads as zeros "+
			"inside the committed size", got)
	}
}

// TestDurableExtent_SpansGenuineHoles pins the other side of that rule. A gap
// nobody ever wrote is a real sparse hole: it reads as zeros correctly and must
// not hold the extent back, otherwise a client that writes only at a high offset
// (thin-provisioned and random-write workloads both do) would never get a size.
func TestDurableExtent_SpansGenuineHoles(t *testing.T) {
	const rec = 4096
	s := testStore(t, Config{ShardCount: 1})
	ctx := context.Background()
	id := FileID("sparse.bin")

	if err := s.WriteAt(ctx, id, 100*rec, bytes.Repeat([]byte{'a'}, rec)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, id); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, ok := s.DurableExtent(ctx, id)
	if !ok || got != 101*rec {
		t.Fatalf("DurableExtent = (%d, %v), want (%d, true) — a never-written gap is a "+
			"genuine hole and must not hold the extent back", got, ok, 101*rec)
	}
}

// TestDurableExtent_UnknownFileReportsNotOk keeps the "unknown" answer distinct
// from "nothing is durable": a caller must be able to tell a file the local tier
// has never heard of from one whose bytes are all still buffered, because the
// two demand opposite handling of a size commit.
func TestDurableExtent_UnknownFileReportsNotOk(t *testing.T) {
	s := testStore(t, Config{})
	if got, ok := s.DurableExtent(context.Background(), "never-written"); ok {
		t.Fatalf("DurableExtent for an unknown file = (%d, true), want ok=false", got)
	}
}
