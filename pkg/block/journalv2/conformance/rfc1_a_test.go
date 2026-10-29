//go:build journal_conformance

package conformance

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// The checks here fail by serving or destroying the wrong bytes without an
// error.

// sentinel is the byte a check fills a read buffer with, so it can tell the
// bytes a read left untouched from any it wrote. distinct never returns it.
const sentinel = 0xa5

// sentinels returns n sentinel bytes.
func sentinels(n int64) []byte { return bytes.Repeat([]byte{sentinel}, int(n)) }

// distinct returns n bytes of one sequence that repeats every 164 bytes,
// started at a point set by tag, none of them zero or the sentinel. Bytes
// zeroed, or served from another tag's buffer or another offset not a
// multiple of 164 away, never compare equal to them.
func distinct(tag, n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte((i*31+tag*97)%164 + 1)
	}
	return p
}

// ext is the extent of n bytes at off.
func ext(off, n int64) journalv2.Extent { return journalv2.Extent{Off: off, Len: n} }

// overlap returns how many bytes a and b share.
func overlap(a, b journalv2.Extent) int64 {
	return max(0, min(a.Off+a.Len, b.Off+b.Len)-max(a.Off, b.Off))
}

// joined returns es sorted by offset with touching extents joined, so a
// check compares the bytes named, whatever pieces a journal cut them into.
func joined(es []journalv2.Extent) []journalv2.Extent {
	out := slices.SortedFunc(slices.Values(es), func(a, b journalv2.Extent) int { return cmp.Compare(a.Off, b.Off) })
	j := 0
	for i, e := range out {
		if i > 0 && out[j-1].Off+out[j-1].Len == e.Off {
			out[j-1].Len += e.Len
			continue
		}
		out[j] = e
		j++
	}
	return out[:j]
}

// joinedHeld is joined for served extents, joining only those of one
// version.
func joinedHeld(hs []journalv2.Held) []journalv2.Held {
	out := slices.SortedFunc(slices.Values(hs), func(a, b journalv2.Held) int { return cmp.Compare(a.Extent.Off, b.Extent.Off) })
	j := 0
	for i, h := range out {
		if i > 0 && out[j-1].Extent.Off+out[j-1].Extent.Len == h.Extent.Off && out[j-1].Version == h.Version {
			out[j-1].Extent.Len += h.Extent.Len
			continue
		}
		out[j] = h
		j++
	}
	return out[:j]
}

// covered returns how many bytes of e the extents in es cover, counting a
// byte two of them cover once.
func covered(es []journalv2.Extent, e journalv2.Extent) int64 {
	var n, end int64
	for _, x := range slices.SortedFunc(slices.Values(es), func(a, b journalv2.Extent) int { return cmp.Compare(a.Off, b.Off) }) {
		// Only the part past what earlier extents reached is new.
		if from := max(x.Off, end); from < x.Off+x.Len {
			n += overlap(ext(from, x.Off+x.Len-from), e)
		}
		end = max(end, x.Off+x.Len)
	}
	return n
}

// readOut is what one ReadAt returned.
type readOut struct {
	p       []byte
	n       int
	missing []journalv2.Extent
	held    []journalv2.Held
	asOf    journalv2.Version
}

// read reads n bytes at off of file id through h into a buffer of sentinel
// bytes.
func read(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off, n int64) readOut {
	t.Helper()

	r := readOut{p: sentinels(n)}
	var err error
	r.n, r.missing, r.held, r.asOf, err = h.ReadAt(t.Context(), id, off, r.p)
	stub(t, "ReadAt", err)
	if err != nil {
		t.Fatalf("read %d bytes at %d: %v", n, off, err)
	}
	return r
}

// wantMissing fails the test unless r reports exactly want missing and left
// the sentinel in every byte of it, read from off.
func wantMissing(t *testing.T, r readOut, off int64, want []journalv2.Extent, what string) {
	t.Helper()

	if !slices.Equal(joined(r.missing), joined(want)) {
		t.Fatalf("%s: missing = %+v, want exactly %+v", what, r.missing, want)
	}
	for _, e := range want {
		if !bytes.Equal(r.p[e.Off-off:e.Off-off+e.Len], sentinels(e.Len)) {
			t.Fatalf("%s: the bytes of missing %+v were written to", what, e)
		}
	}
}

// wantHeld fails the test unless r served exactly want.
func wantHeld(t *testing.T, r readOut, want []journalv2.Held, what string) {
	t.Helper()

	if !slices.Equal(joinedHeld(r.held), joinedHeld(want)) {
		t.Fatalf("%s: held = %+v, want %+v", what, r.held, want)
	}
}

// wantServed fails the test unless r reports nothing missing and served p.
func wantServed(t *testing.T, r readOut, p []byte, what string) {
	t.Helper()

	if len(r.missing) != 0 {
		t.Fatalf("%s: missing = %+v, want the extent served", what, r.missing)
	}
	if !bytes.Equal(r.p, p) {
		t.Fatalf("%s: served bytes other than the ones written", what)
	}
}

// draw issues a content version from the journal, for a fill.
func draw(t *testing.T, f *journalv2test.Fixture) journalv2.Version {
	t.Helper()

	v, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatalf("draw a version: %v", err)
	}
	return v
}

// syncFile makes every operation on file id that returned so far durable.
func syncFile(t *testing.T, h *journalv2.Handle, id journalv2.FileID) {
	t.Helper()

	err := h.Sync(t.Context(), id)
	stub(t, "Sync", err)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// release releases extents of file id, which must succeed.
func release(t *testing.T, h *journalv2.Handle, id journalv2.FileID, extents ...journalv2.Extent) {
	t.Helper()

	err := h.Release(t.Context(), id, extents)
	stub(t, "Release", err)
	if err != nil {
		t.Fatalf("release %+v: %v", extents, err)
	}
}

// fill fills p at off of file id as content of version v, reading the file's
// change version first as a caller that found the extent missing would.
func fill(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, p []byte, v journalv2.Version) {
	t.Helper()

	asOf := read(t, h, id, off, int64(len(p))).asOf
	err := journalv2test.RetrySpare(func() error { return h.Fill(t.Context(), id, off, p, asOf, v) })
	stub(t, "Fill", err)
	if err != nil {
		t.Fatalf("fill %d bytes at %d: %v", len(p), off, err)
	}
}

// existence records cut as the stamp of every version of file id up to
// through, after syncing the file, as an existence commit does.
func existence(t *testing.T, h *journalv2.Handle, id journalv2.FileID, through journalv2.Version, cut journalv2.SnapshotCut) {
	t.Helper()

	syncFile(t, h, id)
	err := h.Stamp(t.Context(), id, through, cut)
	stub(t, "Stamp", err)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
}

// entries returns the index entries of file id.
func entries(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID) []journalv2.IndexEntry {
	t.Helper()

	es, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect a file: %v", err)
	}
	return es
}

