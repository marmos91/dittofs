//go:build journal_conformance

package conformance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/sim"
)

// The checks here fail by leaving the journal in a state it cannot leave, or
// by letting it consume without bound.

// The tags of the shares the checks here attach with a limit of their own,
// clear of the fixture's shares.
var (
	limitedA = journalv2test.Tag(4)
)

// backgroundWait bounds, in real time, how long a check waits for the
// journal's own goroutines: an idle seal after the clock moved, a segment
// retired once its releases are durable, or a call that must return at once.
//
// decision: the journal does that work on goroutines of its own, which no
// hold can wait on without also stopping them, so only a real-time wait sees
// it finish; a correct journal spends a fraction of it. Overturned once the
// fixture can wait on a point the journal reports to the seam.
const backgroundWait = 2 * time.Second

// eventually returns once cond holds, and fails the test with what if it
// does not within backgroundWait.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(backgroundWait); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s within %v", what, backgroundWait)
		}
	}
}

// writeRecordBytes is the bytes a write record of n payload bytes takes in a
// segment: the 96-byte header, the 8-byte modification time and the payload,
// padded to a multiple of 8.
func writeRecordBytes(n int) int64 { return int64(96+8+n+7) &^ 7 }

// spareSlack returns UsedBytes less the accounted size of every segment
// Inspect lists, and whether that rest is a whole number of segment caps, as
// the spares, each allocated at its cap, make it.
//
// decision: a check of UsedBytes compares it through this, not exactly.
// Spares count in UsedBytes, are made and recycled off the write path, and
// whether Inspect lists them is not pinned, so an exact UsedBytes races them
// on a correct journal. The ceiling: a change of UsedBytes by exactly one cap
// reads as a spare. Overturned once Stats reports the spares, when the
// checks subtract them and compare exactly.
func spareSlack(f *journalv2test.Fixture) (int64, bool) {
	used := f.J.Stats().UsedBytes
	var sum int64
	for _, s := range f.J.Inspect().Segments() {
		sum += s.Allocated
	}
	extra := used - sum
	return extra, extra >= 0 && extra%journalv2test.FixtureSegmentCap == 0
}

// wantAccounted fails the test unless UsedBytes settles at the listed
// segments' accounted sizes plus whole spares.
func wantAccounted(t *testing.T, f *journalv2test.Fixture, what string) {
	t.Helper()
	eventually(t, what+": UsedBytes is not the listed segments' sizes plus whole spares", func() bool {
		_, ok := spareSlack(f)
		return ok
	})
}

// sameBut reports whether two UsedBytes differ by want, give or take whole
// spares.
func sameBut(before, after, want int64) bool {
	return (before-after-want)%journalv2test.FixtureSegmentCap == 0
}

// limitedShare attaches tag with limit through the journal itself, since the
// fixture's shares carry none, at the fixture's floor, and closes the handle
// when the test ends.
func limitedShare(t *testing.T, f *journalv2test.Fixture, tag journalv2.ShareTag, limit int64) *journalv2.Handle {
	t.Helper()

	h, err := f.J.Share(tag, limit, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach share %d with a limit of %d: %v", tag, limit, err)
	}
	t.Cleanup(func() { _ = h.Close() }) // ErrClosed once the test closed it
	return h
}

// refusal fails the test unless err, returned by call, is a NoSpaceError
// naming limit and counting a positive shortfall in one of units, or in any
// unit when none is given, and returns it.
func refusal(t *testing.T, err error, call string, limit journalv2.Limit, units ...journalv2.Unit) *journalv2.NoSpaceError {
	t.Helper()

	stub(t, call, err)
	wantNoSpace(t, err, limit, call)
	var ns *journalv2.NoSpaceError
	errors.As(err, &ns)
	if (len(units) > 0 && !slices.Contains(units, ns.Unit)) || ns.Missing <= 0 {
		t.Fatalf("%s was refused with %d %v missing, want a positive count in %v", call, ns.Missing, ns.Unit, units)
	}
	return ns
}

// wantENOSPC fails the test unless err is the filesystem's ENOSPC and not
// one of the journal's own refusals.
func wantENOSPC(t *testing.T, err error, what string) {
	t.Helper()

	stub(t, what, err)
	if !errors.Is(err, syscall.ENOSPC) || errors.Is(err, journalv2.ErrNoSpace) {
		t.Fatalf("%s returned %v, want the filesystem's ENOSPC and no NoSpaceError", what, err)
	}
}

// fullDevice makes every new file and every allocation in the fixture's
// directory fail with ENOSPC, as a full filesystem would, replacing any fault
// set before.
func fullDevice(t *testing.T, f *journalv2test.Fixture) {
	t.Helper()

	full := sim.FaultAction{Kind: sim.Fail, Err: syscall.ENOSPC}
	fault, err := sim.NewSchedule(1, nil,
		sim.Rule{Op: journalv2test.OpCreate, Key: "*", Action: full},
		sim.Rule{Op: journalv2test.OpAllocate, Key: "*", Action: full},
	)
	if err != nil {
		t.Fatalf("a full device: %v", err)
	}
	f.Seam.SetFault(fault)
}

// fillShare writes chunks of size bytes through h, round robin over ids, until
// the share's limit refuses one, and returns every write made and the
// refusal. share is recorded in each write so a check can tell whose it was.
func fillShare(t *testing.T, h *journalv2.Handle, share int, ids []journalv2.FileID, size int64) ([]written, *journalv2.NoSpaceError) {
	t.Helper()

	var out []written
	offs := make([]int64, len(ids))
	for i := range 2 * journalv2test.FixtureMaxBytes / size {
		k := int(i) % len(ids)
		v, err := tryWrite(t, h, ids[k], offs[k], pattern(fmt.Sprint("share", share, i), int(size)))
		stub(t, "WriteAt", err)
		if err != nil {
			return out, refusal(t, err, "a write past the share's limit", journalv2.LimitShare, journalv2.UnitBytes)
		}
		out = append(out, written{share, ids[k], journalv2.Extent{Off: offs[k], Len: size}, v})
		offs[k] += size
	}
	t.Fatalf("%d bytes written into a share were never refused by its limit", 2*journalv2test.FixtureMaxBytes)
	return nil, nil
}

// fillTails goes on writing, after a fill refused at size, ever smaller
// writes through hs to eight files from file first, each file until a write
// is refused, so the active segments' tails hold no more than the smallest
// write's record. refused checks each refusal.
func fillTails(t *testing.T, hs []*journalv2.Handle, first int, refused func(err error)) {
	t.Helper()

	for _, size := range []int{4096, 64} {
		active := map[int]int64{}
		for k := range 8 {
			active[k] = 1 << 40
		}
		for i := 0; len(active) > 0; i++ {
			if i > 2*journalv2test.FixtureMaxBytes/size {
				t.Fatalf("writes of %d bytes into a journal that refused larger ones were never refused", size)
			}
			for k, off := range active {
				_, err := tryWrite(t, hs[k%len(hs)], journalv2test.File(first+k), off, pattern(fmt.Sprint("tail", size, k), size))
				stub(t, "WriteAt", err)
				if err != nil {
					refused(err)
					delete(active, k)
					continue
				}
				active[k] = off + int64(size)
			}
		}
	}
}

// markFile marks every write of ws offloaded, one call per file.
func markFile(t *testing.T, f *journalv2test.Fixture, ws []written) {
	t.Helper()

	byFile := map[journalv2.FileID][]written{}
	for _, w := range ws {
		byFile[w.id] = append(byFile[w.id], w)
	}
	for id, fws := range byFile {
		extents := make([]journalv2.Extent, len(fws))
		for i, w := range fws {
			extents[i] = w.extent
		}
		err := f.Share(fws[0].share).MarkOffloaded(t.Context(), id, extents, fws[0].version, fws[len(fws)-1].version)
		stub(t, "MarkOffloaded", err)
		if err != nil {
			t.Fatalf("mark a file offloaded: %v", err)
		}
	}
}

