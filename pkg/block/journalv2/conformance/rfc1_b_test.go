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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// The checks here fail by leaving the journal in a state it cannot leave, or
// by letting it consume without bound.

// The tags of the shares the checks here attach with a limit of their own,
// clear of the fixture's shares.
var (
	limitedA = journalv2test.Tag(4)
	limitedB = journalv2test.Tag(5)
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

// limitedShare attaches tag with limit through the journal itself, since the
// fixture's shares carry none, and closes the handle when the test ends.
func limitedShare(t *testing.T, f *journalv2test.Fixture, tag journalv2.ShareTag, limit int64) *journalv2.Handle {
	t.Helper()

	h, err := f.J.Share(tag, limit, journalv2.Version{}, journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach share %d with a limit of %d: %v", tag, limit, err)
	}
	t.Cleanup(func() { _ = h.Close() }) // ErrClosed once the test closed it
	return h
}

// refusal fails the test unless err, returned by call, is a NoSpaceError
// naming limit and counting a positive shortfall, in unit unless unit is
// zero, and returns it.
func refusal(t *testing.T, err error, call string, limit journalv2.Limit, unit journalv2.Unit) *journalv2.NoSpaceError {
	t.Helper()

	stub(t, call, err)
	wantNoSpace(t, err, limit, call)
	var ns *journalv2.NoSpaceError
	errors.As(err, &ns)
	if (unit != 0 && ns.Unit != unit) || ns.Missing <= 0 {
		t.Fatalf("%s was refused with %d %v missing, want a positive count of %v", call, ns.Missing, ns.Unit, unit)
	}
	return ns
}

// fillShare writes chunks of size bytes through h, round robin over ids, until
// the share's limit refuses one, and returns every write made. share is
// recorded in each write so a check can tell whose it was.
func fillShare(t *testing.T, h *journalv2.Handle, share int, ids []journalv2.FileID, size int64) []written {
	t.Helper()

	var out []written
	offs := make([]int64, len(ids))
	for i := range 2 * journalv2test.FixtureMaxBytes / size {
		k := int(i) % len(ids)
		v, err := tryWrite(t, h, ids[k], offs[k], pattern(fmt.Sprint("share", share, i), int(size)))
		stub(t, "WriteAt", err)
		if err != nil {
			refusal(t, err, "a write past the share's limit", journalv2.LimitShare, journalv2.UnitBytes)
			return out
		}
		out = append(out, written{share, ids[k], journalv2.Extent{Off: offs[k], Len: size}, v})
		offs[k] += size
	}
	t.Fatalf("%d bytes written into a share were never refused by its limit", 2*journalv2test.FixtureMaxBytes)
	panic("unreachable")
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
	panic("unreachable")
}

// segmentCalls returns, per op and segment file now in the journal's
// directory, how many calls the seam has counted.
func segmentCalls(t *testing.T, f *journalv2test.Fixture) map[string]int {
	t.Helper()

	names, err := f.Seam.List()
	if err != nil {
		t.Fatalf("list the journal's directory: %v", err)
	}
	ops := []string{
		journalv2test.OpOpen, journalv2test.OpAllocate, journalv2test.OpWrite, journalv2test.OpRead,
		journalv2test.OpReadDirect, journalv2test.OpSync, journalv2test.OpTruncate, journalv2test.OpClose,
		journalv2test.OpRename, journalv2test.OpRemove,
	}
	out := map[string]int{}
	for _, name := range names {
		if !strings.HasSuffix(name, ".seg") {
			continue
		}
		for _, op := range ops {
			out[op+" "+name] = f.Seam.CallsFor(op, name)
		}
	}
	return out
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

// soleSegment writes 3 MiB of file id through h in 64 KiB writes, marks it
// offloaded and syncs it, and returns a sealed segment holding nothing but
// held extents of the file, with those extents: the middle of a run three
// segments long.
func soleSegment(t *testing.T, f *journalv2test.Fixture, h *journalv2.Handle, id journalv2.FileID) (journalv2.SegmentInfo, []journalv2.IndexEntry) {
	t.Helper()

	const chunk = 64 << 10
	for off := int64(0); off < 3<<20; off += chunk {
		v := write(t, h, id, off, pattern(fmt.Sprint("sole", off), chunk))
		mark(t, h, id, journalv2.Extent{Off: off, Len: chunk}, v)
	}
	syncNow(t, h, id)

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
	panic("unreachable")
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

// TestRFC1B_ReserveAhead checks that a reservation holds what it took: its
// writes succeed after another writer filled the share to its limit, while a
// plain write and a further reservation are refused naming that limit, and
// Close returns what it left unused; that reserved writes beside a stream
// another writer keeps rolling are never refused; and that a restart drops a
// reservation.
//
// The copy is 8 MiB in 1 MiB writes beside 32 MiB of other writes, not 1 GiB
// beside 10 GiB: the fixture's journal holds 64 MiB.
func TestRFC1B_ReserveAhead(t *testing.T) {
	f := journalv2test.New(t)
	a := limitedShare(t, f, limitedA, 4<<20)

	const piece = 128 << 10
	r, err := a.Reserve(1<<20, 4)
	stub(t, "Reserve", err)
	if err != nil {
		t.Fatalf("reserve 1 MiB for four writes: %v", err)
	}
	ws := fillShare(t, a, 4, []journalv2.FileID{journalv2test.File(40), journalv2test.File(41)}, 256<<10)
	for i := range 4 {
		id := journalv2test.File(42 + i%2)
		_, err := r.WriteAt(t.Context(), id, int64(i/2)*piece, pattern(fmt.Sprint("reserved", i), piece), 0)
		stub(t, "Reservation.WriteAt", err)
		if err != nil {
			t.Fatalf("reserved write %d, within the reservation, with the share at its limit: %v", i, err)
		}
	}
	_, err = a.WriteAt(t.Context(), journalv2test.File(40), 1<<30, pattern("plain", 4096), 0)
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
	write(t, a, last.id, last.extent.Off+last.extent.Len, pattern("returned", 64<<10))

	// Reserved writes beside a writer rolling the other share's streams.
	b := f.Share(1)
	const big = 1 << 20
	rb, err := b.Reserve(8*(big+4096), 8)
	stub(t, "Reserve", err)
	if err != nil {
		t.Fatalf("reserve 8 MiB for eight writes: %v", err)
	}
	others := make(chan error, 1)
	go func() {
		for i := range int64(32) {
			if _, err := tryWrite(t, f.Share(0), journalv2test.File(50+int(i%2)), i/2*big, pattern(fmt.Sprint("other", i), big)); err != nil {
				others <- called("WriteAt", err)
				return
			}
		}
		others <- nil
	}()
	for i := range 8 {
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
	_, err = b.Reserve(1<<20, 1)
	stub(t, "Reserve", err)
	if err != nil {
		t.Fatalf("reserve before the restart: %v", err)
	}
	if got := f.J.Stats().ReservedBytes; got <= 0 {
		t.Fatalf("ReservedBytes = %d with a reservation open, want it counted", got)
	}
	_ = a.Close()
	f.Reopen()
	s := f.J.Stats()
	if s.ReservedBytes != 0 || s.Shares[journalv2test.Tag(1)].ReservedBytes != 0 {
		t.Fatalf("ReservedBytes = %d, the share's %d after a restart, want both 0: a restart drops a reservation", s.ReservedBytes, s.Shares[journalv2test.Tag(1)].ReservedBytes)
	}
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
		var returned atomic.Bool
		wrote := make(chan error, 1)
		go func() {
			_, err := h.WriteAt(t.Context(), id, 0, p, 0)
			returned.Store(true)
			wrote <- err
		}()
		select {
		case <-held.Reached():
		case err := <-wrote:
			stub(t, "WriteAt", err)
			t.Fatalf("share %d: the write returned (%v) without reaching storage", k, err)
		}

		var writeFirst atomic.Bool
		closed := make(chan error, 1)
		go func() {
			err := h.Close()
			writeFirst.Store(returned.Load())
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
		if !writeFirst.Load() {
			t.Fatalf("share %d: Close returned before the write in flight did", k)
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
	// release evicts writes from the front of ws until missing bytes are out.
	release := func(h *journalv2.Handle, ws []written, missing int64) {
		t.Helper()
		var n int64
		for i := 0; n < missing; i++ {
			if i == len(ws) {
				t.Fatalf("released all %d bytes written, short of the %d missing", n, missing)
			}
			n += evict(t, h, ws[i:i+1])
		}
	}

	// The share's limit.
	a := limitedShare(t, f, limitedA, 8<<20)
	ids := []journalv2.FileID{journalv2test.File(0), journalv2test.File(1)}
	ws := fillShare(t, a, 4, ids, 256<<10)
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
	release(a, ws, ns.Missing)
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
	ns = refusal(t, err, "a write past the journal's maximum", journalv2.LimitJournal, journalv2.UnitBytes)
	var n int64
	for _, w := range jws {
		if n >= ns.Missing {
			break
		}
		n += evict(t, f.Share(w.share), []written{w})
	}
	if n < ns.Missing {
		t.Fatalf("released all %d bytes written, short of the %d missing", n, ns.Missing)
	}
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

	// Two extents in three released, so no segment is left holding nothing
	// and retired on its own: only repack returns this space.
	for i, w := range ws {
		if i%3 != 0 {
			evict(t, f.Share(w.share), []written{w})
		}
	}
	used := f.J.Stats().UsedBytes
	freed := repackAll(t, f, journalv2.RepackScope{})
	if freed <= 0 {
		t.Fatal("repack at capacity, over segments two thirds released, freed nothing")
	}
	if got := f.J.Stats().UsedBytes; got >= used {
		t.Fatalf("UsedBytes = %d after repack freed %d, %d before: want it lower", got, freed, used)
	}
	write(t, f.Share(0), journalv2test.File(0), 1<<30, pattern("after-repack", 128<<10))
}

// TestRFC1B_RepackBesideShareAtLimit checks that repack copies the records of
// a share at its limit out of segments it shares with another share's
// released content, refusing no copy by that limit, and returns the other
// share's unreclaimed storage.
func TestRFC1B_RepackBesideShareAtLimit(t *testing.T) {
	f := journalv2test.New(t)
	a, b := limitedShare(t, f, limitedA, 8<<20), f.Share(1)

	const size = 128 << 10
	var aws, bws []written
	for i := int64(0); ; i++ {
		id, off := journalv2test.File(int(i%2)), i/2*size
		v, err := tryWrite(t, a, id, off, pattern(fmt.Sprint("a", i), size))
		stub(t, "WriteAt", err)
		if err != nil {
			refusal(t, err, "a write past share A's limit", journalv2.LimitShare, journalv2.UnitBytes)
			break
		}
		aws = append(aws, written{4, id, journalv2.Extent{Off: off, Len: size}, v})
		bid := journalv2test.File(2 + int(i%2))
		bws = append(bws, written{1, bid, journalv2.Extent{Off: off, Len: size}, write(t, b, bid, off, pattern(fmt.Sprint("b", i), size))})
	}
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
	mixed := 0
	for _, ts := range tags {
		if ts[limitedA] && ts[journalv2test.Tag(1)] {
			mixed++
		}
	}
	if mixed == 0 {
		t.Fatal("no segment holds records of both shares, written interleaved")
	}

	evict(t, b, bws)
	syncNow(t, a, journalv2test.File(0), journalv2test.File(1))
	syncNow(t, b, journalv2test.File(2), journalv2test.File(3))
	s := f.J.Stats()
	moved, aHeld, bUnreclaimed := s.Counters.RepackedBytes, s.Shares[limitedA].HeldBytes, s.Shares[journalv2test.Tag(1)].UnreclaimedBytes
	if bUnreclaimed <= 0 {
		t.Fatalf("share B's UnreclaimedBytes = %d with its content released beside A's, want it counted", bUnreclaimed)
	}

	repackAll(t, f, journalv2.RepackScope{})
	s = f.J.Stats()
	if s.Counters.RepackedBytes == moved {
		t.Fatal("repack copied none of share A's records out of the segments B's releases left")
	}
	if got := s.Shares[limitedA].HeldBytes; got != aHeld {
		t.Fatalf("share A holds %d bytes after the repack, %d before: a copy was dropped", got, aHeld)
	}
	if got := s.Shares[journalv2test.Tag(1)].UnreclaimedBytes; got >= bUnreclaimed {
		t.Fatalf("share B's UnreclaimedBytes = %d after the repack, %d before: want it returned", got, bUnreclaimed)
	}
}

// TestRFC1B_HeadroomIsAllocated checks that once the filesystem allocates
// nothing more, content writes are refused while releases, removals and
// marks still succeed, drawing on headroom allocated ahead, and a repack
// still returns space.
//
// decision: the fixture fails new allocations with EIO, not ENOSPC, as the
// one storage error it injects. What the check asserts holds for either: a
// record that needed an allocation would fail. Overturned once the fixture
// can fail a call with ENOSPC, which the content-write refusal should then
// name.
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

	f.FailNext(journalv2test.OpAllocate, "*", 0)

	refused := false
	for i := range int64(2 * journalv2test.FixtureMaxBytes / (256 << 10)) {
		_, err := tryWrite(t, f.Share(1), journalv2test.File(5), i*256<<10, pattern(fmt.Sprint("new", i), 256<<10))
		stub(t, "WriteAt", err)
		if err != nil {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("content writes went on succeeding with every new allocation failing")
	}

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
			t.Fatalf("share %d: truncate with allocations failing: %v", k, err)
		}
		_, err = h.Delete(t.Context(), journalv2test.File(2+k))
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("share %d: delete with allocations failing: %v", k, err)
		}
		_, err = h.Deallocate(t.Context(), kept[k][0].id, kept[k][0].extent)
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("share %d: deallocate with allocations failing: %v", k, err)
		}
	}
	mark(t, f.Share(0), journalv2test.File(4), journalv2.Extent{Off: 0, Len: 64 << 10}, unmarked)

	if freed := repackAll(t, f, journalv2.RepackScope{}); freed <= 0 {
		t.Fatal("repack over segments two thirds released freed nothing with allocations failing")
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
	full := f.J.Stats().HeadroomBytes
	if full <= 0 {
		t.Fatalf("HeadroomBytes = %d after open, want the headroom allocated", full)
	}

	// One-byte writes a byte apart never merge into one extent.
	const extents = 150_000
	var through journalv2.Version
	for i := range int64(extents) {
		through = write(t, h, id, 2*i, []byte{byte(i)})
	}
	syncNow(t, h, id)
	ws := fillUntilRefused(t, f, []int{1}, 10, 256<<10)

	// Removals of nothing held, each a record drawn on the headroom, until
	// less than half of it remains.
	other := f.Share(1)
	for i := int64(0); f.J.Stats().HeadroomBytes >= full/2; i++ {
		if i == 8192 {
			t.Fatalf("HeadroomBytes = %d of %d after %d removals at the limit, want it below half", f.J.Stats().HeadroomBytes, full, i)
		}
		_, err := other.Deallocate(t.Context(), journalv2test.File(11), journalv2.Extent{Off: 1<<40 + i*4096, Len: 4096})
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("a removal at the limit with the headroom above half: %v", err)
		}
	}

	draws, writes := f.J.Stats().Counters.HeadroomDraws, f.Seam.Calls(journalv2test.OpWrite)
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
	if got := f.Seam.Calls(journalv2test.OpWrite); got != writes {
		t.Fatalf("the offload below half the headroom made %d writes, want none: offloaded records are not appended", got-writes)
	}
	if got := f.J.Stats().Counters.HeadroomDraws; got != draws {
		t.Fatalf("HeadroomDraws moved by %d over the offload, want 0", got-draws)
	}
	es, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatal(err)
	}
	var marked int64
	for _, e := range es {
		if e.Kind == journalv2.EntryHeld {
			if !e.Offloaded {
				t.Fatalf("extent %+v is held dirty after the file was offloaded", e.Extent)
			}
			marked += e.Extent.Len
		}
	}
	if marked != extents {
		t.Fatalf("%d bytes of the file are held after the offload, want %d", marked, extents)
	}
	if got := f.J.Stats().Shares[journalv2test.Tag(0)].DirtyBytes; got != 0 {
		t.Fatalf("share 0's DirtyBytes = %d after its one file was offloaded, want 0, so all of it is evictable", got)
	}

	err = h.Hold(t.Context(), journalv2.ShareID{1}, 1, map[journalv2.FileID]journalv2.Version{id: through})
	refusal(t, err, "a hold below half the headroom", journalv2.LimitHeadroom, 0)
	err = h.Stamp(t.Context(), id, through, 1)
	refusal(t, err, "a stamp below half the headroom", journalv2.LimitHeadroom, 0)
	evict(t, other, ws[:2])
	_, err = other.Truncate(t.Context(), journalv2test.File(10), 0)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("a truncate below half the headroom: %v", err)
	}

	f.Crash()
	es, err = f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatal(err)
	}
	var dirty int64
	for _, e := range es {
		if e.Kind == journalv2.EntryHeld && !e.Offloaded {
			dirty += e.Extent.Len
		}
	}
	if dirty != extents {
		t.Fatalf("%d bytes of the file are dirty after a crash, want all %d: its marks were not persisted", dirty, extents)
	}
}