// wantBits fails the test unless e of file id is held whole, every byte of it
// with its offloaded bit as offloaded says.
func wantBits(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID, e journalv2.Extent, offloaded bool, what string) {
	t.Helper()

	var covered int64
	for _, en := range entries(t, f, id) {
		n := overlap(en.Extent, e)
		if en.Kind != journalv2.EntryHeld || n == 0 {
			continue
		}
		if en.Offloaded != offloaded {
			t.Fatalf("%s: held entry %+v has its offloaded bit %v, want %v", what, en, en.Offloaded, offloaded)
		}
		covered += n
	}
	if covered != e.Len {
		t.Fatalf("%s: %d bytes of %+v are held, want all of it", what, covered, e)
	}
}

// wantNotOffloaded fails the test unless err is a NotOffloadedError of file
// id naming exactly want.
func wantNotOffloaded(t *testing.T, err error, id journalv2.FileID, want []journalv2.Extent, what string) {
	t.Helper()

	stub(t, "Release", err)
	var no *journalv2.NotOffloadedError
	if !errors.As(err, &no) {
		t.Fatalf("%s returned %v, want a NotOffloadedError", what, err)
	}
	if no.File != id || !slices.Equal(joined(no.Extents), joined(want)) {
		t.Fatalf("%s was refused for %+v of file %x, want exactly %+v of file %x", what, no.Extents, no.File.Bytes(), want, id.Bytes())
	}
}

// segment returns the accounting of segment id, and whether the journal owns
// it.
func segment(f *journalv2test.Fixture, id journalv2.SegmentID) (journalv2.SegmentInfo, bool) {
	for _, s := range f.J.Inspect().Segments() {
		if s.ID == id {
			return s, true
		}
	}
	return journalv2.SegmentInfo{}, false
}

// listed reports whether the fixture's directory has a file named name.
func listed(t *testing.T, f *journalv2test.Fixture, name string) bool {
	t.Helper()

	names, err := f.Seam.List()
	if err != nil {
		t.Fatalf("list the journal's directory: %v", err)
	}
	return slices.Contains(names, name)
}

// offerFn is an offload callback for one file. An error it returns that wraps
// a callError names the journal call that failed; any other is the check's
// own failure.
type offerFn func(o journalv2.Offer, report func([]journalv2.Extent) error) error

// offload offloads file id through h with fn as the callback, and fails the
// test on an error of the call or of fn.
func offload(t *testing.T, h *journalv2.Handle, id journalv2.FileID, through journalv2.Version, limit, widen int64, fn offerFn) {
	t.Helper()

	offloaded(t, offloadErr(t, h, id, through, limit, widen, fn))
}

// offloadErr is offload returning the call's error rather than failing the
// test, for an offload made on another goroutine.
func offloadErr(t *testing.T, h *journalv2.Handle, id journalv2.FileID, through journalv2.Version, limit, widen int64, fn offerFn) error {
	return h.Offload(t.Context(), id, through, limit, widen, func(_ context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
		return fn(o, report)
	})
}

// offloaded fails the test on err, an offload's error, naming the journal
// call that failed when fn's error wraps a callError.
func offloaded(t *testing.T, err error) {
	t.Helper()

	var ce *callError
	if errors.As(err, &ce) {
		stub(t, ce.call, ce.err)
	}
	stub(t, "Offload", err)
	if err != nil {
		t.Fatalf("offload: %v", err)
	}
}

// coalesce returns offered sorted by offset, with neighbours of the same
// version and stamp joined, so a check compares what was offered whatever
// records it was cut into.
func coalesce(offered []journalv2.OfferedExtent) []journalv2.OfferedExtent {
	out := slices.SortedFunc(slices.Values(offered), func(a, b journalv2.OfferedExtent) int {
		return cmp.Compare(a.Extent.Off, b.Extent.Off)
	})
	j := 0
	for i, o := range out {
		if i > 0 {
			last := &out[j-1]
			if last.Extent.Off+last.Extent.Len == o.Extent.Off && last.Version == o.Version && last.Cut == o.Cut && last.Stamped == o.Stamped {
				last.Extent.Len += o.Extent.Len
				continue
			}
		}
		out[j] = o
		j++
	}
	return out[:j]
}

// extentsOf returns the extents of offered.
func extentsOf(offered []journalv2.OfferedExtent) []journalv2.Extent {
	out := make([]journalv2.Extent, len(offered))
	for i, o := range offered {
		out[i] = o.Extent
	}
	return out
}

// TestRFC1A_NoZeroFill checks that a never-written extent and a released one
// both read as missing, exactly, with the caller's bytes left untouched, and
// that the journal answers the two alike.
func TestRFC1A_NoZeroFill(t *testing.T) {
	f := journalv2test.New(t)

	// File 0, on share 0, never had its middle written; file 1, on share 1,
	// had it written, offloaded and released.
	h0, h1 := f.Share(0), f.Share(1)
	never, gone := journalv2test.File(0), journalv2test.File(1)
	write(t, h0, never, 0, distinct(1, 64<<10))
	write(t, h0, never, 128<<10, distinct(2, 64<<10))
	p := distinct(3, 192<<10)
	v := write(t, h1, gone, 0, p)
	mark(t, h1, gone, ext(0, 192<<10), v)
	release(t, h1, gone, ext(64<<10, 64<<10))

	middle := []journalv2.Extent{ext(64<<10, 64<<10)}
	var outs [2]readOut
	for i, c := range []struct {
		h    *journalv2.Handle
		id   journalv2.FileID
		what string
	}{{h0, never, "a never-written extent"}, {h1, gone, "a released extent"}} {
		outs[i] = read(t, c.h, c.id, 64<<10, 64<<10)
		wantMissing(t, outs[i], 64<<10, middle, c.what)
		if len(outs[i].held) != 0 {
			t.Fatalf("%s: held = %+v, want nothing served", c.what, outs[i].held)
		}

		// Read with its neighbours, the extent is still reported exactly,
		// not widened to the record or segment around it.
		wide := read(t, c.h, c.id, 0, 192<<10)
		wantMissing(t, wide, 0, middle, c.what+" read with its neighbours")
	}
	if outs[0].n != outs[1].n {
		t.Fatalf("a read of a never-written extent returned n = %d, of a released one n = %d: the journal must not tell them apart", outs[0].n, outs[1].n)
	}
}

// TestRFC1A_WriteDuringOffload checks that a write over an offered extent
// while the offer's callback runs keeps its offloaded bit unset when the
// callback reports the extent offloaded, so Release refuses it, while the
// rest of the report is marked.
func TestRFC1A_WriteDuringOffload(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		v := write(t, h, id, 0, distinct(k, 256<<10))
		offload(t, h, id, v, 1<<20, 0, func(_ journalv2.Offer, report func([]journalv2.Extent) error) error {
			if _, err := tryWrite(t, h, id, 64<<10, distinct(10+k, 64<<10)); err != nil {
				return called("WriteAt", err)
			}
			if err := report([]journalv2.Extent{ext(0, 256<<10)}); err != nil {
				return called("report", err)
			}
			return nil
		})

		wantBits(t, f, id, ext(64<<10, 64<<10), false, "the extent written during the offer")
		wantBits(t, f, id, ext(0, 64<<10), true, "the offered extent before the write")
		wantBits(t, f, id, ext(128<<10, 128<<10), true, "the offered extent after the write")
		err := h.Release(t.Context(), id, []journalv2.Extent{ext(64<<10, 64<<10)})
		wantNotOffloaded(t, err, id, []journalv2.Extent{ext(64<<10, 64<<10)}, "a release of the extent written during the offer")
	}
}