// evict marks each write offloaded and releases it, as the engine evicts
// content its offload commit covers, and returns the bytes released.
func evict(t *testing.T, h *journalv2.Handle, ws []written) int64 {
	t.Helper()

	var n int64
	for _, w := range ws {
		mark(t, h, w.id, w.extent, w.version)
		err := h.Release(t.Context(), w.id, []journalv2.Extent{w.extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatalf("release an offloaded extent: %v", err)
		}
		n += w.extent.Len
	}
	return n
}

// syncNow makes every operation on ids that returned so far durable.
func syncNow(t *testing.T, h *journalv2.Handle, ids ...journalv2.FileID) {
	t.Helper()

	err := h.Sync(t.Context(), ids...)
	stub(t, "Sync", err)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// drainHeadroom issues removals of nothing held through h, each a record
// drawn on the headroom, until less than half of full remains.
func drainHeadroom(t *testing.T, f *journalv2test.Fixture, h *journalv2.Handle, id journalv2.FileID, full int64) {
	t.Helper()

	for i := int64(0); f.J.Stats().HeadroomBytes >= full/2; i++ {
		if i == 8192 {
			t.Fatalf("HeadroomBytes = %d of %d after %d removals at the limit, want it below half", f.J.Stats().HeadroomBytes, full, i)
		}
		_, err := h.Deallocate(t.Context(), id, journalv2.Extent{Off: 1<<40 + i*4096, Len: 4096})
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("a removal at the limit with the headroom above half: %v", err)
		}
	}
}

// headroom returns the headroom as allocated now, which must be some.
func headroom(t *testing.T, f *journalv2test.Fixture) int64 {
	t.Helper()

	full := f.J.Stats().HeadroomBytes
	if full <= 0 {
		t.Fatalf("HeadroomBytes = %d, want the headroom allocated", full)
	}
	return full
}

// repackAll seals the segments left idle, then repacks scope until a pass
// frees nothing, and returns the storage freed. A pass that frees nothing
// before anything was freed is retried while the idle seals may still be
// landing.
func repackAll(t *testing.T, f *journalv2test.Fixture, scope journalv2.RepackScope) int64 {
	t.Helper()

	f.Advance(2 * journalv2test.IdleSealAge)
	var total int64
	deadline := time.Now().Add(backgroundWait)
	for range 256 {
		freed, err := f.J.Repack(t.Context(), journalv2test.FixtureMaxBytes, scope)
		stub(t, "Repack", err)
		if err != nil {
			t.Fatalf("repack: %v", err)
		}
		total += freed
		if freed == 0 {
			if total > 0 || time.Now().After(deadline) {
				return total
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("repack kept freeing storage after 256 passes")
	return 0
}

// segmentNames returns the segment files now in the journal's directory.
func segmentNames(t *testing.T, f *journalv2test.Fixture) []string {
	t.Helper()

	names, err := f.Seam.List()
	if err != nil {
		t.Fatalf("list the journal's directory: %v", err)
	}
	return slices.DeleteFunc(names, func(n string) bool { return !strings.HasSuffix(n, ".seg") })
}

// segmentCalls returns, per op and segment file now in the journal's
// directory, how many calls the seam has counted, leaving out every count of
// zero: a spare renamed into a segment's name between two snapshots then
// changes neither.
func segmentCalls(t *testing.T, f *journalv2test.Fixture) map[string]int {
	t.Helper()

	ops := []string{
		journalv2test.OpOpen, journalv2test.OpAllocate, journalv2test.OpWrite, journalv2test.OpRead,
		journalv2test.OpReadDirect, journalv2test.OpSync, journalv2test.OpTruncate, journalv2test.OpClose,
		journalv2test.OpRename, journalv2test.OpRemove,
	}
	out := map[string]int{}
	for _, name := range segmentNames(t, f) {
		for _, op := range ops {
			if n := f.Seam.CallsFor(op, name); n > 0 {
				out[op+" "+name] = n
			}
		}
	}
	return out
}

// segmentWrites returns how many writes the seam has counted on the segment
// files now in the journal's directory.
func segmentWrites(t *testing.T, f *journalv2test.Fixture) (n int) {
	t.Helper()

	for _, name := range segmentNames(t, f) {
		n += f.Seam.CallsFor(journalv2test.OpWrite, name)
	}
	return n
}

// retired reports whether segment id is no longer the journal's: not listed,
// and unlinked or renamed away at the seam.
func retired(f *journalv2test.Fixture, id journalv2.SegmentID) bool {
	if _, ok := segmentInfo(f, id); ok {
		return false
	}
	name := segmentName(id)
	return f.Seam.CallsFor(journalv2test.OpRemove, name)+f.Seam.CallsFor(journalv2test.OpRename, name) > 0
}

// refusedAtOnce runs call, which the journal must refuse, with every call on
// a segment held at the seam, and returns its error. It fails the test if the
// call reaches a segment, if the seam counts any call on one meanwhile, or if
// the call does not return within backgroundWait.
//
// decision: only calls on segment files are held and counted. Spares are made
// on the journal's own goroutines, on files of their own, and may be in
// flight when the refusal runs; counting them would fail a correct journal.
// The ceiling: a refusal that creates a file of its own is not seen. Overturned
// if a refusal can be shown to touch a spare.
func refusedAtOnce(t *testing.T, f *journalv2test.Fixture, what string, call func() error) error {
	t.Helper()

	before := segmentCalls(t, f)
	held := f.Seam.Hold(t, "*", "*.seg")
	done := make(chan error, 1)
	go func() { done <- call() }()

	var err error
	select {
	case err = <-done:
	case <-held.Reached():
		held.Release()
		<-done
		t.Fatalf("%s reached a segment before it was refused", what)
	case <-time.After(backgroundWait):
		t.Fatalf("%s did not return within %v with storage held: it waits", what, backgroundWait)
	}
	held.Release()
	if after := segmentCalls(t, f); !maps.Equal(before, after) {
		t.Fatalf("%s made calls on segments before it was refused", what)
	}
	return err
}

// heldIn returns the held entries of file id that segment seg backs.
func heldIn(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID, seg journalv2.SegmentID) []journalv2.IndexEntry {
	t.Helper()

	es, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect a file: %v", err)
	}
	var out []journalv2.IndexEntry
	for _, e := range es {
		if e.Kind == journalv2.EntryHeld && e.Location.Segment == seg {
			out = append(out, e)
		}
	}
	return out
}

// segmentInfo returns the accounting of segment id, and whether the journal
// owns it.
func segmentInfo(f *journalv2test.Fixture, id journalv2.SegmentID) (journalv2.SegmentInfo, bool) {
	for _, s := range f.J.Inspect().Segments() {
		if s.ID == id {
			return s, true
		}
	}
	return journalv2.SegmentInfo{}, false
}

// soleChunk is the size of the writes soleSegment makes.
const soleChunk = 64 << 10

// soleSegment writes 3 MiB of file id through h in 64 KiB writes, syncs it,
// marks it offloaded in one call, and returns a sealed segment holding
// nothing but held extents of the file, with those extents: the middle of a
// run three segments long. The one mark goes after the content, so no
// offloaded record lands in that segment.
func soleSegment(t *testing.T, f *journalv2test.Fixture, h *journalv2.Handle, id journalv2.FileID) (journalv2.SegmentInfo, []journalv2.IndexEntry) {
	t.Helper()

	var extents []journalv2.Extent
	var first, last journalv2.Version
	for off := int64(0); off < 3<<20; off += soleChunk {
		last = write(t, h, id, off, pattern(fmt.Sprint("sole", off), soleChunk))
		if off == 0 {
			first = last
		}
		extents = append(extents, journalv2.Extent{Off: off, Len: soleChunk})
	}
	syncNow(t, h, id)
	err := h.MarkOffloaded(t.Context(), id, extents, first, last)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark the file offloaded: %v", err)
	}

	for _, s := range f.J.Inspect().Segments() {
		es := heldIn(t, f, id, s.ID)
		var n int64
		for _, e := range es {
			n += e.Extent.Len
		}
		if s.Sealed && n > 0 && s.HeldBytes == n && s.RetainedBytes == 0 {
			return s, es
		}
	}
	t.Fatal("no sealed segment holds a piece of the 3 MiB file and nothing else")
	return journalv2.SegmentInfo{}, nil
}

// TestRFC1B_Reservation checks that concurrent writers at the journal's
// maximum never take its footprint past it: every write either fits or is
// refused naming the journal's limit.
func TestRFC1B_Reservation(t *testing.T) {
	f := journalv2test.New(t)
	h0, h1 := f.Share(0), f.Share(1)

	const writers, size = 8, 256 << 10
	var (
		mu       sync.Mutex
		accepted int64
		over     []journalv2.Stats
		refusals []error
		wg       sync.WaitGroup
	)
	for w := range writers {
		h := []*journalv2.Handle{h0, h1}[w%2]
		wg.Go(func() {
			id := journalv2test.File(20 + w)
			for i := range int64(journalv2test.FixtureMaxBytes / size) {
				_, err := tryWrite(t, h, id, i*size, pattern(fmt.Sprint("race", w, i), size))
				s := f.J.Stats()
				mu.Lock()
				if s.UsedBytes > s.MaxBytes {
					over = append(over, s)
				}
				if err != nil {
					refusals = append(refusals, err)
					mu.Unlock()
					return
				}
				accepted += size
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	for _, err := range refusals {
		wantNoSpace(t, err, journalv2.LimitJournal, "a write racing others at the journal's maximum")
	}
	if len(refusals) != writers {
		t.Fatalf("%d of %d writers were refused, each writing the journal's maximum: want all of them", len(refusals), writers)
	}
	if len(over) != 0 {
		t.Fatalf("UsedBytes = %d above MaxBytes %d while writers raced at the limit", over[0].UsedBytes, over[0].MaxBytes)
	}
	s := f.J.Stats()
	var shares int64
	for _, ss := range s.Shares {
		shares += ss.UsedBytes
	}
	if accepted > s.MaxBytes || shares > s.MaxBytes || s.UsedBytes > s.MaxBytes {
		t.Fatalf("accepted %d bytes, shares use %d, the journal %d: want each at most MaxBytes %d", accepted, shares, s.UsedBytes, s.MaxBytes)
	}
}

// reserveAheadExtents is the index bound the reserve-ahead check runs under:
// room for its 10^4 reserved writes, counted as the reservation counts them,
// and for a writer to fill what is left.
const reserveAheadExtents = 40_000

// reserveAheadSpares is the spare pool the reserve-ahead check runs with: the
// six spares a reservation of four 1 MiB writes takes, one per cap its bytes
// can cross plus one, and two for the rolls of the writer beside it. The
// fixture's pool of one per stream would refuse that reservation naming the
// pool.
const reserveAheadSpares = 8

// reserve reserves n record bytes for writes writes through h, retrying a
// refusal for want of a spare while the pool refills, and fails the test if
// it is refused.
func reserve(t *testing.T, h *journalv2.Handle, n int64, writes int) *journalv2.Reservation {
	t.Helper()

	var r *journalv2.Reservation
	err := journalv2test.RetrySpare(func() (err error) {
		r, err = h.Reserve(n, writes)
		return err
	})
	stub(t, "Reserve", err)
	if err != nil {
		t.Fatalf("reserve %d bytes for %d writes: %v", n, writes, err)
	}
	return r
}

// TestRFC1B_ReserveAhead checks that a reservation holds what it took: its
// writes succeed after another writer filled the share to its limit, while a
// plain write and a further reservation are refused naming that limit, and
// Close returns what it left unused; that reserved writes beside a stream
// another writer keeps rolling are never refused; that a restart drops a
// reservation; that 10^4 reserved writes succeed beside a writer that filled
// the index, while a plain one is refused naming it; and that a reserved
// write succeeds with the headroom below half.
//
// The copy is 4 MiB in 1 MiB writes beside 32 MiB of other writes, not 1 GiB
// beside 10 GiB: the fixture's journal holds 64 MiB, and the spare pool must
// hold what the reservation takes.
func TestRFC1B_ReserveAhead(t *testing.T) {
	f := journalv2test.NewWith(t, func(s *journalv2.Settings) {
		s.ExtentLimit = reserveAheadExtents
		s.SpareTarget = reserveAheadSpares
	})
	a := limitedShare(t, f, limitedA, 4<<20)

	const piece, chunk = 128 << 10, 256 << 10
	r := reserve(t, a, 1<<20, 4)
	ws, _ := fillShare(t, a, 4, []journalv2.FileID{journalv2test.File(40), journalv2test.File(41)}, chunk)
	for i := range 4 {
		id := journalv2test.File(42 + i%2)
		_, err := r.WriteAt(t.Context(), id, int64(i/2)*piece, pattern(fmt.Sprint("reserved", i), piece), 0)
		stub(t, "Reservation.WriteAt", err)
		if err != nil {
			t.Fatalf("reserved write %d, within the reservation, with the share at its limit: %v", i, err)
		}
	}
	// The fill was refused at this size, and the reserved writes took nothing
	// from outside the reservation, so it does not fit.
	_, err := a.WriteAt(t.Context(), journalv2test.File(40), 1<<30, pattern("plain", chunk), 0)
	refusal(t, err, "a plain write with the share at its limit", journalv2.LimitShare, journalv2.UnitBytes)
	_, err = a.Reserve(1<<20, 1)
	refusal(t, err, "a reservation past the share's limit", journalv2.LimitShare, journalv2.UnitBytes)

	if got := f.J.Stats().Shares[limitedA].ReservedBytes; got <= 0 {
		t.Fatalf("the share's ReservedBytes = %d with half a reservation unused, want it counted", got)
	}
	stub(t, "Reservation.Close", r.Close())
	if got := f.J.Stats().Shares[limitedA].ReservedBytes; got != 0 {
		t.Fatalf("the share's ReservedBytes = %d after the reservation closed, want 0", got)
	}
	// What the reservation left unused, about half a megabyte, is the share's
	// again.
	last := ws[len(ws)-1]
	write(t, a, last.id, last.extent.Off+last.extent.Len, pattern("returned", chunk))

	// Reserved writes beside a writer rolling the other share's streams.
	b, other := f.Share(1), f.Share(0)
	const big = 1 << 20
	rb := reserve(t, b, 4*(big+4096), 4)
	others := make(chan error, 1)
	go func() {
		for i := range int64(32) {
			if _, err := tryWrite(t, other, journalv2test.File(50+int(i%2)), i/2*big, pattern(fmt.Sprint("other", i), big)); err != nil {
				others <- called("WriteAt", err)
				return
			}
		}
		others <- nil
	}()
	for i := range 4 {
		_, err := rb.WriteAt(t.Context(), journalv2test.File(60+i%2), int64(i/2)*big, pattern(fmt.Sprint("copy", i), big), 0)
		stub(t, "Reservation.WriteAt", err)
		if err != nil {
			t.Fatalf("reserved write %d beside a rolling writer was refused: %v", i, err)
		}
	}
	if err := <-others; err != nil {
		var ce *callError
		if errors.As(err, &ce) {
			stub(t, ce.call, ce.err)
		}
		t.Fatalf("an unreserved write below the journal's maximum: %v", err)
	}
	stub(t, "Reservation.Close", rb.Close())

	// A reservation left open across a restart.
	reserve(t, b, 1<<20, 1)
	if got := f.J.Stats().ReservedBytes; got <= 0 {
		t.Fatalf("ReservedBytes = %d with a reservation open, want it counted", got)
	}
	_ = a.Close()
	f.Reopen()
	s := f.J.Stats()
	if s.ReservedBytes != 0 || s.Shares[journalv2test.Tag(1)].ReservedBytes != 0 {
		t.Fatalf("ReservedBytes = %d, the share's %d after a restart, want both 0: a restart drops a reservation", s.ReservedBytes, s.Shares[journalv2test.Tag(1)].ReservedBytes)
	}

	// Reserved writes beside a writer that filled the index.
	const reserved = 10_000
	b, other = f.Share(1), f.Share(0)
	ri := reserve(t, b, reserved*writeRecordBytes(1), reserved)
	filler, scattered := journalv2test.File(70), journalv2test.File(71)
	// One-byte writes a byte apart never merge into one extent.
	for i := int64(0); ; i++ {
		if i == reserveAheadExtents {
			t.Fatalf("%d one-byte writes under an index bound of %d were never refused", i, reserveAheadExtents)
		}
		_, err := tryWrite(t, other, filler, 2*i, []byte{byte(i)})
		stub(t, "WriteAt", err)
		if err != nil {
			refusal(t, err, "a write past the index's bound", journalv2.LimitIndex, journalv2.UnitEntries)
			break
		}
	}
	for i := range int64(reserved) {
		_, err := ri.WriteAt(t.Context(), scattered, 2*i, []byte{byte(i)}, 0)
		stub(t, "Reservation.WriteAt", err)
		if err != nil {
			t.Fatalf("reserved write %d with the index full: %v", i, err)
		}
	}
	_, err = other.WriteAt(t.Context(), filler, 1<<30, []byte{1}, 0)
	refusal(t, err, "a plain write with the index full", journalv2.LimitIndex, journalv2.UnitEntries)
	stub(t, "Reservation.Close", ri.Close())
	for _, x := range []struct {
		h  *journalv2.Handle
		id journalv2.FileID
	}{{other, filler}, {b, scattered}} {
		v, err := x.h.Delete(t.Context(), x.id)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("delete a file of the full index: %v", err)
		}
		x.h.Settle(x.id, v)
	}

	// A reserved write with the headroom below half.
	full := headroom(t, f)
	rh := reserve(t, b, 4*writeRecordBytes(4096), 4)
	fillUntilRefused(t, f, []int{0}, 80, chunk)
	drainHeadroom(t, f, other, journalv2test.File(82), full)
	for i := range int64(4) {
		_, err := rh.WriteAt(t.Context(), journalv2test.File(83), i*4096, pattern(fmt.Sprint("below-half", i), 4096), 0)
		stub(t, "Reservation.WriteAt", err)
		if err != nil {
			t.Fatalf("reserved write %d with the headroom below half: %v", i, err)
		}
	}
	stub(t, "Reservation.Close", rh.Close())
}

// TestRFC1B_HandleClose checks that closing a handle waits for a write in
// flight on it, refuses every later call through it, and leaves what the
// write stored held and readable through a new handle of the same tag.
func TestRFC1B_HandleClose(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		tag := journalv2test.Tag(6 + k)
		h := limitedShare(t, f, tag, 0)
		id, p := journalv2test.File(k), pattern(fmt.Sprint("inflight", k), 4096)

		held := f.Seam.Hold(t, journalv2test.OpWrite, "*.seg")
		wrote := make(chan error, 1)
		go func() {
			_, err := h.WriteAt(t.Context(), id, 0, p, 0)
			wrote <- err
		}()
		select {
		case <-held.Reached():
		case err := <-wrote:
			stub(t, "WriteAt", err)
			t.Fatalf("share %d: the write returned (%v) without reaching storage", k, err)
		}

		// let is closed just before the hold is released. The write cannot
		// return before then, so a Close that returns before it did not wait
		// for the write. Close returning after the release is not ordered
		// against the write's caller, so nothing is read from that.
		let := make(chan struct{})
		var early atomic.Bool
		closed := make(chan error, 1)
		go func() {
			err := h.Close()
			select {
			case <-let:
			default:
				early.Store(true)
			}
			closed <- err
		}()

		// Close has begun once a new call through the handle is refused. The
		// spin is bounded in iterations, not time, and ends early if Close
		// returns while the write is still held.
		begun := false
		for range 100_000 {
			select {
			case err := <-closed:
				stub(t, "Close", err)
				t.Fatalf("share %d: Close returned (%v) while a write on the handle was held", k, err)
			default:
			}
			_, err := h.Extents(id)
			if errors.Is(err, journalv2.ErrClosed) {
				begun = true
				break
			}
			stub(t, "Extents", err)
			runtime.Gosched()
		}
		close(let)
		held.Release()
		var err error
		select {
		case err = <-closed:
		case <-time.After(backgroundWait):
			t.Fatalf("share %d: Close did not return within %v once the write was let go", k, backgroundWait)
		}
		stub(t, "Close", err)
		if err != nil {
			t.Fatalf("share %d: close: %v", k, err)
		}
		if !begun {
			t.Fatalf("share %d: calls through the handle were not refused while Close waited for the write", k)
		}
		if early.Load() {
			t.Fatalf("share %d: Close returned while the write in flight was still held at storage", k)
		}
		if err := <-wrote; err != nil {
			stub(t, "WriteAt", err)
			t.Fatalf("share %d: the write in flight at Close: %v", k, err)
		}

		_, err = h.WriteAt(t.Context(), id, 4096, p, 0)
		if !errors.Is(err, journalv2.ErrClosed) {
			stub(t, "WriteAt", err)
			t.Fatalf("share %d: a write through the closed handle returned %v, want ErrClosed", k, err)
		}

		again := limitedShare(t, f, tag, 0)
		buf := make([]byte, len(p))
		_, missing, _, _, err := again.ReadAt(t.Context(), id, 0, buf)
		stub(t, "ReadAt", err)
		if err != nil {
			t.Fatalf("share %d: read through a new handle: %v", k, err)
		}
		if len(missing) != 0 || string(buf) != string(p) {
			t.Fatalf("share %d: the write in flight at Close reads missing %+v through a new handle, want it held", k, missing)
		}
	}
}

// TestRFC1B_NamedRefusal checks that a write past a share's limit, and one
// past the journal's maximum, is refused naming that limit and the bytes it
// lacks, at once and without storage I/O, and that releasing and repacking
// that many offloaded bytes lets the same write through.
func TestRFC1B_NamedRefusal(t *testing.T) {
	f := journalv2test.New(t)

	retry := func(what string, h *journalv2.Handle, id journalv2.FileID, off int64, p []byte) {
		t.Helper()
		writes := f.Seam.Calls(journalv2test.OpWrite)
		write(t, h, id, off, p)
		if f.Seam.Calls(journalv2test.OpWrite) == writes {
			t.Fatalf("%s: the retried write stored its bytes without a write at the seam", what)
		}
	}
	// release evicts writes from the front of ws until want bytes are out.
	release := func(ws []written, want int64) {
		t.Helper()
		var n int64
		for i := 0; n < want; i++ {
			if i == len(ws) {
				t.Fatalf("released all %d bytes written, short of the %d wanted", n, want)
			}
			n += evict(t, f.Share(ws[i].share), ws[i:i+1])
		}
	}

	// The share's limit.
	a := limitedShare(t, f, limitedA, 8<<20)
	ids := []journalv2.FileID{journalv2test.File(0), journalv2test.File(1)}
	ws, _ := fillShare(t, a, 4, ids, 256<<10)
	syncNow(t, a, ids...)
	p := pattern("past-share", 256<<10)
	err := refusedAtOnce(t, f, "a write past the share's limit", func() error {
		_, err := a.WriteAt(t.Context(), ids[0], 1<<30, p, 0)
		return err
	})
	ns := refusal(t, err, "a write past the share's limit", journalv2.LimitShare, journalv2.UnitBytes)
	if ns.Missing > writeRecordBytes(len(p)) {
		t.Fatalf("the share's refusal names %d bytes missing, more than the %d-byte record it refused", ns.Missing, writeRecordBytes(len(p)))
	}
	var n int64
	for i := 0; n < ns.Missing; i++ {
		if i == len(ws) {
			t.Fatalf("released all %d bytes written, short of the %d missing", n, ns.Missing)
		}
		n += evict(t, a, ws[i:i+1])
	}
	repackAll(t, f, journalv2.RepackScope{Share: limitedA})
	retry("the share", a, ids[0], 1<<30, p)

	// The journal's maximum.
	jws := fillUntilRefused(t, f, []int{0, 1}, 10, 256<<10)
	for k := range 2 {
		syncNow(t, f.Share(k), journalv2test.File(10+k))
	}
	q := pattern("past-journal", 256<<10)
	err = refusedAtOnce(t, f, "a write past the journal's maximum", func() error {
		_, err := f.Share(0).WriteAt(t.Context(), journalv2test.File(10), 1<<30, q, 0)
		return err
	})
	ns = refusal(t, err, "a write past the journal's maximum", journalv2.LimitJournal, journalv2.UnitBytes, journalv2.UnitSegments)
	missing := ns.Missing
	if ns.Unit == journalv2.UnitSegments {
		missing *= journalv2test.FixtureSegmentCap
	}
	// The maximum counts whole segments, and released bytes return only as
	// whole segments, so two caps more than what was missing guarantee at
	// least one whole segment freed net, whatever the records around them.
	release(jws, missing+2*journalv2test.FixtureSegmentCap)
	repackAll(t, f, journalv2.RepackScope{})
	retry("the journal", f.Share(0), journalv2test.File(10), 1<<30, q)
}

// TestRFC1B_ShareLimit checks that a share at its limit is refused naming it
// while another share still writes, and that both shares' accounting, and the
// limit's refusal, are the same after a reopen.
func TestRFC1B_ShareLimit(t *testing.T) {
	f := journalv2test.New(t)
	const limit = 4 << 20
	a, b := limitedShare(t, f, limitedA, limit), f.Share(1)
	aIDs := []journalv2.FileID{journalv2test.File(0), journalv2test.File(1)}
	bID := journalv2test.File(2)

	fillShare(t, a, 4, aIDs, 128<<10)
	write(t, b, bID, 0, pattern("other-share", 128<<10))
	syncNow(t, a, aIDs...)
	syncNow(t, b, bID)

	before := f.J.Stats().Shares
	if before[limitedA].UsedBytes > limit {
		t.Fatalf("the share uses %d bytes, past its limit of %d", before[limitedA].UsedBytes, limit)
	}
	_ = a.Close()
	f.Reopen()
	a = limitedShare(t, f, limitedA, limit)
	after := f.J.Stats().Shares
	for _, tag := range []journalv2.ShareTag{limitedA, journalv2test.Tag(1)} {
		if before[tag] != after[tag] {
			t.Fatalf("share %d's accounting is %+v after a reopen, %+v before", tag, after[tag], before[tag])
		}
	}

	_, err := a.WriteAt(t.Context(), aIDs[0], 1<<30, pattern("past", 128<<10), 0)
	refusal(t, err, "a write past the share's limit after a reopen", journalv2.LimitShare, journalv2.UnitBytes)
	write(t, f.Share(1), bID, 128<<10, pattern("other-after", 128<<10))
}

// TestRFC1B_RepackAtCapacity checks that a journal refused at its maximum can
// repack, and that the repack returns space without any write succeeding
// first.
func TestRFC1B_RepackAtCapacity(t *testing.T) {
	f := journalv2test.New(t)
	ws := fillUntilRefused(t, f, []int{0, 1}, 0, 128<<10)

	// One mark per file, and one extent in three released, so the releases
	// take well under half the headroom and no segment is left holding
	// nothing to retire on its own: only repack returns this space.
	markFile(t, f, ws)
	for i, w := range ws {
		if i%3 != 0 {
			continue
		}
		err := f.Share(w.share).Release(t.Context(), w.id, []journalv2.Extent{w.extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatalf("release an offloaded extent at capacity: %v", err)
		}
	}
	if freed := repackAll(t, f, journalv2.RepackScope{}); freed <= 0 {
		t.Fatal("repack at capacity, over segments a third released, freed nothing")
	}
	write(t, f.Share(0), journalv2test.File(0), 1<<30, pattern("after-repack", 128<<10))
}

// TestRFC1B_RepackBesideShareAtLimit checks that repack copies the records of
// a share filled exactly to its limit out of segments it shares with another
// share's released content, refusing no copy by that limit, and retires every
// such segment.
func TestRFC1B_RepackBesideShareAtLimit(t *testing.T) {
	f := journalv2test.New(t)
	a, b := limitedShare(t, f, limitedA, 8<<20), f.Share(1)

	const size = 128 << 10
	var aws, bws []written

	// Which stream a file's records go to is the journal's choice, so the
	// files are picked by where their first records landed: an A file and a B
	// file whose first records share a segment go on sharing their stream.
	const candidates = 4
	var aIDs, bIDs []journalv2.FileID
	first := map[journalv2.FileID]journalv2.SegmentID{}
	for i := range 2 * candidates {
		id, h, share, ws := journalv2test.File(i), a, -1, &aws
		if i >= candidates {
			h, share, ws = b, 1, &bws
		}
		*ws = append(*ws, written{share, id, journalv2.Extent{Off: 0, Len: size}, write(t, h, id, 0, pattern(fmt.Sprint("first", i), size))})
		first[id] = heldEntry(t, f, id).Location.Segment
	}
	for i := range candidates {
		for j := candidates; j < 2*candidates; j++ {
			aid, bid := journalv2test.File(i), journalv2test.File(j)
			if first[aid] != first[bid] {
				continue
			}
			if !slices.Contains(aIDs, aid) {
				aIDs = append(aIDs, aid)
			}
			if !slices.Contains(bIDs, bid) {
				bIDs = append(bIDs, bid)
			}
		}
	}
	if len(aIDs) == 0 {
		t.Fatalf("the first records of %d files of each share never shared a segment", candidates)
	}

	var ns *journalv2.NoSpaceError
	offs := map[journalv2.FileID]int64{}
	for i := 0; ns == nil; i++ {
		id := aIDs[i%len(aIDs)]
		off := size + offs[id]
		v, err := tryWrite(t, a, id, off, pattern(fmt.Sprint("a", i), size))
		stub(t, "WriteAt", err)
		if err != nil {
			ns = refusal(t, err, "a write past share A's limit", journalv2.LimitShare, journalv2.UnitBytes)
			break
		}
		offs[id] += size
		aws = append(aws, written{-1, id, journalv2.Extent{Off: off, Len: size}, v})
		bid := bIDs[i%len(bIDs)]
		boff := size + offs[bid]
		offs[bid] += size
		bws = append(bws, written{1, bid, journalv2.Extent{Off: boff, Len: size}, write(t, b, bid, boff, pattern(fmt.Sprint("b", i), size))})
	}
	// What the refused write lacked says what is left: a write whose record
	// is exactly that fills the share to its limit.
	if free := writeRecordBytes(size) - ns.Missing; free >= writeRecordBytes(1) {
		id, off := aIDs[0], int64(1<<30)
		n := int(free - writeRecordBytes(0))
		aws = append(aws, written{-1, id, journalv2.Extent{Off: off, Len: int64(n)}, write(t, a, id, off, pattern("exact", n))})
	}
	_, err := a.WriteAt(t.Context(), aIDs[0], 1<<31, []byte{1}, 0)
	refusal(t, err, "a write with share A filled to its limit", journalv2.LimitShare, journalv2.UnitBytes)
	for _, w := range aws {
		mark(t, a, w.id, w.extent, w.version)
	}

	tags := map[journalv2.SegmentID]map[journalv2.ShareTag]bool{}
	for _, es := range f.J.Inspect().All() {
		for _, e := range es {
			if tags[e.Location.Segment] == nil {
				tags[e.Location.Segment] = map[journalv2.ShareTag]bool{}
			}
			tags[e.Location.Segment][e.Tag] = true
		}
	}
	var mixed []journalv2.SegmentID
	for seg, ts := range tags {
		if ts[limitedA] && ts[journalv2test.Tag(1)] {
			mixed = append(mixed, seg)
		}
	}
	if len(mixed) == 0 {
		t.Fatal("no segment holds records of both shares, written interleaved")
	}

	evict(t, b, bws)
	for i := range candidates {
		syncNow(t, a, journalv2test.File(i))
		syncNow(t, b, journalv2test.File(candidates+i))
	}
	aHeld := f.J.Stats().Shares[limitedA].HeldBytes

	repackAll(t, f, journalv2.RepackScope{})
	for _, seg := range mixed {
		if _, ok := segmentInfo(f, seg); ok {
			t.Fatalf("segment %d, holding share A's records beside B's released ones, is still owned after repack", seg)
		}
	}
	if got := f.J.Stats().Shares[limitedA].HeldBytes; got != aHeld {
		t.Fatalf("share A holds %d bytes after the repack, %d before: a copy was dropped", got, aHeld)
	}
}

// TestRFC1B_HeadroomIsAllocated checks that once the filesystem is full,
// content writes are refused with its ENOSPC while releases, removals and
// marks still succeed, drawing on headroom allocated ahead, and a repack
// still returns space.
func TestRFC1B_HeadroomIsAllocated(t *testing.T) {
	f := journalv2test.New(t)

	var kept [2][]written
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		for i := range int64(48) {
			off := i * 64 << 10
			v := write(t, h, id, off, pattern(fmt.Sprint("kept", k, i), 64<<10))
			kept[k] = append(kept[k], written{k, id, journalv2.Extent{Off: off, Len: 64 << 10}, v})
		}
		write(t, h, journalv2test.File(2+k), 0, pattern(fmt.Sprint("removed", k), 64<<10))
		syncNow(t, h, id, journalv2test.File(2+k))
	}
	unmarked := write(t, f.Share(0), journalv2test.File(4), 0, pattern("unmarked", 64<<10))
	syncNow(t, f.Share(0), journalv2test.File(4))

	fullDevice(t, f)

	hs := []*journalv2.Handle{f.Share(0), f.Share(1)}
	refused := false
	for i := range int64(2 * journalv2test.FixtureMaxBytes / (256 << 10)) {
		_, err := tryWrite(t, hs[i%2], journalv2test.File(5+int(i%2)), i/2*256<<10, pattern(fmt.Sprint("new", i), 256<<10))
		if err != nil {
			wantENOSPC(t, err, "a content write on a full device")
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("content writes went on succeeding on a full device")
	}
	// The active segments' tails would otherwise take the records below
	// without the headroom.
	fillTails(t, hs, 20, func(err error) { wantENOSPC(t, err, "a small content write on a full device") })

	for k := range 2 {
		h := f.Share(k)
		for i, w := range kept[k] {
			if i%3 != 0 {
				evict(t, h, []written{w})
			}
		}
		_, err := h.Truncate(t.Context(), journalv2test.File(2+k), 4096)
		stub(t, "Truncate", err)
		if err != nil {
			t.Fatalf("share %d: truncate on a full device: %v", k, err)
		}
		_, err = h.Delete(t.Context(), journalv2test.File(2+k))
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("share %d: delete on a full device: %v", k, err)
		}
		_, err = h.Deallocate(t.Context(), kept[k][0].id, kept[k][0].extent)
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("share %d: deallocate on a full device: %v", k, err)
		}
	}
	mark(t, f.Share(0), journalv2test.File(4), journalv2.Extent{Off: 0, Len: 64 << 10}, unmarked)

	if freed := repackAll(t, f, journalv2.RepackScope{}); freed <= 0 {
		t.Fatal("repack over segments two thirds released freed nothing on a full device")
	}
}

// TestRFC1B_OffloadedRecordsBelowHalfTheHeadroom checks that below half the
// headroom an offload marks every extent of a file of 150,000 extents in
// memory, appending nothing and drawing nothing, so a crash leaves them dirty
// again; and that a hold and a stamp are refused naming the headroom while
// releases and removals still succeed.
func TestRFC1B_OffloadedRecordsBelowHalfTheHeadroom(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	full := headroom(t, f)

	// One-byte writes a byte apart never merge into one extent.
	const extents = 150_000
	var through journalv2.Version
	for i := range int64(extents) {
		through = write(t, h, id, 2*i, []byte{byte(i)})
	}
	syncNow(t, h, id)
	ws := fillUntilRefused(t, f, []int{1}, 10, 256<<10)
	other := f.Share(1)
	drainHeadroom(t, f, other, journalv2test.File(11), full)

	draws, writes := f.J.Stats().Counters.HeadroomDraws, segmentWrites(t, f)
	// An offer can end at the bound on the segments it pins, so the file is
	// offered until an offer has nothing left in it.
	for pass := 0; ; pass++ {
		if pass == 64 {
			t.Fatal("the file still offered dirty extents after 64 offloads")
		}
		empty := false
		err := h.Offload(t.Context(), id, through, 1<<40, 0, func(_ context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
			if len(o.Dirty) == 0 {
				empty = true
				return nil
			}
			es := make([]journalv2.Extent, len(o.Dirty))
			for i, d := range o.Dirty {
				es[i] = d.Extent
			}
			if err := report(es); err != nil {
				return called("report", err)
			}
			return nil
		})
		var ce *callError
		if errors.As(err, &ce) {
			stub(t, ce.call, ce.err)
		}
		stub(t, "Offload", err)
		if err != nil {
			t.Fatalf("offload below half the headroom: %v", err)
		}
		if empty {
			break
		}
	}
	if got := segmentWrites(t, f); got != writes {
		t.Fatalf("the offload below half the headroom made %d writes to segments, want none: offloaded records are not appended", got-writes)
	}
	if got := f.J.Stats().Counters.HeadroomDraws; got != draws {
		t.Fatalf("HeadroomDraws moved by %d over the offload, want 0", got-draws)
	}
	held := func(offloaded bool) (n int64) {
		t.Helper()
		es, err := f.J.Inspect().File(id)
		stub(t, "Inspect.File", err)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Kind == journalv2.EntryHeld && e.Offloaded == offloaded {
				n += e.Extent.Len
			}
		}
		return n
	}
	if got := held(true); got != extents {
		t.Fatalf("%d bytes of the file are held offloaded after the offload, want all %d", got, extents)
	}
	if got := f.J.Stats().Shares[journalv2test.Tag(0)].DirtyBytes; got != 0 {
		t.Fatalf("share 0's DirtyBytes = %d after its one file was offloaded, want 0, so all of it is evictable", got)
	}

	err := h.Hold(t.Context(), journalv2.ShareID{1}, 1, map[journalv2.FileID]journalv2.Version{id: through})
	refusal(t, err, "a hold below half the headroom", journalv2.LimitHeadroom)
	err = h.Stamp(t.Context(), id, through, 1)
	refusal(t, err, "a stamp below half the headroom", journalv2.LimitHeadroom)
	evict(t, other, ws[:2])
	_, err = other.Truncate(t.Context(), journalv2test.File(10), 0)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("a truncate below half the headroom: %v", err)
	}

	// Synced first, so a journal that did append the marks would keep them.
	syncNow(t, h, id)
	f.Crash()
	if got := held(false); got != extents {
		t.Fatalf("%d bytes of the file are dirty after a crash, want all %d: its marks were not persisted", got, extents)
	}
}

// TestRFC1B_RecordsWithoutBytesAtTheLimit checks that at the journal's
// maximum a truncate, a deallocate, a delete, a mark, a release and a seal
// with its footer all succeed, none refused for space, each counted as a draw
// on the headroom; and that a content write whose new segment the
// filesystem cannot allocate is refused with its ENOSPC, not the journal's
// own refusal.
func TestRFC1B_RecordsWithoutBytesAtTheLimit(t *testing.T) {
	f := journalv2test.New(t)
	for k := range 2 {
		write(t, f.Share(k), journalv2test.File(10+k), 0, pattern(fmt.Sprint("pre", k), 64<<10))
		syncNow(t, f.Share(k), journalv2test.File(10+k))
	}
	write(t, f.Share(0), journalv2test.File(12), 0, pattern("deleted", 64<<10))
	ws := fillUntilRefused(t, f, []int{0, 1}, 0, 256<<10)
	h0, h1 := f.Share(0), f.Share(1)

	s := f.J.Stats()
	draws, sealed := s.Counters.HeadroomDraws, s.Sealed
	// The active segments' tails would otherwise take the records below
	// without the headroom.
	fillTails(t, []*journalv2.Handle{h0, h1}, 20, func(err error) {
		wantNoSpace(t, err, journalv2.LimitJournal, "a small write at the journal's maximum")
	})

	try := func(call string, err error) {
		t.Helper()
		stub(t, call, err)
		if err != nil {
			t.Fatalf("%s at the journal's maximum: %v", call, err)
		}
	}
	_, err := h0.Truncate(t.Context(), journalv2test.File(10), 4096)
	try("Truncate", err)
	_, err = h1.Deallocate(t.Context(), journalv2test.File(11), journalv2.Extent{Off: 0, Len: 4096})
	try("Deallocate", err)
	_, err = h0.Delete(t.Context(), journalv2test.File(12))
	try("Delete", err)
	w := ws[0]
	try("MarkOffloaded", f.Share(w.share).MarkOffloaded(t.Context(), w.id, []journalv2.Extent{w.extent}, w.version, w.version))
	try("Release", f.Share(w.share).Release(t.Context(), w.id, []journalv2.Extent{w.extent}))

	f.Advance(2 * journalv2test.IdleSealAge)
	eventually(t, "no segment was sealed at the journal's maximum", func() bool { return f.J.Stats().Sealed > sealed })
	for _, seg := range f.J.Inspect().Segments() {
		if seg.Failed {
			t.Fatalf("segment %d failed an append at the journal's maximum", seg.ID)
		}
	}
	// Five records and at least one seal.
	if got := f.J.Stats().Counters.HeadroomDraws - draws; got < 6 {
		t.Fatalf("HeadroomDraws moved by %d over five records and a seal at the limit, want at least 6", got)
	}

	// A second journal, below its maximum, on a full device.
	g := journalv2test.NewUnwatched(t)
	fullDevice(t, g)
	for i := range int64(2 * journalv2test.FixtureMaxBytes / (256 << 10)) {
		_, err := tryWrite(t, g.Share(int(i%2)), journalv2test.File(int(i%2)), i/2*256<<10, pattern(fmt.Sprint("enospc", i), 256<<10))
		if err != nil {
			wantENOSPC(t, err, "a content write whose new segment cannot be allocated")
			return
		}
	}
	t.Fatal("content writes went on succeeding on a full device")
}

// TestRFC1B_NoVersionRepeats checks that a version a crash lost is not issued
// again and that the journal's floor holds after a reopen; that a journal
// opened with no version limit issues nothing; and that writes and draws are
// refused naming the version limit once they reach it and resume once it is
// raised.
func TestRFC1B_NoVersionRepeats(t *testing.T) {
	f := journalv2test.New(t)
	above := func(what string, v journalv2.Version, floors ...journalv2.Version) {
		t.Helper()
		for _, fl := range floors {
			if v.Compare(fl) <= 0 {
				t.Fatalf("%s is version %+v, not above %+v", what, v, fl)
			}
		}
	}

	// The write is never synced, so the crash loses it. The drop stays until
	// the crash has ended the old journal's view of the seam, so no sync of A
	// still on its way lands between clearing it and the crash; the reopened
	// journal's syncs are dropped until it is cleared, which only a second
	// crash would see.
	f.Drop(journalv2test.OpSync, "*.seg")
	a := write(t, f.Share(0), journalv2test.File(0), 0, pattern("lost", 4096))
	f.Crash()
	f.Seam.SetFault(nil)

	_, missing, _, _, err := f.Share(0).ReadAt(t.Context(), journalv2test.File(0), 0, make([]byte, 4096))
	stub(t, "ReadAt", err)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) == 0 {
		t.Fatal("the unsynced write is held after the crash, so the check cannot tell a repeated version")
	}
	floor := f.Floor()
	above("the first write after the crash", write(t, f.Share(1), journalv2test.File(1), 0, pattern("after", 4096)), a, floor)
	v, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatal(err)
	}
	above("the first draw after the crash", v, a, floor)

	f.ReopenWithoutLimit()
	_, err = f.Share(0).WriteAt(t.Context(), journalv2test.File(0), 0, pattern("no-limit", 4096), 0)
	stub(t, "WriteAt", err)
	wantNoSpace(t, err, journalv2.LimitVersion, "a write before any version limit")
	_, err = f.J.Draw()
	stub(t, "Draw", err)
	wantNoSpace(t, err, journalv2.LimitVersion, "a draw before any version limit")

	// One limit, set once, and drawn up to.
	floor = f.Floor()
	limit := journalv2.Version{Epoch: floor.Epoch, Counter: floor.Counter + 16}
	if err := f.J.SetVersionLimit(limit); err != nil {
		stub(t, "SetVersionLimit", err)
		t.Fatalf("set a version limit: %v", err)
	}
	var last journalv2.Version
	for drawn := 0; ; drawn++ {
		if drawn > 16 {
			t.Fatalf("%d draws under a limit 16 above the floor were never refused", drawn)
		}
		d, err := f.J.Draw()
		if err != nil {
			refusal(t, err, "a draw at the version limit", journalv2.LimitVersion, journalv2.UnitVersions)
			break
		}
		above("a draw below the limit", d, floor, last)
		if d.Compare(limit) >= 0 {
			t.Fatalf("a draw issued %+v, at or above the limit %+v", d, limit)
		}
		last = d
	}
	_, err = f.Share(0).WriteAt(t.Context(), journalv2test.File(0), 0, pattern("limit", 4096), 0)
	refusal(t, err, "a write at the version limit", journalv2.LimitVersion, journalv2.UnitVersions)

	if err := f.J.SetVersionLimit(journalv2.Version{Epoch: limit.Epoch, Counter: limit.Counter + 1000}); err != nil {
		stub(t, "SetVersionLimit", err)
		t.Fatalf("raise the version limit: %v", err)
	}
	above("a write once the limit was raised", write(t, f.Share(0), journalv2test.File(0), 0, pattern("raised", 4096)), last)
	d, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatalf("a draw once the limit was raised: %v", err)
	}
	above("a draw once the limit was raised", d, last)
}