// TestRFC1B_RecordsWithoutBytesAtTheLimit checks that at the journal's
// maximum a truncate, a deallocate, a delete, a mark, a release and an idle
// seal with its footer all succeed, none refused for space, and that each is
// counted as a draw on the headroom.
//
// The leg that fails a content write's new segment with ENOSPC is not here:
// the fixture cannot inject ENOSPC.
func TestRFC1B_RecordsWithoutBytesAtTheLimit(t *testing.T) {
	f := journalv2test.New(t)
	for k := range 2 {
		write(t, f.Share(k), journalv2test.File(10+k), 0, pattern(fmt.Sprint("pre", k), 64<<10))
		syncNow(t, f.Share(k), journalv2test.File(10+k))
	}
	write(t, f.Share(0), journalv2test.File(12), 0, pattern("deleted", 64<<10))
	ws := fillUntilRefused(t, f, []int{0, 1}, 0, 256<<10)

	s := f.J.Stats()
	draws, sealed := s.Counters.HeadroomDraws, s.Sealed
	h0, h1 := f.Share(0), f.Share(1)
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
	eventually(t, "no idle segment was sealed at the journal's maximum", func() bool { return f.J.Stats().Sealed > sealed })
	for _, seg := range f.J.Inspect().Segments() {
		if seg.Failed {
			t.Fatalf("segment %d failed an append at the journal's maximum", seg.ID)
		}
	}
	// Five records and at least one seal.
	if got := f.J.Stats().Counters.HeadroomDraws - draws; got < 6 {
		t.Fatalf("HeadroomDraws moved by %d over five records and a seal at the limit, want at least 6", got)
	}
}

