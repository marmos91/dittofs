//go:build journal_conformance

package conformance

import (
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// The checks here fail by leaving an operator unable to tell which of two
// opposite situations a journal is in.

// written is one write a check made: where, and its version.
type written struct {
	share   int // the fixture share's index, or -1 for a write through a handle of the check's own
	id      journalv2.FileID
	extent  journalv2.Extent
	version journalv2.Version
}

// fillUntilRefused writes chunks of size bytes through the fixture's shares,
// round robin, each share to a file of its own starting at file first, until
// the journal refuses one, and returns every write made. The refusal must
// name the journal's own limit.
func fillUntilRefused(t *testing.T, f *journalv2test.Fixture, shares []int, first int, size int64) []written {
	t.Helper()

	var out []written
	offs := make([]int64, len(shares))
	for i := range 2 * journalv2test.FixtureMaxBytes / size {
		k := int(i) % len(shares)
		id := journalv2test.File(first + k)
		v, err := tryWrite(t, f.Share(shares[k]), id, offs[k], pattern(fmt.Sprint("fill", first, i), int(size)))
		stub(t, "WriteAt", err)
		if err != nil {
			wantNoSpace(t, err, journalv2.LimitJournal, "a write past the journal's maximum")
			return out
		}
		out = append(out, written{shares[k], id, journalv2.Extent{Off: offs[k], Len: size}, v})
		offs[k] += size
	}
	t.Fatalf("%d bytes written into a journal of %d were never refused", 2*journalv2test.FixtureMaxBytes, journalv2test.FixtureMaxBytes)
	panic("unreachable")
}

// atCapacity fails the test unless the journal's allocated occupancy is
// within two segments of its maximum, as it is once a write has been refused
// at the maximum.
func atCapacity(t *testing.T, s journalv2.Stats) {
	t.Helper()

	if s.UsedBytes <= s.MaxBytes-2*journalv2test.FixtureSegmentCap {
		t.Fatalf("UsedBytes = %d after a refusal at capacity, want within two segments of MaxBytes %d", s.UsedBytes, s.MaxBytes)
	}
}

// TestRFC1D_WedgeIsVisibleDirty checks that one Stats call shows a journal at
// capacity with nothing evictable, and which share holds the dirty bytes.
func TestRFC1D_WedgeIsVisibleDirty(t *testing.T) {
	f := journalv2test.New(t)
	write(t, f.Share(1), journalv2test.File(9), 0, pattern("b", 64<<10))
	fillUntilRefused(t, f, []int{0}, 0, 256<<10)

	s := f.J.Stats()
	atCapacity(t, s)
	if s.HeldBytes == 0 || s.DirtyBytes != s.HeldBytes {
		t.Fatalf("DirtyBytes = %d, HeldBytes = %d with nothing offloaded: want them equal and non-zero, so nothing reads as evictable", s.DirtyBytes, s.HeldBytes)
	}
	a, b := s.Shares[journalv2test.Tag(0)], s.Shares[journalv2test.Tag(1)]
	if b.DirtyBytes != 64<<10 || a.DirtyBytes != s.DirtyBytes-b.DirtyBytes {
		t.Fatalf("per-share DirtyBytes = %d and %d of %d: want share 1 to hold its 64 KiB and share 0 the rest", a.DirtyBytes, b.DirtyBytes, s.DirtyBytes)
	}
}

// TestRFC1D_WedgeIsVisibleUnreclaimed checks that one Stats call shows a
// journal at capacity whose storage is released content no repack has
// recovered, and names it per share.
//
// decision: one extent in three of each share stays held. The journal
// retires a segment whose every record is released on its own, which would
// return the very storage this check needs to see unreclaimed; a segment that
// still holds something is not retired, and only repack recovers the rest.
// The clock never moves, so no background pass runs on a timer either.
func TestRFC1D_WedgeIsVisibleUnreclaimed(t *testing.T) {
	f := journalv2test.New(t)
	ws := fillUntilRefused(t, f, []int{0, 1}, 0, 128<<10)

	// Each share marks its file offloaded in one call, so the marks take
	// little of the headroom the releases need.
	byFile := map[journalv2.FileID][]written{}
	for _, w := range ws {
		byFile[w.id] = append(byFile[w.id], w)
	}
	for id, fws := range byFile {
		var extents []journalv2.Extent
		for _, w := range fws {
			extents = append(extents, w.extent)
		}
		h := f.Share(fws[0].share)
		err := h.MarkOffloaded(t.Context(), id, extents, fws[0].version, fws[len(fws)-1].version)
		stub(t, "MarkOffloaded", err)
		if err != nil {
			t.Fatalf("mark a file offloaded: %v", err)
		}
	}

	released := map[int]int64{}
	var kept int64
	for i, w := range ws {
		if i%3 == 0 {
			kept += w.extent.Len
			continue
		}
		err := f.Share(w.share).Release(t.Context(), w.id, []journalv2.Extent{w.extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatalf("release an offloaded extent: %v", err)
		}
		released[w.share] += w.extent.Len
	}

	s := f.J.Stats()
	atCapacity(t, s)
	if s.HeldBytes != kept || s.DirtyBytes != 0 {
		t.Fatalf("HeldBytes = %d, DirtyBytes = %d, want the %d bytes kept and none dirty", s.HeldBytes, s.DirtyBytes, kept)
	}
	if s.UnreclaimedBytes == 0 || s.UnreclaimedBytes+s.HeldBytes > s.UsedBytes {
		t.Fatalf("UnreclaimedBytes = %d with %d held of %d used: want the released storage named, and no more than what is used", s.UnreclaimedBytes, s.HeldBytes, s.UsedBytes)
	}
	for k, n := range released {
		if got := s.Shares[journalv2test.Tag(k)].UnreclaimedBytes; got == 0 {
			t.Fatalf("share %d released %d bytes, and its UnreclaimedBytes is zero", k, n)
		}
	}
}

// loseDirty writes a record of file i, syncs it, damages it on disk and reads
// it, so the journal drops it as corrupt with its offloaded bit unset.
func loseDirty(t *testing.T, f *journalv2test.Fixture, i int) {
	t.Helper()

	h, id := f.Share(0), journalv2test.File(i)
	p := pattern(fmt.Sprint("lost", i), 4096)
	write(t, h, id, 0, p)
	err := h.Sync(t.Context(), id)
	stub(t, "Sync", err)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	corrupt(t, f, id, p)
	// The read finds the damage, and its error is the loss it reports.
	_, _, _, _, err = h.ReadAt(t.Context(), id, 0, make([]byte, len(p)))
	stub(t, "ReadAt", err)
}

// TestRFC1D_GenerationNeverRepeats checks that the loss generation continues
// across a close and reopen in one process, never restarting below a value it
// already exposed.
func TestRFC1D_GenerationNeverRepeats(t *testing.T) {
	f := journalv2test.New(t)

	start := f.J.Stats().LossGeneration
	for i := range 3 {
		loseDirty(t, f, i)
	}
	before := f.J.Stats().LossGeneration
	if before != start+3 {
		t.Fatalf("LossGeneration = %d after three losses of dirty content from %d, want %d", before, start, start+3)
	}

	f.Reopen()
	reopened := f.J.Stats().LossGeneration
	if reopened < before {
		t.Fatalf("LossGeneration = %d after a reopen in the same process, below the %d exposed before it", reopened, before)
	}

	// Measured from the reopened value, not from 3: whether an open that finds
	// the three loss records still on disk raises the generation again is the
	// journal's choice, as long as it never goes back.
	loseDirty(t, f, 3)
	if got := f.J.Stats().LossGeneration; got != reopened+1 {
		t.Fatalf("LossGeneration = %d after one more loss of dirty content, want %d", got, reopened+1)
	}
}

// TestRFC1D_StatsTakeNoWriteLock checks that Stats takes no lock a write
// needs.
//
// decision: the lock leg holds a write at its storage call, so it catches a
// Stats that waits on a lock the write keeps across that call, not one a
// write takes and drops before it, such as the index's: from the public
// surface, only a write stopped at storage can be held at all. The lock-order
// recorder of the data path is what pins the rest.
func TestRFC1D_StatsTakeNoWriteLock(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)

	held := f.Seam.Hold(t, journalv2test.OpWrite, "*.seg")
	wrote := make(chan error, 1)
	go func() {
		_, err := h.WriteAt(t.Context(), journalv2test.File(0), 0, pattern("held", 4096), 0)
		wrote <- err
	}()
	select {
	case <-held.Reached():
	case err := <-wrote:
		stub(t, "WriteAt", err)
		t.Fatalf("the write returned (%v) without reaching storage", err)
	}

	// decision: whether Stats returns while a write is stopped mid-way can
	// only be decided by waiting for it, so this is a real-time bound. It is
	// generous, because a Stats that waits on the held write never returns.
	statted := make(chan struct{})
	go func() {
		f.J.Stats()
		close(statted)
	}()
	select {
	case <-statted:
	case <-time.After(2 * time.Second):
		t.Fatal("Stats did not return while a write was held at storage")
	}
	held.Release()
	err := <-wrote
	stub(t, "WriteAt", err)
	if err != nil {
		t.Fatalf("the held write: %v", err)
	}
}

// TestRFC1D_StatsCostIsFlat checks that the cost of Stats does not grow with
// the extents the journal holds.
//
// decision: it counts allocations, the one cost a check can measure without
// timing it. Its ceiling: it catches a Stats whose allocations grow with the
// index, not a walk that allocates nothing, which only a counted hook in the
// journal would show. It skips under the race detector, which changes
// allocation counts, so it runs only in a leg without it.
func TestRFC1D_StatsCostIsFlat(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	f := journalv2test.New(t)
	h := f.Share(0)

	// AllocsPerRun counts the whole process's allocations, so each size is
	// measured once the journal's background work, such as making spares, has
	// gone quiet, and the smallest of several samples is kept.
	quiet := func() {
		deadline := time.Now().Add(2 * time.Second)
		for prev := -1; ; time.Sleep(10 * time.Millisecond) {
			n := f.Seam.Calls(journalv2test.OpWrite)
			if n == prev || time.Now().After(deadline) {
				return
			}
			prev = n
		}
	}
	allocsAt := func(extents int) float64 {
		for i := f.J.Stats().ExtentCount; i < int64(extents); i++ {
			// One-byte writes a byte apart never merge into one extent.
			write(t, h, journalv2test.File(1), 2*i, []byte{byte(i)})
		}
		quiet()
		least := testing.AllocsPerRun(10, func() { f.J.Stats() })
		for range 4 {
			least = min(least, testing.AllocsPerRun(10, func() { f.J.Stats() }))
		}
		return least
	}
	small, large := allocsAt(1_000), allocsAt(100_000)
	if large > small {
		t.Fatalf("Stats allocates %.0f times at 10^5 extents and %.0f at 10^3: its cost grows with the index", large, small)
	}
}

// TestRFC1D_BacklogAge checks that OldestDirty is the write time of the
// oldest extent still dirty, and moves on when that extent is offloaded.
func TestRFC1D_BacklogAge(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)

	t0 := f.Now()
	v0 := write(t, h, journalv2test.File(0), 0, pattern("old", 4096))
	f.Advance(time.Minute)
	t1 := f.Now()
	write(t, h, journalv2test.File(1), 0, pattern("new", 4096))

	if got := f.J.Stats().OldestDirty; !got.Equal(t0) {
		t.Fatalf("OldestDirty = %v with the first write still dirty, want its write time %v", got, t0)
	}
	mark(t, h, journalv2test.File(0), journalv2.Extent{Off: 0, Len: 4096}, v0)
	if got := f.J.Stats().OldestDirty; !got.Equal(t1) {
		t.Fatalf("OldestDirty = %v once the first write was offloaded, want the second's write time %v", got, t1)
	}
}