// TestRFC1A_OfferStopsAtThrough checks that an offer carries nothing above
// its through version, which stays dirty, and carries each extent at or below
// it with its own version and the cut its existence commit stamped.
func TestRFC1A_OfferStopsAtThrough(t *testing.T) {
	f := journalv2test.New(t)
	const mib = 1 << 20

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		v10 := write(t, h, id, 0, pattern(fmt.Sprint("v10-", k), 4*mib))
		existence(t, h, id, v10, 3)
		v12 := write(t, h, id, mib, pattern(fmt.Sprint("v12-", k), mib))
		existence(t, h, id, v12, 4)
		write(t, h, id, 8*mib, pattern(fmt.Sprint("v13-", k), mib))

		var o journalv2.Offer
		offload(t, h, id, v12, journalv2test.FixtureMaxBytes, 0, func(got journalv2.Offer, _ func([]journalv2.Extent) error) error {
			o = got
			return nil
		})

		want := []journalv2.OfferedExtent{
			{Extent: ext(0, mib), Version: v10, Cut: 3, Stamped: true},
			{Extent: ext(mib, mib), Version: v12, Cut: 4, Stamped: true},
			{Extent: ext(2*mib, 2*mib), Version: v10, Cut: 3, Stamped: true},
		}
		if got := coalesce(o.Dirty); !slices.Equal(got, want) {
			t.Fatalf("share %d: the offer through the second write carried %+v, want %+v", k, got, want)
		}
		above := ext(8*mib, mib)
		for _, d := range slices.Concat(o.Dirty, o.Superseded) {
			if overlap(d.Extent, above) != 0 {
				t.Fatalf("share %d: the offer carried %+v, of the write above through", k, d)
			}
		}
		wantBits(t, f, id, above, false, "the write above through")
	}
}

// TestRFC1A_LostStampIsOffered checks that a version whose stamp was refused
// for want of headroom is offered with Stamped false rather than skipped, and
// that once it is reported and released the headroom recovers.
func TestRFC1A_LostStampIsOffered(t *testing.T) {
	f := journalv2test.New(t)
	ws := fillUntilRefused(t, f, []int{0, 1}, 0, 256<<10)

	// Every version but each file's last is stamped while the headroom is
	// still above half.
	last := map[journalv2.FileID]journalv2.Version{}
	stampedThrough := map[journalv2.FileID]journalv2.Version{}
	for _, w := range ws {
		stampedThrough[w.id] = last[w.id]
		last[w.id] = w.version
	}
	for k := range 2 {
		id := journalv2test.File(k)
		if stampedThrough[id] == (journalv2.Version{}) {
			t.Fatalf("share %d made fewer than two writes before the journal filled", k)
		}
		existence(t, f.Share(k), id, stampedThrough[id], 1)
	}

	// Removals of content the journal does not hold free nothing, and are
	// what draws the headroom down at capacity.
	full := f.J.Stats().HeadroomBytes
	if full == 0 {
		t.Fatal("HeadroomBytes is zero in a journal at capacity")
	}
	drain, h0 := journalv2test.File(50), f.Share(0)
	var drained journalv2.Version
	for i := int64(0); f.J.Stats().HeadroomBytes >= full/2; i++ {
		if i == 1<<16 {
			t.Fatalf("HeadroomBytes = %d after %d removals of nothing held, still not below half of %d", f.J.Stats().HeadroomBytes, i, full)
		}
		v, err := h0.Deallocate(t.Context(), drain, ext(2*i, 1))
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("deallocate nothing held, with the headroom above half: %v", err)
		}
		drained = v
	}

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		syncFile(t, h, id)
		err := h.Stamp(t.Context(), id, last[id], 2)
		stub(t, "Stamp", err)
		wantNoSpace(t, err, journalv2.LimitHeadroom, "a stamp below half the headroom")
	}

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		offeredLast := false
		for pass := 0; ; pass++ {
			if pass == 1000 {
				t.Fatalf("share %d: still offering after %d passes", k, pass)
			}
			var dirty []journalv2.OfferedExtent
			offload(t, h, id, last[id], journalv2test.FixtureMaxBytes, 0, func(o journalv2.Offer, report func([]journalv2.Extent) error) error {
				dirty = o.Dirty
				if len(dirty) == 0 {
					return nil
				}
				if err := report(extentsOf(dirty)); err != nil {
					return called("report", err)
				}
				return nil
			})
			if len(dirty) == 0 {
				break
			}
			for _, d := range dirty {
				switch {
				case d.Version == last[id]:
					if d.Stamped {
						t.Fatalf("share %d: the version whose stamp was refused is offered as %+v, want Stamped false", k, d)
					}
					offeredLast = true
				case !d.Stamped || d.Cut != 1:
					t.Fatalf("share %d: a version stamped at cut 1 is offered as %+v", k, d)
				}
			}
			release(t, h, id, extentsOf(dirty)...)
		}
		if !offeredLast {
			t.Fatalf("share %d: the version whose stamp was refused was never offered", k)
		}
	}

	// Nothing is left holding the journal full: the marker of the draining
	// removals is settled, the segments holding the removal and release
	// records are sealed idle, and repack runs until it frees nothing more.
	//
	// decision: idle sealing runs on the journal's goroutines once the clock
	// passes IdleSealAge, so between repack rounds the check waits up to 2 s
	// of real time for the headroom to come back. Its ceiling: a journal that
	// needs longer than that to seal and retire fails here. Overturned if a
	// segment's sealing becomes visible from the public surface, when the
	// check can wait for it rather than for the headroom.
	h0.Settle(drain, drained)
	for k := range 2 {
		syncFile(t, f.Share(k), journalv2test.File(k))
	}
	f.Advance(journalv2test.IdleSealAge)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		for {
			freed, err := f.J.Repack(t.Context(), 2*journalv2test.FixtureMaxBytes, journalv2.RepackScope{})
			stub(t, "Repack", err)
			if err != nil {
				t.Fatalf("repack: %v", err)
			}
			if freed == 0 {
				break
			}
		}
		got := f.J.Stats().HeadroomBytes
		if got >= full/2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HeadroomBytes = %d once every offered version was released, its segments sealed and repacked, still below half of %d", got, full)
		}
	}
}