// TestRFC1B_FloorBuckets checks that deleting other files does not make a
// fill of a file with no entry stale, while deleting and recreating the file
// itself does.
//
// decision: four other files are deleted, not 10^4. A file with no entry is
// checked against its bucket, and 10^4 deletes over 4,096 buckets raise X's
// with probability about 91%, so a correct journal would refuse X's fill. Four
// raise it with probability about 0.1%, and the file IDs are fixed, so a
// journal whose hash puts one of them in X's bucket fails every run rather
// than now and then. Overturned if the bucket of a file becomes observable,
// so the check can pick files that miss X's.
func TestRFC1B_FloorBuckets(t *testing.T) {
	f := journalv2test.New(t)
	h, other := f.Share(0), f.Share(1)
	v, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatal(err)
	}

	asOf := func(id journalv2.FileID) journalv2.Version {
		t.Helper()
		_, missing, _, as, err := h.ReadAt(t.Context(), id, 0, make([]byte, 4096))
		stub(t, "ReadAt", err)
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 1 {
			t.Fatalf("a read of a file never written reports missing %+v, want it all", missing)
		}
		return as
	}
	x, y := journalv2test.File(0), journalv2test.File(1)
	xAsOf, yAsOf := asOf(x), asOf(y)

	for i := range 4 {
		id := journalv2test.File(100 + i)
		write(t, other, id, 0, []byte{byte(i)})
		d, err := other.Delete(t.Context(), id)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("delete file %d: %v", i, err)
		}
		other.Settle(id, d)
	}
	p := pattern("fill-x", 4096)
	err = h.Fill(t.Context(), x, 0, p, xAsOf, v)
	stub(t, "Fill", err)
	if err != nil {
		t.Fatalf("a fill of a file with no entry, after deletes of others: %v", err)
	}
	buf := make([]byte, len(p))
	_, missing, _, _, err := h.ReadAt(t.Context(), x, 0, buf)
	stub(t, "ReadAt", err)
	if err != nil || len(missing) != 0 || string(buf) != string(p) {
		t.Fatalf("the accepted fill reads missing %+v (%v), want its bytes served", missing, err)
	}

	d, err := h.Delete(t.Context(), y)
	stub(t, "Delete", err)
	if err != nil {
		t.Fatal(err)
	}
	h.Settle(y, d)
	write(t, h, y, 64<<10, pattern("recreated", 4096))
	err = h.Fill(t.Context(), y, 0, pattern("fill-y", 4096), yAsOf, v)
	stub(t, "Fill", err)
	if !errors.Is(err, journalv2.ErrStaleFill) {
		t.Fatalf("a fill of a file deleted and recreated since its read returned %v, want ErrStaleFill", err)
	}
	_, missing, _, _, err = h.ReadAt(t.Context(), y, 0, make([]byte, 4096))
	stub(t, "ReadAt", err)
	if err != nil || len(missing) != 1 {
		t.Fatalf("the refused fill left missing %+v (%v), want the range still missing", missing, err)
	}
}