// TestRFC1B_NoVersionRepeats checks that a version a crash lost is not issued
// again, that writes and draws are refused naming the version limit once it
// is reached and resume once it is raised.
//
// The leg that opens a journal with no version limit is not here: the fixture
// always sets one.
func TestRFC1B_NoVersionRepeats(t *testing.T) {
	f := journalv2test.New(t)

	// The write is never synced, so the crash loses it.
	f.Drop(journalv2test.OpSync, "*.seg")
	a := write(t, f.Share(0), journalv2test.File(0), 0, pattern("lost", 4096))
	f.Seam.SetFault(nil)
	f.Crash()

	_, missing, _, _, err := f.Share(0).ReadAt(t.Context(), journalv2test.File(0), 0, make([]byte, 4096))
	stub(t, "ReadAt", err)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) == 0 {
		t.Fatal("the unsynced write is held after the crash, so the check cannot tell a repeated version")
	}
	if v := write(t, f.Share(1), journalv2test.File(1), 0, pattern("after", 4096)); v.Compare(a) <= 0 {
		t.Fatalf("the first write after the crash has version %+v, not above the lost write's %+v", v, a)
	}
	v, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatal(err)
	}
	if v.Compare(a) <= 0 {
		t.Fatalf("the first draw after the crash is %+v, not above the lost write's %+v", v, a)
	}

	f.J.SetVersionLimit(journalv2.Version{Counter: v.Counter + 1})
	_, err = f.Share(0).WriteAt(t.Context(), journalv2test.File(0), 0, pattern("limit", 4096), 0)
	refusal(t, err, "a write at the version limit", journalv2.LimitVersion, journalv2.UnitVersions)
	_, err = f.J.Draw()
	refusal(t, err, "a draw at the version limit", journalv2.LimitVersion, journalv2.UnitVersions)

	f.J.SetVersionLimit(journalv2.Version{Counter: v.Counter + 1000})
	if w := write(t, f.Share(0), journalv2test.File(0), 0, pattern("raised", 4096)); w.Compare(v) <= 0 {
		t.Fatalf("a write once the limit was raised has version %+v, not above %+v", w, v)
	}
	if _, err := f.J.Draw(); err != nil {
		stub(t, "Draw", err)
		t.Fatalf("a draw once the limit was raised: %v", err)
	}
}