// TestRFC1A_FillSafety checks that a write to an extent a fill has found
// absent, made while the fill is held at storage, survives the fill.
//
// The hold stops only the fill's write, so a journal that lets the write run
// beside the fill completes it while the fill waits.
//
// decision: the write is given 200 ms of real time to return before the fill
// is let go. A journal that orders it after the fill blocks it on a lock
// nothing at the seam can see, so only a real-time wait can tell it was
// attempted, and a correct journal spends the 200 ms in full. Its ceiling: a
// journal that lets the write past the fill but takes longer than that to
// complete it passes. Overturned if the journal gains a seam point inside its
// file append domain, which a hold could then catch instead.
func TestRFC1A_FillSafety(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		v := draw(t, f)
		r := read(t, h, id, 0, 64<<10)
		wantMissing(t, r, 0, []journalv2.Extent{ext(0, 64<<10)}, "an extent never written")

		held := f.Seam.HoldFirst(t, journalv2test.OpWrite, "*.seg")
		filled := make(chan error, 1)
		go func() {
			filled <- journalv2test.RetrySpare(func() error { return h.Fill(t.Context(), id, 0, distinct(k, 64<<10), r.asOf, v) })
		}()
		select {
		case <-held.Reached():
		case err := <-filled:
			stub(t, "Fill", err)
			t.Fatalf("the fill returned (%v) without reaching storage", err)
		}

		newer := distinct(10+k, 64<<10)
		var wv journalv2.Version
		wrote := make(chan error, 1)
		go func() {
			var err error
			wv, err = tryWrite(t, h, id, 0, newer)
			wrote <- err
		}()
		var werr error
		returned := false
		select {
		case werr = <-wrote:
			returned = true
		case <-time.After(200 * time.Millisecond):
		}
		held.Release()
		if !returned {
			werr = <-wrote
		}
		ferr := <-filled
		stub(t, "WriteAt", werr)
		if werr != nil {
			t.Fatalf("the write: %v", werr)
		}
		stub(t, "Fill", ferr)
		if ferr != nil && !errors.Is(ferr, journalv2.ErrStaleFill) {
			t.Fatalf("the held fill: %v", ferr)
		}

		r = read(t, h, id, 0, 64<<10)
		wantServed(t, r, newer, "the extent written while the fill was held")
		wantHeld(t, r, []journalv2.Held{{Extent: ext(0, 64<<10), Version: wv}}, "the extent written while the fill was held")
		wantBits(t, f, id, ext(0, 64<<10), false, "the extent written while the fill was held")
		syncFile(t, h, id)
	}

	f.Crash()
	for k := range 2 {
		r := read(t, f.Share(k), journalv2test.File(k), 0, 64<<10)
		wantServed(t, r, distinct(10+k, 64<<10), "the extent written while the fill was held, after a crash")
	}
}

// TestRFC1A_FillSetsTheBit checks that filled content carries its offloaded
// bit, so Release permits it, and that a write over filled content still held
// clears the bit there, so Release refuses exactly the part written.
func TestRFC1A_FillSetsTheBit(t *testing.T) {
	f := journalv2test.New(t)
	head, tail := ext(0, 64<<10), ext(64<<10, 64<<10)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		fill(t, h, id, 0, distinct(k, 128<<10), draw(t, f))
		release(t, h, id, head)

		write(t, h, id, tail.Off, distinct(10+k, int(tail.Len)))
		err := h.Release(t.Context(), id, []journalv2.Extent{tail})
		wantNotOffloaded(t, err, id, []journalv2.Extent{tail}, "a release of a write over filled content")
	}
}

// TestRFC1A_Refusal checks that a release naming an extent whose offloaded
// bit is unset is refused, names exactly the dirty part, and releases and
// appends nothing.
func TestRFC1A_Refusal(t *testing.T) {
	f := journalv2test.New(t)

	// segWrites counts writes into the journal's segments: a spare being
	// made in the background writes to files of its own.
	segWrites := func() (n int) {
		for _, s := range f.J.Inspect().Segments() {
			n += f.Seam.CallsFor(journalv2test.OpWrite, segmentName(s.ID))
		}
		return n
	}

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := distinct(k, 128<<10)
		v := write(t, h, id, 0, p[:64<<10])
		mark(t, h, id, ext(0, 64<<10), v)
		write(t, h, id, 64<<10, p[64<<10:])

		before := fmt.Sprint(entries(t, f, id))
		s, writes := f.J.Stats(), segWrites()
		for _, c := range []struct {
			named, dirty []journalv2.Extent
			what         string
		}{
			{[]journalv2.Extent{ext(0, 64<<10), ext(64<<10, 64<<10)}, []journalv2.Extent{ext(64<<10, 64<<10)}, "a release of an offloaded extent and a dirty one"},
			{[]journalv2.Extent{ext(32<<10, 64<<10)}, []journalv2.Extent{ext(64<<10, 32<<10)}, "a release of an extent half offloaded"},
		} {
			err := h.Release(t.Context(), id, c.named)
			wantNotOffloaded(t, err, id, c.dirty, c.what)
		}

		if after := fmt.Sprint(entries(t, f, id)); after != before {
			t.Fatalf("share %d: refused releases changed the index: %s, then %s", k, before, after)
		}
		wantServed(t, read(t, h, id, 0, 128<<10), p, "the extents named by refused releases")
		got := f.J.Stats()
		if got.Counters.ReleasedBytes != s.Counters.ReleasedBytes || got.UnreclaimedBytes != s.UnreclaimedBytes {
			t.Fatalf("share %d: refused releases moved ReleasedBytes %d to %d and UnreclaimedBytes %d to %d", k,
				s.Counters.ReleasedBytes, got.Counters.ReleasedBytes, s.UnreclaimedBytes, got.UnreclaimedBytes)
		}
		if n := segWrites(); n != writes {
			t.Fatalf("share %d: refused releases wrote to the segments %d times", k, n-writes)
		}
	}
}

// TestRFC1A_PartialReleaseKeepsRecordWhole checks that releasing half of one
// record leaves the other half readable, dirty and undamaged after a crash,
// whether recovery reads the record's segment from its catalog or scans it.
//
// The record is 1 MiB, in segments of recordCap.
func TestRFC1A_PartialReleaseKeepsRecordWhole(t *testing.T) {
	f := journalv2test.NewWithSegmentCap(t, recordCap)
	const size = 1 << 20

	// File 0's record is in a segment sealed before the crash, so recovery can
	// read it from the segment's catalog; file 1's is in a segment still
	// active, which recovery scans.
	h0, sealedID := f.Share(0), journalv2test.File(0)
	v0 := write(t, h0, sealedID, 0, distinct(0, size))
	seg := heldEntry(t, f, sealedID).Location.Segment
	for off := int64(2 << 20); ; off += 64 << 10 {
		if s, _ := segment(f, seg); s.Sealed {
			break
		}
		if off > 4*recordCap {
			t.Fatalf("segment %d is not sealed after %d bytes more of its file", seg, off-2<<20)
		}
		write(t, h0, sealedID, off, pattern(fmt.Sprint("roll", off), 64<<10))
	}
	h1, activeID := f.Share(1), journalv2test.File(1)
	v1 := write(t, h1, activeID, 0, distinct(1, size))
	if s, _ := segment(f, heldEntry(t, f, activeID).Location.Segment); s.Sealed {
		t.Fatal("the record of the scanned leg is in a sealed segment")
	}

	half := ext(0, size/2)
	for _, c := range []struct {
		h  *journalv2.Handle
		id journalv2.FileID
		v  journalv2.Version
	}{{h0, sealedID, v0}, {h1, activeID, v1}} {
		offload(t, c.h, c.id, c.v, size, 0, func(_ journalv2.Offer, report func([]journalv2.Extent) error) error {
			if err := report([]journalv2.Extent{half}); err != nil {
				return called("report", err)
			}
			return nil
		})
		release(t, c.h, c.id, half)
		syncFile(t, c.h, c.id)
	}

	f.Crash()
	s := f.J.Stats()
	if s.Recovery.CatalogSegments == 0 || s.Recovery.ScannedSegments == 0 {
		t.Fatalf("recovery read %d segments from catalogs and scanned %d: the check needs both", s.Recovery.CatalogSegments, s.Recovery.ScannedSegments)
	}
	for k, id := range []journalv2.FileID{sealedID, activeID} {
		r := read(t, f.Share(k), id, 0, size)
		wantMissing(t, r, 0, []journalv2.Extent{half}, "the released half")
		if !bytes.Equal(r.p[size/2:], distinct(k, size)[size/2:]) {
			t.Fatalf("share %d: the half kept reads other bytes than were written", k)
		}
		wantBits(t, f, id, ext(size/2, size/2), false, "the half kept")
	}
	for _, l := range s.Losses {
		if l.File == sealedID || l.File == activeID {
			t.Fatalf("loss event %+v for a record released in part", l)
		}
	}
	if s.Counters.CorruptExtentsDataLoss != 0 || s.Counters.CorruptExtentsRepairable != 0 || s.DamagedSegments != 0 {
		t.Fatalf("corrupt extents %d dirty and %d offloaded, %d damaged segments after a partial release, want none",
			s.Counters.CorruptExtentsDataLoss, s.Counters.CorruptExtentsRepairable, s.DamagedSegments)
	}
}