// TestRFC1B_SegmentAndRecordAccounting checks that a record written into an
// allocated segment moves only its share's usage, by its record bytes, with
// nothing left reserved; that an idle seal truncates the segment's unwritten
// tail and the journal's usage falls by it; and that a segment sealed full
// keeps its cap.
func TestRFC1B_SegmentAndRecordAccounting(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		write(t, h, id, 0, pattern(fmt.Sprint("first", k), 4096))
		for i := int64(1); i <= 16; i++ {
			before := f.J.Stats()
			p := pattern(fmt.Sprint("rec", k, i), 4096)
			write(t, h, id, i*4096, p)
			after := f.J.Stats()
			if !sameBut(before.UsedBytes, after.UsedBytes, 0) {
				t.Fatalf("share %d write %d: the journal's UsedBytes moved from %d to %d for a record inside an allocated segment", k, i, before.UsedBytes, after.UsedBytes)
			}
			// The writes are small and the segment allocated, so no record is
			// split at a cap and each is exactly one write record.
			rose := after.Shares[journalv2test.Tag(k)].UsedBytes - before.Shares[journalv2test.Tag(k)].UsedBytes
			if rose != writeRecordBytes(len(p)) {
				t.Fatalf("share %d write %d: the share's UsedBytes rose by %d for a %d-byte write, want its record's %d bytes", k, i, rose, len(p), writeRecordBytes(len(p)))
			}
			if after.ReservedBytes != 0 || after.Shares[journalv2test.Tag(k)].ReservedBytes != 0 {
				t.Fatalf("share %d write %d: ReservedBytes = %d, the share's %d once the write returned, want 0", k, i, after.ReservedBytes, after.Shares[journalv2test.Tag(k)].ReservedBytes)
			}
		}
	}

	// Past the idle-seal size, then idle.
	h, id := f.Share(0), journalv2test.File(0)
	for off := int64(17 * 4096); off < 17*4096+journalv2test.FixtureIdleSealBytes; off += 4096 {
		write(t, h, id, off, pattern(fmt.Sprint("idle", off), 4096))
	}
	syncNow(t, h, id)
	seg := heldEntry(t, f, id).Location.Segment
	// Every open segment holding at least the idle-seal size is sealed by the
	// advance, the other share's included; the tails are summed once all of
	// them are, so none seals after the sum and moves UsedBytes again.
	open := map[journalv2.SegmentID]bool{}
	var due []journalv2.SegmentID
	for _, s := range f.J.Inspect().Segments() {
		open[s.ID] = !s.Sealed
		if !s.Sealed && s.HeldBytes >= journalv2test.FixtureIdleSealBytes {
			due = append(due, s.ID)
		}
	}
	if !slices.Contains(due, seg) {
		t.Fatalf("segment %d, written past the idle-seal size, is not an open segment holding that much", seg)
	}
	used := f.J.Stats().UsedBytes
	f.Advance(2 * journalv2test.IdleSealAge)
	eventually(t, "the idle segments were not sealed with the accounting settled", func() bool {
		for _, id := range due {
			if s, ok := segmentInfo(f, id); !ok || !s.Sealed {
				return false
			}
		}
		_, settled := spareSlack(f)
		return settled
	})
	var tails int64
	for _, s := range f.J.Inspect().Segments() {
		if open[s.ID] && s.Sealed {
			tails += journalv2test.FixtureSegmentCap - s.Allocated
		}
	}
	s, _ := segmentInfo(f, seg)
	fi, err := os.Stat(filepath.Join(f.Dir, segmentName(seg)))
	if err != nil {
		t.Fatal(err)
	}
	if s.Allocated >= journalv2test.FixtureSegmentCap || fi.Size() != s.Allocated {
		t.Fatalf("the idle-sealed segment is accounted at %d and is %d bytes long, want it truncated below the cap %d and accounted at that size", s.Allocated, fi.Size(), journalv2test.FixtureSegmentCap)
	}
	if got := f.J.Stats().UsedBytes; !sameBut(used, got, tails) {
		t.Fatalf("UsedBytes = %d after idle seals returned %d bytes of tail from %d, want it to fall by them", got, tails, used)
	}

	// Sealed full.
	h, id = f.Share(1), journalv2test.File(2)
	write(t, h, id, 0, pattern("full0", 64<<10))
	full := heldEntry(t, f, id).Location.Segment
	for off := int64(64 << 10); ; off += 64 << 10 {
		if s, _ := segmentInfo(f, full); s.Sealed {
			break
		}
		if off > 4<<20 {
			t.Fatalf("segment %d is not sealed after 4 MiB written", full)
		}
		write(t, h, id, off, pattern(fmt.Sprint("full", off), 64<<10))
	}
	s, _ = segmentInfo(f, full)
	fi, err = os.Stat(filepath.Join(f.Dir, segmentName(full)))
	if err != nil {
		t.Fatal(err)
	}
	if s.Allocated != journalv2test.FixtureSegmentCap || fi.Size() != journalv2test.FixtureSegmentCap {
		t.Fatalf("a segment sealed full is accounted at %d and is %d bytes long, want both at the cap %d", s.Allocated, fi.Size(), journalv2test.FixtureSegmentCap)
	}
	wantAccounted(t, f, "after a segment sealed full")
}