// TestRFC1D_CleanBytesCountAsOccupancy checks that a journal full of clean,
// offloaded content reads as full, while nothing is dirty or unreclaimed: a
// trigger on allocated occupancy fires, and one on dirty plus unreclaimed
// bytes would not.
func TestRFC1D_CleanBytesCountAsOccupancy(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)

	v, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatalf("draw a version for the filled content: %v", err)
	}
	const size = 256 << 10
	refused := false
	for i := range 2 * journalv2test.FixtureMaxBytes / size {
		id, buf := journalv2test.File(i), make([]byte, size)
		_, _, _, asOf, err := h.ReadAt(t.Context(), id, 0, buf)
		stub(t, "ReadAt", err)
		if err != nil {
			t.Fatalf("read an absent file: %v", err)
		}
		err = journalv2test.RetrySpare(func() error {
			return h.Fill(t.Context(), id, 0, pattern(fmt.Sprint("clean", i), size), asOf, v)
		})
		stub(t, "Fill", err)
		if err != nil {
			wantNoSpace(t, err, journalv2.LimitJournal, "a fill past the journal's maximum")
			refused = true
			break
		}
	}
	if !refused {
		t.Fatalf("fills of %d bytes into a journal of %d were never refused", 2*journalv2test.FixtureMaxBytes, journalv2test.FixtureMaxBytes)
	}

	s := f.J.Stats()
	atCapacity(t, s)
	if s.HeldBytes == 0 {
		t.Fatal("HeldBytes is zero with the journal full of filled content")
	}
	if s.DirtyBytes != 0 || s.UnreclaimedBytes != 0 {
		t.Fatalf("DirtyBytes = %d, UnreclaimedBytes = %d with only filled content held, want both zero", s.DirtyBytes, s.UnreclaimedBytes)
	}
}