// TestRFC1A_ReleaseFreesNothingInsideSegment checks that releases of every
// shape leave the segments that stay held untouched: no call on them that
// could free, zero or deallocate storage, and their durable bytes kept as a
// prefix.
//
// The call counts and durable images are the check, since they are what the
// filesystem saw. The segments' Allocated is compared too, but it is the
// journal's own figure, so it catches only a journal that also tells.
func TestRFC1A_ReleaseFreesNothingInsideSegment(t *testing.T) {
	f := journalv2test.New(t)

	// Four records per file; the releases take the middle of one, the head
	// of another, a run across two, the tail of a third, and one whole.
	records := []journalv2.Extent{ext(0, 256<<10), ext(256<<10, 256<<10), ext(512<<10, 256<<10), ext(768<<10, 64<<10)}
	shapes := []journalv2.Extent{ext(64<<10, 64<<10), ext(256<<10, 32<<10), ext(480<<10, 64<<10), ext(736<<10, 32<<10), ext(768<<10, 64<<10)}
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		for i, r := range records {
			v := write(t, h, id, r.Off, distinct(10*k+i, int(r.Len)))
			mark(t, h, id, r, v)
		}
		syncFile(t, h, id)
	}

	// freeing is every call that could free, zero or deallocate storage.
	freeing := []string{journalv2test.OpTruncate, journalv2test.OpAllocate, journalv2test.OpRemove, journalv2test.OpRename}
	type snap struct {
		calls     []int
		durable   []byte
		allocated int64
	}
	take := func(s journalv2.SegmentInfo) snap {
		name := segmentName(s.ID)
		var out snap
		for _, op := range freeing {
			out.calls = append(out.calls, f.Seam.CallsFor(op, name))
		}
		img, err := f.Seam.Durable(name)
		if err != nil {
			t.Fatalf("read segment %d: %v", s.ID, err)
		}
		out.durable, out.allocated = img, s.Allocated
		return out
	}
	before := map[journalv2.SegmentID]snap{}
	for _, s := range f.J.Inspect().Segments() {
		before[s.ID] = take(s)
	}

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		for _, e := range shapes {
			release(t, h, id, e)
		}
		syncFile(t, h, id)
	}

	checked := 0
	for _, s := range f.J.Inspect().Segments() {
		b, ok := before[s.ID]
		if !ok || s.HeldBytes == 0 {
			continue
		}
		checked++
		a := take(s)
		for i, op := range freeing {
			if a.calls[i] != b.calls[i] {
				t.Fatalf("segment %d, still held, took %d %s calls during releases", s.ID, a.calls[i]-b.calls[i], op)
			}
		}
		// The baseline's zero tail is the segment's allocation not yet
		// written, where a release's record may legitimately be appended.
		if !bytes.HasPrefix(a.durable, bytes.TrimRight(b.durable, "\x00")) {
			t.Fatalf("segment %d, still held, had bytes it held before the releases changed", s.ID)
		}
		if a.allocated != b.allocated {
			t.Fatalf("segment %d, still held, is allocated %d bytes after the releases, %d before", s.ID, a.allocated, b.allocated)
		}
	}
	if checked == 0 {
		t.Fatal("no segment that held the released records still holds anything")
	}
}

// TestRFC1A_UnlinkUnderRead checks that a segment left holding nothing by a
// release is neither unlinked nor renamed while a read of it is at storage,
// is retired once the read is done, and that the read returns either the
// bytes written or the extent missing with the caller's bytes untouched.
//
// decision: whether the retirement waits for the read is decided by waiting
// 200 ms of real time with the read held and seeing no remove or rename of
// the segment, since retirement runs on the journal's own goroutines. Its
// ceiling: a journal that retires without waiting for readers, but later than
// 200 ms after the release is durable, passes; the retirement after the read
// is waited for up to 2 s. Overturned if retirement becomes part of the sync
// that makes the release durable, when no wait is needed.
func TestRFC1A_UnlinkUnderRead(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := distinct(k, 3<<20)
		v := write(t, h, id, 0, p)
		mark(t, h, id, ext(0, int64(len(p))), v)
		syncFile(t, h, id)

		// A held entry that is the whole of what its sealed segment holds.
		var e journalv2.IndexEntry
		for _, en := range entries(t, f, id) {
			s, _ := segment(f, en.Location.Segment)
			if en.Kind == journalv2.EntryHeld && s.Sealed && s.HeldBytes == en.Extent.Len && s.RetainedBytes == 0 {
				e = en
				break
			}
		}
		if e.Kind == 0 {
			t.Fatalf("share %d: no sealed segment holds a piece of the 3 MiB write and nothing else", k)
		}

		name := segmentName(e.Location.Segment)
		held := f.Seam.Hold(t, "read*", name)
		removed := f.Seam.Hold(t, journalv2test.OpRemove, name)
		renamed := f.Seam.Hold(t, journalv2test.OpRename, name)
		type result struct {
			r   readOut
			err error
		}
		done := make(chan result, 1)
		go func() {
			r := readOut{p: sentinels(e.Extent.Len)}
			var err error
			r.n, r.missing, r.held, r.asOf, err = h.ReadAt(t.Context(), id, e.Extent.Off, r.p)
			done <- result{r, err}
		}()
		select {
		case <-held.Reached():
		case res := <-done:
			stub(t, "ReadAt", res.err)
			t.Fatalf("share %d: the read returned (%v) without reading its segment", k, res.err)
		}
		release(t, h, id, e.Extent)
		syncFile(t, h, id)
		select {
		case <-removed.Reached():
			t.Fatalf("share %d: segment %d was removed while a read of it was at storage", k, e.Location.Segment)
		case <-renamed.Reached():
			t.Fatalf("share %d: segment %d was renamed while a read of it was at storage", k, e.Location.Segment)
		case <-time.After(200 * time.Millisecond):
		}
		held.Release()

		res := <-done
		stub(t, "ReadAt", res.err)
		if res.err != nil {
			t.Fatalf("share %d: the read under the release: %v", k, res.err)
		}
		if len(res.r.missing) == 0 {
			wantServed(t, res.r, p[e.Extent.Off:e.Extent.Off+e.Extent.Len], "a read under the release of its segment's last extent")
		} else {
			wantMissing(t, res.r, e.Extent.Off, []journalv2.Extent{e.Extent}, "a read under the release of its segment's last extent")
		}

		removed.Release()
		renamed.Release()
		for deadline := time.Now().Add(2 * time.Second); listed(t, f, name); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("share %d: segment %d, left holding nothing, is still in the directory after the read", k, e.Location.Segment)
			}
		}
	}
}