// TestRFC1B_FreedMeansFreed checks that storage counts as returned only once
// its unlink completes: releasing a segment's last held extents leaves the
// segment owned, accounted and counted unreclaimed while the unlink is held,
// and a repack whose unlink is held returns only once it completes.
func TestRFC1B_FreedMeansFreed(t *testing.T) {
	f := journalv2test.New(t)

	// A retired segment is unlinked or recycled as a spare, so both are held.
	holdRetire := func(seg journalv2.SegmentID) (*journalv2test.Hold, *journalv2test.Hold) {
		name := segmentName(seg)
		return f.Seam.Hold(t, journalv2test.OpRemove, name), f.Seam.Hold(t, journalv2test.OpRename, name)
	}
	reached := func(what string, a, b *journalv2test.Hold, early <-chan error) {
		t.Helper()
		select {
		case <-a.Reached():
		case <-b.Reached():
		case err := <-early:
			stub(t, "Repack", err)
			t.Fatalf("%s returned (%v) before retiring its segment", what, err)
		case <-time.After(backgroundWait):
			t.Fatalf("%s did not retire its segment within %v", what, backgroundWait)
		}
	}

	h, id := f.Share(0), journalv2test.File(0)
	seg, es := soleSegment(t, f, h, id)
	rm, mv := holdRetire(seg.ID)
	before := f.J.Stats()
	for _, e := range es {
		err := h.Release(t.Context(), id, []journalv2.Extent{e.Extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatal(err)
		}
	}
	syncNow(t, h, id)
	reached("the release of a segment's last extents", rm, mv, nil)
	if _, ok := segmentInfo(f, seg.ID); !ok {
		t.Fatalf("segment %d is no longer owned while its unlink is held", seg.ID)
	}
	if extra, ok := spareSlack(f); !ok {
		t.Fatalf("UsedBytes is %d off the owned segments' sizes plus whole spares while the unlink is held: the held segment left it early", extra)
	}
	if s := f.J.Stats(); s.UnreclaimedBytes < before.UnreclaimedBytes+seg.HeldBytes {
		t.Fatalf("UnreclaimedBytes = %d while the unlink was held, want at least %d with the released segment in it", s.UnreclaimedBytes, before.UnreclaimedBytes+seg.HeldBytes)
	}
	rm.Release()
	mv.Release()
	eventually(t, "the segment was not retired once its unlink was let go", func() bool { return retired(f, seg.ID) })
	wantAccounted(t, f, "after the unlink completed")

	h, id = f.Share(1), journalv2test.File(1)
	seg, es = soleSegment(t, f, h, id)
	for _, e := range es[1:] {
		err := h.Release(t.Context(), id, []journalv2.Extent{e.Extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatal(err)
		}
	}
	syncNow(t, h, id)
	rm, mv = holdRetire(seg.ID)
	repacked := make(chan error, 1)
	var freed int64
	go func() {
		var err error
		freed, err = f.J.Repack(t.Context(), journalv2test.FixtureMaxBytes, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg.ID}})
		repacked <- err
	}()
	reached("a repack", rm, mv, repacked)
	select {
	case err := <-repacked:
		stub(t, "Repack", err)
		t.Fatalf("Repack returned (freed %d, %v) while its segment's unlink was held", freed, err)
	default:
	}
	rm.Release()
	mv.Release()
	var err error
	select {
	case err = <-repacked:
	case <-time.After(backgroundWait):
		t.Fatalf("Repack did not return within %v once its unlink was let go", backgroundWait)
	}
	stub(t, "Repack", err)
	if err != nil {
		t.Fatal(err)
	}
	if freed < seg.Allocated {
		t.Fatalf("Repack freed %d once its unlink completed, want at least the segment's %d", freed, seg.Allocated)
	}
	if !retired(f, seg.ID) {
		t.Fatalf("Repack returned with segment %d still owned", seg.ID)
	}
}