// TestRFC1D_MetricsAreReachable checks that every condition a metric names
// moves that metric, read from Stats.
func TestRFC1D_MetricsAreReachable(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)

	// A refused reservation. It exceeds the spares as well as the journal's
	// maximum, and a refusal names the spare pool only when no other limit
	// would refuse.
	_, err := h.Reserve(2*journalv2test.FixtureMaxBytes, 1)
	stub(t, "Reserve", err)
	wantNoSpace(t, err, journalv2.LimitJournal, "a reservation of twice the journal's maximum")
	if got := f.J.Stats().Counters.ReservationRefused; got == 0 {
		t.Fatal("ReservationRefused is zero after a refused reservation")
	}

	// A failed sync, whether the journal then re-appends its window or fails
	// it.
	f.FailNext(journalv2test.OpSync, "*.seg", 1)
	write(t, h, journalv2test.File(0), 0, pattern("sync", 4096))
	stub(t, "Sync", h.Sync(t.Context(), journalv2test.File(0)))
	f.Seam.SetFault(nil)
	if got := f.J.Stats().Counters.SyncFailures; got == 0 {
		t.Fatal("SyncFailures is zero after a sync failed at storage")
	}

	// A corrupt dirty extent.
	loseDirty(t, f, 1)
	if got := f.J.Stats().Counters.CorruptExtentsDataLoss; got == 0 {
		t.Fatal("CorruptExtentsDataLoss is zero after a dirty extent was read corrupt")
	}

	// A torn tail: the record's segment cut to one byte of its last write by
	// the crash. Its syncs are dropped first, so the record stays unsynced
	// even for a journal that syncs early, and the tear is kept to that
	// segment, so a spare written in the background cannot take it. It
	// assumes the last write starts a record, as a journal that appends a
	// record in one call does; one that writes a header and its payload apart
	// would keep a payload byte behind an empty slot, which a scan can end at
	// cleanly.
	f.Drop(journalv2test.OpSync, "*.seg")
	write(t, h, journalv2test.File(2), 0, pattern("torn", 4096))
	torn := heldEntry(t, f, journalv2test.File(2)).Location.Segment
	f.Seam.SetFault(nil) // the record stays unsynced; recovery's syncs must work
	f.Crash(journalv2test.TornIn(segmentName(torn), 1))
	if got := f.J.Stats().Counters.TornTails; got == 0 {
		t.Fatal("TornTails is zero after an open that found a torn tail")
	}

	// A segment from another journal, copied in under an id this journal has
	// not used. Its header names the other journal, so it is foreign whatever
	// id its header gives and whatever its records, never a spare. The
	// margin keeps the id clear of spares this journal may still be making,
	// which take the next ids up.
	const margin = 8
	other := journalv2test.NewUnwatched(t)
	write(t, other.Share(0), journalv2test.File(0), 0, pattern("foreign", 4096))
	err = other.Share(0).Sync(t.Context(), journalv2test.File(0))
	stub(t, "Sync", err)
	if err != nil {
		t.Fatal(err)
	}
	data, err := other.Seam.Durable(segmentName(heldEntry(t, other, journalv2test.File(0)).Location.Segment))
	if err != nil {
		t.Fatal(err)
	}
	plant(t, f, segmentName(newestID(t, f)+margin), data)
	f.Reopen()
	if got := f.J.Stats().RejectedSegments; got == 0 {
		t.Fatal("RejectedSegments is zero after an open beside a segment of another journal")
	}
}