// change is what a check compares of one Change.
type change struct {
	Kind    journalv2.ChangeKind
	Extent  journalv2.Extent
	Version journalv2.Version
}

// since returns what Since yields for file id from from, failing the test
// unless it comes in version order.
func since(t *testing.T, h *journalv2.Handle, id journalv2.FileID, from journalv2.Version) []change {
	t.Helper()

	var out []change
	for c, err := range h.Since(t.Context(), id, from) {
		stub(t, "Since", err)
		if err != nil {
			t.Fatalf("since: %v", err)
		}
		if c.Clone != nil {
			t.Fatalf("since yielded %+v with a clone spec, for a file never cloned", c)
		}
		out = append(out, change{c.Kind, c.Extent, c.Version})
	}
	if !slices.IsSortedFunc(out, func(a, b change) int { return a.Version.Compare(b.Version) }) {
		t.Fatalf("since yielded %+v, not in version order", out)
	}
	return out
}

// joinedChanges returns cs sorted by version, kind and offset, with touching
// extents of one kind and version joined, so a check compares what changed
// whatever pieces a journal cut it into.
func joinedChanges(cs []change) []change {
	out := slices.SortedFunc(slices.Values(cs), func(a, b change) int {
		return cmp.Or(a.Version.Compare(b.Version), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Extent.Off, b.Extent.Off))
	})
	j := 0
	for i, c := range out {
		if i > 0 {
			last := &out[j-1]
			if last.Kind == c.Kind && last.Version == c.Version && last.Extent.Off+last.Extent.Len == c.Extent.Off {
				last.Extent.Len += c.Extent.Len
				continue
			}
		}
		out[j] = c
		j++
	}
	return out[:j]
}

// sameChanges reports whether got and want hold the same changes, whatever
// their order within one version and the pieces they come in.
func sameChanges(got, want []change) bool {
	return slices.Equal(joinedChanges(got), joinedChanges(want))
}

// toEnd is the extent Since reports for a removal from off to the end of the
// file.
func toEnd(off int64) journalv2.Extent { return ext(off, math.MaxInt64-off) }

// TestRFC1A_RemovalsSurviveToSince checks that a truncate, a deallocate and a
// delete come back from a crash as markers Since yields at their versions,
// and nowhere above them; that Settle drops them until the next crash; and
// that the next crash brings them back.
func TestRFC1A_RemovalsSurviveToSince(t *testing.T) {
	f := journalv2test.New(t)

	type removal struct {
		share int
		id    journalv2.FileID
		v     journalv2.Version
		want  []change
	}
	removals := make([]removal, 3)
	for i := range removals {
		k := i % 2
		h, id := f.Share(k), journalv2test.File(i)
		w := write(t, h, id, 0, distinct(i, 256<<10))
		syncFile(t, h, id)
		var v journalv2.Version
		var err error
		var want []change
		switch i {
		case 0:
			v, err = h.Truncate(t.Context(), id, 128<<10)
			stub(t, "Truncate", err)
			want = []change{{journalv2.ChangeWrite, ext(0, 128<<10), w}, {journalv2.ChangeTruncate, toEnd(128 << 10), v}}
		case 1:
			v, err = h.Deallocate(t.Context(), id, ext(64<<10, 64<<10))
			stub(t, "Deallocate", err)
			want = []change{
				{journalv2.ChangeWrite, ext(0, 64<<10), w}, {journalv2.ChangeDeallocate, ext(64<<10, 64<<10), v},
				{journalv2.ChangeWrite, ext(128<<10, 128<<10), w},
			}
		case 2:
			v, err = h.Delete(t.Context(), id)
			stub(t, "Delete", err)
			want = []change{{journalv2.ChangeDelete, toEnd(0), v}}
		}
		if err != nil {
			t.Fatalf("removal %d: %v", i, err)
		}
		removals[i] = removal{k, id, v, want}
	}

	unsettled := func(when string) {
		t.Helper()
		for i, r := range removals {
			h := f.Share(r.share)
			if got := since(t, h, r.id, journalv2.Version{}); !sameChanges(got, r.want) {
				t.Fatalf("%s: Since(file %d, 0) = %+v, want %+v", when, i, got, r.want)
			}
			if got := since(t, h, r.id, r.v); len(got) != 0 {
				t.Fatalf("%s: Since(file %d, its removal's version) = %+v, want nothing", when, i, got)
			}
		}
		if got := f.J.Stats().RemovalMarkers; got != len(removals) {
			t.Fatalf("%s: RemovalMarkers = %d, want %d", when, got, len(removals))
		}
	}

	f.Crash()
	unsettled("after a crash before any Settle")

	for i, r := range removals {
		h := f.Share(r.share)
		h.Settle(r.id, r.v)
		var held []change
		for _, c := range r.want {
			if c.Kind == journalv2.ChangeWrite {
				held = append(held, c)
			}
		}
		if got := since(t, h, r.id, journalv2.Version{}); !sameChanges(got, held) {
			t.Fatalf("settled: Since(file %d, 0) = %+v, want only its held extents %+v", i, got, held)
		}
	}
	if got := f.J.Stats().RemovalMarkers; got != 0 {
		t.Fatalf("RemovalMarkers = %d with every removal settled, want 0", got)
	}

	f.Crash()
	unsettled("after a second crash")
}

// TestRFC1A_NoResurrection checks that a fill of content below a removal's
// version over the removed extent is not held, before or after a reopen,
// while a fill at the removal's version is.
func TestRFC1A_NoResurrection(t *testing.T) {
	f := journalv2test.New(t)
	e := ext(0, 64<<10)

	var removed [2]journalv2.Version
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		old := distinct(k, 64<<10)
		v2 := write(t, h, id, 0, old)
		v3, err := h.Deallocate(t.Context(), id, e)
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("deallocate: %v", err)
		}
		removed[k] = v3

		fill(t, h, id, 0, old, v2)
		wantMissing(t, read(t, h, id, 0, e.Len), 0, []journalv2.Extent{e}, "a fill below the removal's version")
	}
	f.Reopen()
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		wantMissing(t, read(t, h, id, 0, e.Len), 0, []journalv2.Extent{e}, "a fill below the removal's version, after a reopen")
		fill(t, h, id, 0, distinct(10+k, 64<<10), removed[k])
	}

	for i, when := range []string{"a fill at the removal's version", "a fill at the removal's version, after a reopen"} {
		for k := range 2 {
			r := read(t, f.Share(k), journalv2test.File(k), 0, e.Len)
			wantServed(t, r, distinct(10+k, 64<<10), when)
			wantHeld(t, r, []journalv2.Held{{Extent: e, Version: removed[k]}}, when)
		}
		if i == 0 {
			f.Reopen()
		}
	}
}