// TestRFC1B_StorageReturned checks that a sealed segment whose every held
// extent is released is retired with no repack and leaves the footprint,
// while a segment released in part stays owned and accounted, its released
// bytes counted unreclaimed, until repack retires it.
func TestRFC1B_StorageReturned(t *testing.T) {
	f := journalv2test.New(t)

	h, id := f.Share(0), journalv2test.File(0)
	seg, es := soleSegment(t, f, h, id)
	for _, e := range es {
		err := h.Release(t.Context(), id, []journalv2.Extent{e.Extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatal(err)
		}
	}
	syncNow(t, h, id)
	eventually(t, "the segment left holding nothing was not retired", func() bool { return retired(f, seg.ID) })
	if slices.Contains(segmentNames(t, f), segmentName(seg.ID)) {
		t.Fatalf("segment %d is still in the directory once the journal retired it", seg.ID)
	}
	wantAccounted(t, f, "after the segment was retired")

	h, id = f.Share(1), journalv2test.File(1)
	seg, es = soleSegment(t, f, h, id)
	// One whole write's record, so what it frees is that record's bytes.
	var e journalv2.Extent
	for _, en := range es {
		if en.Extent.Len == soleChunk && en.Extent.Off%soleChunk == 0 {
			e = en.Extent
			break
		}
	}
	if e.Len == 0 {
		t.Fatalf("no held extent of segment %d is one whole write: %+v", seg.ID, es)
	}
	before := f.J.Stats()
	err := h.Release(t.Context(), id, []journalv2.Extent{e})
	stub(t, "Release", err)
	if err != nil {
		t.Fatal(err)
	}
	syncNow(t, h, id)
	s := f.J.Stats()
	if !sameBut(before.UsedBytes, s.UsedBytes, 0) {
		t.Fatalf("UsedBytes moved from %d to %d on a partial release", before.UsedBytes, s.UsedBytes)
	}
	if grew := s.UnreclaimedBytes - before.UnreclaimedBytes; grew < e.Len || grew > writeRecordBytes(int(e.Len)) {
		t.Fatalf("UnreclaimedBytes grew by %d on the release of a %d-byte write, want its record's bytes, at most %d", grew, e.Len, writeRecordBytes(int(e.Len)))
	}
	if _, ok := segmentInfo(f, seg.ID); !ok {
		t.Fatalf("segment %d, released in part, was retired without a repack", seg.ID)
	}
	if freed := repackAll(t, f, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg.ID}}); freed <= 0 {
		t.Fatal("repack of a segment released in part freed nothing")
	}
	if !retired(f, seg.ID) {
		t.Fatalf("segment %d is still owned after its repack", seg.ID)
	}
	wantAccounted(t, f, "after the repack retired the segment")
}