// TestRFC1D_LossEvents checks that every extent the journal stops holding on
// its own appears as a loss event naming its file, extent, reason and bit,
// and that a poller that missed events can tell how many.
func TestRFC1D_LossEvents(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)

	// last returns the newest event's sequence number, and whether there is
	// one: nothing pins where the numbering starts.
	last := func() (uint64, bool) {
		ls := f.J.Stats().Losses
		if len(ls) == 0 {
			return 0, false
		}
		return ls[len(ls)-1].SeqNum, true
	}
	start, hadOne := last()
	e := journalv2.Extent{Off: 0, Len: 4096}

	// A corrupt dirty extent.
	corruptID := journalv2test.File(0)
	loseDirty(t, f, 0)

	// A stale extent: held content older than the oldest version of the ref
	// said to cover it.
	staleID := journalv2test.File(1)
	write(t, h, staleID, 0, pattern("stale", 4096))
	newer, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatal(err)
	}
	mark(t, h, staleID, e, newer)

	want := map[journalv2.FileID]journalv2.LossReason{corruptID: journalv2.LossCorrupt, staleID: journalv2.LossStale}
	for _, l := range f.J.Stats().Losses {
		r, ok := want[l.File]
		if !ok || (hadOne && l.SeqNum <= start) {
			continue
		}
		if l.Reason != r || l.Share != journalv2test.Tag(0) || l.Extent != e {
			t.Fatalf("loss event %+v, want reason %v for share %d over %+v", l, r, journalv2test.Tag(0), e)
		}
		// Both extents were dirty, so both events report the bit unset: an
		// event reports the bit as it was, even for a stale drop, which
		// loses nothing the remote tier lacks.
		if l.Offloaded {
			t.Fatalf("loss event %+v for a dirty extent reports its offloaded bit set", l)
		}
		delete(want, l.File)
	}
	if len(want) != 0 {
		t.Fatalf("no loss event for %d of the two dropped extents: %v", len(want), want)
	}

	// Far more events than the ring keeps: the sequence numbers still count
	// every one, so a poller that saw none of them knows how many it missed.
	seen, _ := last()
	const n = 5_000
	for i := range n {
		id := journalv2test.File(100 + i)
		write(t, h, id, 0, []byte{1})
		v, err := f.J.Draw()
		stub(t, "Draw", err)
		if err != nil {
			t.Fatal(err)
		}
		mark(t, h, id, journalv2.Extent{Off: 0, Len: 1}, v)
	}
	if now, _ := last(); now-seen != n {
		t.Fatalf("the loss sequence moved by %d over %d stale drops, want %d", now-seen, n, n)
	}
}