// TestRFC1A_RemovalDuringOffload checks that a truncate or a delete while an
// offer's callback runs leaves the offered bytes readable through the offer,
// takes effect at once for reads, and makes a report of the removed extent
// mark nothing.
func TestRFC1A_RemovalDuringOffload(t *testing.T) {
	f := journalv2test.New(t)
	e := ext(0, 256<<10)

	for k, removal := range []struct {
		call   string
		remove func(h *journalv2.Handle, id journalv2.FileID) error
	}{
		{"Truncate", func(h *journalv2.Handle, id journalv2.FileID) error {
			_, err := h.Truncate(t.Context(), id, 0)
			return err
		}},
		{"Delete", func(h *journalv2.Handle, id journalv2.FileID) error {
			_, err := h.Delete(t.Context(), id)
			return err
		}},
	} {
		h, id := f.Share(k), journalv2test.File(k)
		p := distinct(k, int(e.Len))
		v := write(t, h, id, 0, p)
		marked := f.J.Stats().Counters.MarkedBytes

		offload(t, h, id, v, 1<<20, 0, func(o journalv2.Offer, report func([]journalv2.Extent) error) error {
			if err := removal.remove(h, id); err != nil {
				return called(removal.call, err)
			}
			buf := sentinels(e.Len)
			if _, err := o.Offered.ReadAt(buf, 0); err != nil {
				return called("Offered.ReadAt", err)
			}
			if !bytes.Equal(buf, p) {
				return errors.New("the offer's reader returned other bytes than were offered, after a removal")
			}
			r := readOut{p: sentinels(e.Len)}
			var err error
			if r.n, r.missing, r.held, r.asOf, err = h.ReadAt(t.Context(), id, 0, r.p); err != nil {
				return called("ReadAt", err)
			}
			if !slices.Equal(joined(r.missing), []journalv2.Extent{e}) || !bytes.Equal(r.p, sentinels(e.Len)) {
				return fmt.Errorf("a read after the removal reported missing %+v, want exactly %+v and the caller's bytes untouched", r.missing, e)
			}
			if err := report([]journalv2.Extent{e}); err != nil {
				return called("report", err)
			}
			return nil
		})

		if got := f.J.Stats().Counters.MarkedBytes; got != marked {
			t.Fatalf("share %d: a report of an extent removed during the offer marked %d bytes", k, got-marked)
		}
		for _, en := range entries(t, f, id) {
			if en.Kind == journalv2.EntryHeld {
				t.Fatalf("share %d: held entry %+v after the extent was removed during the offer", k, en)
			}
		}
	}
}

// TestRFC1A_NeighboursAreFrozen checks that the offloaded neighbours an offer
// carries on request stay readable through the offer as they were when it
// started, while one is overwritten and the other released; that the offer's
// version bounds cover them; and that a report naming them changes no bit.
func TestRFC1A_NeighboursAreFrozen(t *testing.T) {
	f := journalv2test.New(t)
	left, middle, right := ext(0, 64<<10), ext(64<<10, 64<<10), ext(128<<10, 64<<10)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		pl, pr := distinct(10*k, 64<<10), distinct(10*k+1, 64<<10)
		vl := write(t, h, id, left.Off, pl)
		mark(t, h, id, left, vl)
		vr := write(t, h, id, right.Off, pr)
		mark(t, h, id, right, vr)
		vm := write(t, h, id, middle.Off, distinct(10*k+2, 64<<10))

		offered := false
		offload(t, h, id, vm, 1<<20, 64<<10, func(o journalv2.Offer, report func([]journalv2.Extent) error) error {
			offered = true
			want := []journalv2.Held{{Extent: left, Version: vl}, {Extent: right, Version: vr}}
			if got := joinedHeld(o.Neighbours); !slices.Equal(got, want) {
				return fmt.Errorf("the offer carried neighbours %+v, want %+v", o.Neighbours, want)
			}
			if o.Oldest != vl || o.Newest != vm {
				return fmt.Errorf("the offer's versions run from %v to %v, want from the left neighbour's %v to the dirty extent's %v", o.Oldest, o.Newest, vl, vm)
			}

			if _, err := tryWrite(t, h, id, left.Off, distinct(10*k+3, 64<<10)); err != nil {
				return called("WriteAt", err)
			}
			if err := h.Release(t.Context(), id, []journalv2.Extent{right}); err != nil {
				return called("Release", err)
			}
			for _, n := range []struct {
				e journalv2.Extent
				p []byte
			}{{left, pl}, {right, pr}} {
				buf := sentinels(n.e.Len)
				if _, err := o.Offered.ReadAt(buf, n.e.Off); err != nil {
					return called("Offered.ReadAt", err)
				}
				if !bytes.Equal(buf, n.p) {
					return fmt.Errorf("the offer's reader returned other bytes for neighbour %+v than it held when the offer started", n.e)
				}
			}

			before, err := f.J.Inspect().File(id)
			if err != nil {
				return called("Inspect.File", err)
			}
			if err := report([]journalv2.Extent{left, right}); err != nil {
				return called("report", err)
			}
			after, err := f.J.Inspect().File(id)
			if err != nil {
				return called("Inspect.File", err)
			}
			if fmt.Sprint(after) != fmt.Sprint(before) {
				return fmt.Errorf("a report naming the neighbours changed the index: %+v, then %+v", before, after)
			}
			return nil
		})
		if !offered {
			t.Fatalf("share %d: an offload of a file with a dirty extent never called its callback", k)
		}
	}
}

// TestRFC1A_OfferedBytesVerified checks that an offer's reader returns no
// byte of a record whose payload does not verify, even when the damage lies
// past what the offer covers, and that the record is dropped as corrupt and
// never marked.
//
// The record is 1 MiB, in segments of recordCap.
func TestRFC1A_OfferedBytesVerified(t *testing.T) {
	f := journalv2test.NewWithSegmentCap(t, recordCap)
	const size = 1 << 20

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := pattern(fmt.Sprint("verified", k), size)
		v := write(t, h, id, 0, p)
		syncFile(t, h, id)
		corruptAt(t, f, id, p, size-1)

		s := f.J.Stats()
		offload(t, h, id, v, 64<<10, 0, func(o journalv2.Offer, report func([]journalv2.Extent) error) error {
			if len(o.Dirty) == 0 {
				return nil // dropped before it was offered: no byte of it can be returned
			}
			buf := sentinels(64 << 10)
			n, err := o.Offered.ReadAt(buf, 0)
			if errors.Is(err, journalv2.ErrNotImplemented) {
				return called("Offered.ReadAt", err)
			}
			if !errors.Is(err, journalv2.ErrCorrupt) {
				return fmt.Errorf("the offer's reader returned %v over a record that does not verify, want ErrCorrupt", err)
			}
			if n != 0 || !bytes.Equal(buf, sentinels(64<<10)) {
				return fmt.Errorf("the offer's reader returned %d bytes of a record that does not verify", n)
			}
			if err := report(extentsOf(o.Dirty)); errors.Is(err, journalv2.ErrNotImplemented) {
				return called("report", err)
			}
			return nil
		})

		got := f.J.Stats()
		if got.Counters.CorruptExtentsDataLoss == s.Counters.CorruptExtentsDataLoss {
			t.Fatalf("share %d: CorruptExtentsDataLoss did not move after an offer of a dirty record that does not verify", k)
		}
		if got.Counters.MarkedBytes != s.Counters.MarkedBytes {
			t.Fatalf("share %d: %d bytes were marked offloaded from a record that does not verify", k, got.Counters.MarkedBytes-s.Counters.MarkedBytes)
		}
		dropped := false
		for _, l := range got.Losses {
			dropped = dropped || (l.File == id && l.Reason == journalv2.LossCorrupt)
		}
		if !dropped {
			t.Fatalf("share %d: no corrupt loss event for the record that does not verify", k)
		}
		wantMissing(t, read(t, h, id, 0, size), 0, []journalv2.Extent{ext(0, size)}, "a dropped corrupt record")
	}
}