// TestRFC1B_FloorBuckets checks that deleting many other files does not make
// a fill of a file that still holds content stale, while deleting and
// recreating the file itself does.
func TestRFC1B_FloorBuckets(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	const gap = 64 << 10

	// Each file holds a first extent, so it keeps its own entry, and is read
	// missing past it.
	asOf := func(i int) (journalv2.Version, journalv2.Version) {
		t.Helper()
		id := journalv2test.File(i)
		v := write(t, h, id, 0, pattern(fmt.Sprint("held", i), 4096))
		_, missing, _, as, err := h.ReadAt(t.Context(), id, gap, make([]byte, 4096))
		stub(t, "ReadAt", err)
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 1 {
			t.Fatalf("the read past the held extent reports missing %+v, want it all", missing)
		}
		return v, as
	}
	x, y := journalv2test.File(0), journalv2test.File(1)
	xv, xAsOf := asOf(0)
	yv, yAsOf := asOf(1)

	other := f.Share(1)
	for i := range 10_000 {
		id := journalv2test.File(100 + i)
		write(t, other, id, 0, []byte{byte(i)})
		v, err := other.Delete(t.Context(), id)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("delete file %d: %v", i, err)
		}
		other.Settle(id, v)
	}
	err := h.Fill(t.Context(), x, gap, pattern("fill-x", 4096), xAsOf, xv)
	stub(t, "Fill", err)
	if err != nil {
		t.Fatalf("a fill of a file that holds content, after deletes of 10^4 others: %v", err)
	}

	d, err := h.Delete(t.Context(), y)
	stub(t, "Delete", err)
	if err != nil {
		t.Fatal(err)
	}
	h.Settle(y, d)
	write(t, h, y, 0, pattern("recreated", 4096))
	err = h.Fill(t.Context(), y, gap, pattern("fill-y", 4096), yAsOf, yv)
	stub(t, "Fill", err)
	if !errors.Is(err, journalv2.ErrStaleFill) {
		t.Fatalf("a fill of a file deleted and recreated since its read returned %v, want ErrStaleFill", err)
	}
	_, missing, _, _, err := h.ReadAt(t.Context(), y, gap, make([]byte, 4096))
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
			if after.UsedBytes != before.UsedBytes {
				t.Fatalf("share %d write %d: the journal's UsedBytes moved from %d to %d for a record inside an allocated segment", k, i, before.UsedBytes, after.UsedBytes)
			}
			rose := after.Shares[journalv2test.Tag(k)].UsedBytes - before.Shares[journalv2test.Tag(k)].UsedBytes
			if rose < int64(len(p)) || rose > writeRecordBytes(len(p)) {
				t.Fatalf("share %d write %d: the share's UsedBytes rose by %d for a %d-byte write, want its record's bytes, at most %d", k, i, rose, len(p), writeRecordBytes(len(p)))
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
	open := map[journalv2.SegmentID]bool{}
	for _, s := range f.J.Inspect().Segments() {
		open[s.ID] = !s.Sealed
	}
	used := f.J.Stats().UsedBytes
	f.Advance(2 * journalv2test.IdleSealAge)
	eventually(t, "the idle segment was not sealed", func() bool {
		s, ok := segmentInfo(f, seg)
		return ok && s.Sealed
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
	if got := f.J.Stats().UsedBytes; got != used-tails {
		t.Fatalf("UsedBytes = %d after idle seals returned %d bytes of tail from %d, want %d", got, tails, used, used-tails)
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
}

// TestRFC1B_FreedMeansFreed checks that storage counts as returned only once
// its unlink completes: releasing a segment's last held extents leaves
// UsedBytes and UnreclaimedBytes covering it while the unlink is held, and a
// repack whose unlink is held returns only once it completes.
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
	s := f.J.Stats()
	if s.UsedBytes != before.UsedBytes {
		t.Fatalf("UsedBytes moved from %d to %d while the segment's unlink was held", before.UsedBytes, s.UsedBytes)
	}
	if s.UnreclaimedBytes < before.UnreclaimedBytes+seg.HeldBytes {
		t.Fatalf("UnreclaimedBytes = %d while the unlink was held, want at least %d with the released segment in it", s.UnreclaimedBytes, before.UnreclaimedBytes+seg.HeldBytes)
	}
	rm.Release()
	mv.Release()
	eventually(t, "UsedBytes did not fall once the unlink completed", func() bool { return f.J.Stats().UsedBytes < before.UsedBytes })

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
}

// TestRFC1B_StorageReturned checks that a sealed segment whose every held
// extent is released is unlinked with no repack and its size leaves the
// footprint, while a segment released in part stays in the footprint, its
// released bytes counted unreclaimed, until repack retires it.
func TestRFC1B_StorageReturned(t *testing.T) {
	f := journalv2test.New(t)

	h, id := f.Share(0), journalv2test.File(0)
	seg, es := soleSegment(t, f, h, id)
	used := f.J.Stats().UsedBytes
	for _, e := range es {
		err := h.Release(t.Context(), id, []journalv2.Extent{e.Extent})
		stub(t, "Release", err)
		if err != nil {
			t.Fatal(err)
		}
	}
	syncNow(t, h, id)
	eventually(t, "the segment left holding nothing was not retired", func() bool {
		_, ok := segmentInfo(f, seg.ID)
		return !ok
	})
	names, err := f.Seam.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == segmentName(seg.ID) {
			t.Fatalf("segment %d is still in the directory once the journal stopped listing it", seg.ID)
		}
	}
	if got := f.J.Stats().UsedBytes; got != used-seg.Allocated {
		t.Fatalf("UsedBytes = %d after the segment was unlinked, %d before: want it to fall by its %d bytes", got, used, seg.Allocated)
	}

	h, id = f.Share(1), journalv2test.File(1)
	seg, es = soleSegment(t, f, h, id)
	before := f.J.Stats()
	e := es[0].Extent
	err = h.Release(t.Context(), id, []journalv2.Extent{e})
	stub(t, "Release", err)
	if err != nil {
		t.Fatal(err)
	}
	syncNow(t, h, id)
	s := f.J.Stats()
	if s.UsedBytes != before.UsedBytes {
		t.Fatalf("UsedBytes moved from %d to %d on a partial release", before.UsedBytes, s.UsedBytes)
	}
	if grew := s.UnreclaimedBytes - before.UnreclaimedBytes; grew < e.Len || grew > writeRecordBytes(int(e.Len)) {
		t.Fatalf("UnreclaimedBytes grew by %d on the release of %d bytes, want the released record's bytes", grew, e.Len)
	}
	if _, ok := segmentInfo(f, seg.ID); !ok {
		t.Fatalf("segment %d, released in part, was retired without a repack", seg.ID)
	}
	if freed := repackAll(t, f, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg.ID}}); freed <= 0 {
		t.Fatal("repack of a segment released in part freed nothing")
	}
	if _, ok := segmentInfo(f, seg.ID); ok {
		t.Fatalf("segment %d is still owned after its repack", seg.ID)
	}
	if got := f.J.Stats().UsedBytes; got >= before.UsedBytes {
		t.Fatalf("UsedBytes = %d after the repack retired the segment, %d before: want it lower", got, before.UsedBytes)
	}
}