// TestRFC1D_SegmentHeldBytes checks that each segment's held-bytes counter
// matches a walk of the index at every step of writes, releases and repack.
func TestRFC1D_SegmentHeldBytes(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)

	check := func(step string) {
		t.Helper()
		walked := map[journalv2.SegmentID]int64{}
		for _, entries := range f.J.Inspect().All() {
			for _, e := range entries {
				if e.Kind == journalv2.EntryHeld {
					walked[e.Location.Segment] += e.Extent.Len
				}
			}
		}
		for _, s := range f.J.Inspect().Segments() {
			if s.HeldBytes != walked[s.ID] {
				t.Fatalf("after %s: segment %d counts %d held bytes, the index walk %d", step, s.ID, s.HeldBytes, walked[s.ID])
			}
			delete(walked, s.ID)
		}
		if len(walked) != 0 {
			t.Fatalf("after %s: the index places held bytes in segments Segments does not list: %v", step, walked)
		}
	}

	var ws []written
	for i := range 4 * journalv2test.FixtureSegmentCap / (64 << 10) {
		id, off := journalv2test.File(i%2), int64(i/2)*64<<10
		v := write(t, h, id, off, pattern(fmt.Sprint("seg", i), 64<<10))
		ws = append(ws, written{0, id, journalv2.Extent{Off: off, Len: 64 << 10}, v})
		check(fmt.Sprintf("write %d", i))
	}
	for i, w := range ws {
		if i%3 == 0 {
			continue
		}
		mark(t, h, w.id, w.extent, w.version)
		err := h.Release(t.Context(), w.id, []journalv2.Extent{w.extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		check(fmt.Sprintf("release %d", i))
	}
	moved := f.J.Stats().Counters.RepackedBytes
	freed, err := f.J.Repack(t.Context(), 2*journalv2test.FixtureSegmentCap, journalv2.RepackScope{})
	stub(t, "Repack", err)
	if err != nil {
		t.Fatalf("repack: %v", err)
	}
	if freed == 0 && f.J.Stats().Counters.RepackedBytes == moved {
		t.Fatal("repack over segments two thirds released moved nothing and freed nothing")
	}
	check("repack")
}

// TestRFC1D_VerificationReports checks that verification names an extent the
// index claims and its record no longer supports, and changes nothing.
func TestRFC1D_VerificationReports(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	id, p := journalv2test.File(0), pattern("verify", 4096)
	write(t, h, id, 0, p)
	err := h.Sync(t.Context(), id)
	stub(t, "Sync", err)
	if err != nil {
		t.Fatal(err)
	}
	// A second file, on the other share, that stays whole.
	whole := journalv2test.File(1)
	write(t, f.Share(1), whole, 0, pattern("whole", 4096))
	err = f.Share(1).Sync(t.Context(), whole)
	stub(t, "Sync", err)
	if err != nil {
		t.Fatal(err)
	}
	corrupt(t, f, id, p)

	before, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatal(err)
	}
	// Writes into the segments only: a spare being made in the background
	// writes to files of its own while Verify runs.
	segWrites := func() (n int) {
		for _, s := range f.J.Inspect().Segments() {
			n += f.Seam.CallsFor(journalv2test.OpWrite, segmentName(s.ID))
		}
		return n
	}
	writes, losses := segWrites(), f.J.Stats().Counters.CorruptExtentsDataLoss

	report, err := f.J.Inspect().Verify(t.Context())
	stub(t, "Inspect.Verify", err)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := report.Unsupported[id]; len(got) != 1 || got[0].Extent != (journalv2.Extent{Off: 0, Len: int64(len(p))}) {
		t.Fatalf("Verify reported %+v unsupported for the damaged file, want its one extent", got)
	}
	if got := report.Unsupported[whole]; len(got) != 0 {
		t.Fatalf("Verify reported %+v unsupported for a file that is whole", got)
	}

	after, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("Verify changed the index: %+v, then %+v", before, after)
	}
	if got := segWrites(); got != writes {
		t.Fatalf("Verify wrote to the segments %d times", got-writes)
	}
	if got := f.J.Stats().Counters.CorruptExtentsDataLoss; got != losses {
		t.Fatalf("Verify dropped %d extents as corrupt; it reports and never repairs", got-losses)
	}
}