// recordCap is the segment size of the checks that need a 1 MiB record whole
// in one segment, beside another share's: a write larger than what its
// segment has left is split into records, and the fixture's default segments
// are 1 MiB.
const recordCap = 4 << 20

// TestRFC1A_NoPinOnSegmentUnderRepack checks that an offer made while a
// repack pass is part-way through a segment takes nothing from that segment:
// it takes an extent already copied from its new place and leaves the rest,
// and the pass still completes and unlinks the segment.
//
// decision: the pass is parked twice: at its first write into a segment,
// before any copy can be in the index, and at the segment's unlink or rename,
// which a pass makes only after every copy is durable and repointed. The
// second park is where copied extents are checked, so a pass that repoints
// one record at a time is not checked between two of its copies. Overturned
// if repack reports its own points to the seam, when a hold can stop it after
// a named copy instead.
//
// decision: each offer made against the parked pass is given 5 s of real
// time, so a journal whose offer waits on the pass fails rather than hanging
// to the suite's timeout. Its ceiling: an offer slower than that on a loaded
// runner fails without being wrong. Overturned by a journal that documents
// offers waiting on repack, which this check would then have to release.
func TestRFC1A_NoPinOnSegmentUnderRepack(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)

		// The segment holds four dirty extents, one released record so a
		// repack has something to recover, and filled content up to its seal.
		dirty := make([]journalv2.Extent, 4)
		var through journalv2.Version
		for i := range dirty {
			dirty[i] = ext(int64(i)*64<<10, 64<<10)
			through = write(t, h, id, dirty[i].Off, distinct(10*k+i, 64<<10))
		}
		seg := heldEntry(t, f, id).Location.Segment
		gone := ext(1<<20, 256<<10)
		mark(t, h, id, gone, write(t, h, id, gone.Off, distinct(10*k+5, int(gone.Len))))
		release(t, h, id, gone)
		filled := draw(t, f)
		for off := int64(2 << 20); ; off += 64 << 10 {
			if s, _ := segment(f, seg); s.Sealed {
				break
			}
			if off > 8<<20 {
				t.Fatalf("share %d: segment %d is not sealed after %d bytes filled", k, seg, off-2<<20)
			}
			fill(t, h, id, off, pattern(fmt.Sprint("fill", k, off), 64<<10), filled)
		}
		syncFile(t, h, id)
		var inSeg []journalv2.Extent
		for _, en := range entries(t, f, id) {
			if en.Kind == journalv2.EntryHeld && !en.Offloaded && en.Location.Segment == seg {
				inSeg = append(inSeg, en.Extent)
			}
		}
		if len(inSeg) == 0 {
			t.Fatalf("share %d: segment %d holds none of the file's dirty extents", k, seg)
		}

		// againstPass offers the file while the pass is parked, and checks
		// that nothing is taken from the segment under repack and, once a copy
		// is made, that every dirty extent copied out of it is offered.
		againstPass := func(copied bool) {
			t.Helper()
			done := make(chan error, 1)
			go func() {
				done <- offloadErr(t, h, id, through, journalv2test.FixtureMaxBytes, 0, func(o journalv2.Offer, _ func([]journalv2.Extent) error) error {
					now, err := f.J.Inspect().File(id)
					if err != nil {
						return called("Inspect.File", err)
					}
					offered := extentsOf(o.Dirty)
					for _, d := range offered {
						for _, en := range now {
							if en.Kind == journalv2.EntryHeld && overlap(en.Extent, d) != 0 && en.Location.Segment == seg {
								return fmt.Errorf("the offer carried %+v from segment %d, under repack", d, seg)
							}
						}
					}
					if !copied {
						return nil
					}
					var moved, total int64
					for _, e := range inSeg {
						total += e.Len
					}
					for _, en := range now {
						if en.Kind != journalv2.EntryHeld || en.Offloaded || en.Location.Segment == seg {
							continue
						}
						want := covered(inSeg, en.Extent)
						moved += want
						if covered(offered, en.Extent) < want {
							return fmt.Errorf("extent %+v, already copied out of the segment under repack, was not offered whole", en.Extent)
						}
					}
					if moved != total {
						return fmt.Errorf("%d of the segment's %d dirty bytes are indexed elsewhere when it is unlinked, want all of them", moved, total)
					}
					return nil
				})
			}()
			select {
			case err := <-done:
				offloaded(t, err)
			case <-time.After(5 * time.Second):
				t.Fatalf("share %d: an offer made while a repack pass was parked did not return", k)
			}
		}

		first := f.Seam.Hold(t, journalv2test.OpWrite, "*.seg")
		repacked := make(chan error, 1)
		go func() {
			_, err := f.J.Repack(t.Context(), 2*journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
			repacked <- err
		}()
		passDone := func(err error) {
			t.Helper()
			stub(t, "Repack", err)
			if err != nil {
				t.Fatalf("share %d: repack: %v", k, err)
			}
		}
		select {
		case <-first.Reached():
		case err := <-repacked:
			passDone(err)
			t.Fatalf("share %d: a repack of a segment holding dirty extents wrote nothing", k)
		}
		againstPass(false)

		name := segmentName(seg)
		unlink := f.Seam.Hold(t, journalv2test.OpRemove, name)
		rename := f.Seam.Hold(t, journalv2test.OpRename, name)
		first.Release()
		select {
		case <-unlink.Reached():
		case <-rename.Reached():
		case err := <-repacked:
			passDone(err)
			t.Fatalf("share %d: the repack returned without unlinking or renaming segment %d", k, seg)
		}
		againstPass(true)
		unlink.Release()
		rename.Release()
		passDone(<-repacked)
		if listed(t, f, segmentName(seg)) {
			t.Fatalf("share %d: segment %d is still in the directory after its repack completed", k, seg)
		}

		// A later call offers what the earlier ones left behind.
		var offered []journalv2.Extent
		offload(t, h, id, through, journalv2test.FixtureMaxBytes, 0, func(o journalv2.Offer, _ func([]journalv2.Extent) error) error {
			offered = extentsOf(o.Dirty)
			return nil
		})
		for _, e := range dirty {
			if n := covered(offered, e); n != e.Len {
				t.Fatalf("share %d: an offer after the repack covers %d bytes of dirty extent %+v, want all of it", k, n, e)
			}
		}
	}
}
