//go:build journal_conformance

package conformance

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// The checks here inject damage systematically rather than by example: a
// crash at every sync point, a cut at every byte of a torn write, every
// subset of reordered writes, a flip of every bit of every region, and the
// edits an operator can make from outside.

// driven is a fixture with the reference model mirrored on every call made
// through it, so the journal can be compared with the model after a crash.
// Its label names the pass of a sweep in every failure.
type driven struct {
	t     *testing.T
	label string
	f     *journalv2test.Fixture
	m     *model.Model
}

// newDriven returns a driven fixture on a new journal, watched by the
// profiler, labelled label in its failures; the test's own t, or an
// iteration's, is what the fixture and the watch are registered on.
func newDriven(tb testing.TB, t *testing.T, label string) *driven {
	tb.Helper()
	return &driven{t, label, journalv2test.New(tb), model.New()}
}

// wrote is what a write returned.
type wrote struct {
	v   journalv2.Version
	err error
}

// mirrorWrite applies a write's result to the model.
func (d *driven) mirrorWrite(s, i int, off int64, p []byte, w wrote) {
	d.t.Helper()
	if err := d.m.WriteAt(journalv2test.Tag(s), journalv2test.File(i), off, p, 0, w.v, w.err); err != nil {
		d.t.Fatalf("%s: model: %v", d.label, err)
	}
}

// write writes p at off of file i through share s, mirrored, and returns
// the record.
func (d *driven) write(s, i int, off int64, p []byte) rec {
	d.t.Helper()

	h, id := d.f.Share(s), journalv2test.File(i)
	var w wrote
	w.err = journalv2test.RetrySpare(func() error {
		w.v, w.err = h.WriteAt(d.t.Context(), id, off, p, 0)
		return w.err
	})

	stub(d.t, "WriteAt", w.err)
	d.mirrorWrite(s, i, off, p, w)
	if w.err != nil {
		d.t.Fatalf("%s: write %d bytes at %d: %v", d.label, len(p), off, w.err)
	}

	return rec{s, id, journalv2.Extent{Off: off, Len: int64(len(p))}, w.v, p}
}

// mirrorSync applies a sync's result to the model.
func (d *driven) mirrorSync(s int, ids []journalv2.FileID, err error) {
	d.t.Helper()
	if merr := d.m.Sync(journalv2test.Tag(s), ids, err); merr != nil {
		d.t.Fatalf("%s: model: %v", d.label, merr)
	}
}

// sync syncs ids through share s, mirrored.
func (d *driven) sync(s int, ids ...journalv2.FileID) {
	d.t.Helper()

	err := d.f.Share(s).Sync(d.t.Context(), ids...)
	stub(d.t, "Sync", err)
	d.mirrorSync(s, ids, err)
	if err != nil {
		d.t.Fatalf("%s: sync: %v", d.label, err)
	}
}

// writeSynced writes and syncs one record of size bytes at off of file i
// through share s.
func (d *driven) writeSynced(s, i int, off int64, size int) rec {
	d.t.Helper()
	r := d.write(s, i, off, pattern(fmtTag("d", s, i, off), size))
	d.sync(s, r.id)
	return r
}

// fill writes records of size bytes on file i through share s from offset 0,
// each synced, until one lands in a new segment, and returns the records of
// the first segment, the record that rolled, unsynced, and the first
// segment's id.
func (d *driven) fill(s, i int, size int) ([]rec, rec, journalv2.SegmentID) {
	d.t.Helper()

	id := journalv2test.File(i)
	var recs []rec
	var first journalv2.SegmentID
	for n := 0; n <= 2*journalv2test.FixtureSegmentCap/size; n++ {
		off := int64(n) * int64(size)
		r := d.write(s, i, off, pattern(fmtTag("f", s, i, off), size))
		seg := entryAt(d.t, d.f, id, off).Location.Segment
		if n == 0 {
			first = seg
		}

		if seg != first {
			return recs, r, first
		}

		d.sync(s, id)
		recs = append(recs, r)
	}

	d.t.Fatalf("%s: %d records of %d bytes never rolled a segment of %d", d.label, 2*journalv2test.FixtureSegmentCap/size, size, journalv2test.FixtureSegmentCap)
	return nil, rec{}, 0
}

// reopened opens the journal again on what the crash left, failing with the
// pass's label.
func (d *driven) reopened() {
	d.t.Helper()
	if err := d.f.TryCrash(); err != nil {
		d.t.Fatalf("%s: the open after the crash: %v", d.label, err)
	}
}

// crashed applies a crash to the model once the journal is open again on
// what the crash left, and compares the journal with the model for both
// shares: everything a returned sync covered reads its bytes, and anything
// else is held record by record, whole or not at all.
func (d *driven) crashed() {
	d.t.Helper()

	if err := d.m.Crash(); err != nil {
		d.t.Fatalf("%s: model: %v", d.label, err)
	}

	if err := d.m.Reopen(d.f.Floor()); err != nil {
		d.t.Fatalf("%s: model: %v", d.label, err)
	}

	for s := range 2 {
		view := model.HandleView{Handle: d.f.Share(s), Journal: d.f.J}
		if err := d.m.Compare(d.t.Context(), journalv2test.Tag(s), view); err != nil {
			d.t.Fatalf("%s: after the crash, share %d differs from the model: %v", d.label, s, err)
		}
	}
}

// holdNext installs a hold on the next call matching op and key and only
// then releases cur, so the call parked at cur passes and the one after it
// parks: a call sees the holds in place when it arrives, never one installed
// while it waits.
func holdNext(t *testing.T, f *journalv2test.Fixture, op, key string, cur *journalv2test.Hold) *journalv2test.Hold {
	t.Helper()

	next := f.Seam.Hold(t, op, key)
	cur.Release()
	return next
}

// magicAt reports whether the live bytes of the segment file name carry
// magic at off, or at the file's last 32 bytes when off is negative.
func magicAt(t *testing.T, f *journalv2test.Fixture, name string, off int64, magic string) bool {
	t.Helper()

	b := liveBytes(t, f, name)
	if off < 0 {
		off = int64(len(b)) - raw.TrailerSize
	}

	return off >= 0 && off+4 <= int64(len(b)) && string(b[off:off+4]) == magic
}

// parked is an operation stopped at a hold: the hold, the channel its result
// comes back on, a precondition that the parked call is the sync point meant,
// and what to apply to the model from the calls that returned before it.
type parked struct {
	held   *journalv2test.Hold
	done   <-chan error
	check  func() string
	mirror func()
}

// crashParked crashes the storage with the parked call in flight, releases
// it, opens the journal again on what the crash left and compares it with
// the model. The call the crash took never returned to the process that
// made it, so the model is told nothing of it: whatever it wrote is unsynced
// and lost with the rest.
func crashParked(t *testing.T, d *driven, p parked) {
	t.Helper()

	if msg := p.check(); msg != "" {
		t.Fatalf("precondition: the held call is not the sync point meant: %s", msg)
	}

	if err := d.f.Seam.Crash(); err != nil {
		t.Fatalf("crash: %v", err)
	}

	p.held.Release()
	<-p.done
	if p.mirror != nil {
		p.mirror()
	}

	d.f.Seam.SetFault(nil)
	d.reopened()
	d.crashed()
}

// waitParked waits until the operation reaches its hold, failing the test
// if it returns first; an operation that reached it already is not waited
// for.
func waitParked(t *testing.T, p parked, what string) {
	t.Helper()

	select {
	case <-p.held.Reached():
		return
	default:
	}

	select {
	case <-p.held.Reached():
	case err := <-p.done:
		stub(t, what, err)
		t.Fatalf("%s returned (%v) without reaching its hold", what, err)
	}
}

// rollParked writes 64 KiB records on file 0 through share 0, each synced,
// with a hold on the first segment's sync installed before each write, until
// the write that rolls parks a sync of the first segment at the hold. It
// returns the parked write and the first segment's id. A seal that runs
// after the write returned is waited for by its sync too; the write is then
// mirrored already, and a write still parked is not, since it never returns
// to the process the crash ends.
func rollParked(t *testing.T, d *driven) (parked, journalv2.SegmentID) {
	t.Helper()

	const size = 64 << 10
	id := journalv2test.File(0)
	d.writeSynced(0, 0, 0, size)
	seg := entryAt(t, d.f, id, 0).Location.Segment
	name := segmentName(seg)

	for n := 1; n <= 2*journalv2test.FixtureSegmentCap/size; n++ {
		off := int64(n) * size
		p := pattern(fmtTag("f", 0, 0, off), size)

		held := d.f.Seam.Hold(t, journalv2test.OpSync, name)
		result := make(chan wrote, 1)
		done := make(chan error, 1)

		go func() {
			var w wrote
			w.err = journalv2test.RetrySpare(func() error {
				w.v, w.err = d.f.Share(0).WriteAt(t.Context(), id, off, p, 0)
				return w.err
			})
			result <- w
			done <- w.err
		}()

		rolled := func() bool {
			w := <-result
			d.mirrorWrite(0, 0, off, p, w)
			stub(t, "WriteAt", w.err)
			if w.err != nil {
				t.Fatalf("write at %d: %v", off, w.err)
			}
			return entryAt(t, d.f, id, off).Location.Segment != seg
		}

		select {
		case <-held.Reached():
			return parked{held: held, done: done}, seg
		case <-done:
			if rolled() {
				// The seal runs after the write returned: its sync is still
				// to come.
				<-held.Reached()
				return parked{held: held, done: done}, seg
			}

			held.Release()
			d.sync(0, id)
		}
	}

	t.Fatalf("records of %d bytes never rolled a segment of %d", size, journalv2test.FixtureSegmentCap)
	return parked{}, 0
}

// maxSealSyncs bounds the syncs of the sealed segment a seal is given to
// show its marker or its trailer in the live bytes: the records before the
// marker, the marker, the footer and the trailer, each synced on its own at
// most.
const maxSealSyncs = 4

// advanceUntil moves the parked sync forward, one sync of the segment file
// name at a time, until cond holds of the live bytes, failing as a
// precondition when maxSealSyncs syncs did not make it hold.
func advanceUntil(t *testing.T, d *driven, p *parked, name string, cond func() bool, what string) {
	t.Helper()

	for n := 0; !cond(); n++ {
		if n == maxSealSyncs {
			t.Fatalf("precondition: %s after %d syncs of %s", what, n, name)
		}

		p.held = holdNext(t, d.f, journalv2test.OpSync, name, p.held)
		waitParked(t, *p, "the roll")
	}
}

// indexedEnd returns the end of the last record the index places in segment
// seg: its offset plus its header, its time and its payload.
func indexedEnd(t *testing.T, where string, f *journalv2test.Fixture, seg journalv2.SegmentID) int64 {
	t.Helper()

	var end int64
	for _, entries := range f.J.Inspect().All() {
		for _, e := range entries {
			if e.Kind == journalv2.EntryHeld && e.Location.Segment == seg {
				end = max(end, e.Location.Offset+raw.RecordHeaderSize+8+e.Extent.Len)
			}
		}
	}

	if end == 0 {
		t.Fatalf("%s: the index places no record in segment %d", where, seg)
	}

	return end
}

// sealMarkerAfter reports whether the live bytes of the segment file name
// carry a seal marker of segment seg at an 8-byte boundary at or after from:
// its magic, its segment id, and its offset field naming its own position.
func sealMarkerAfter(t *testing.T, f *journalv2test.Fixture, name string, seg journalv2.SegmentID, from int64) bool {
	t.Helper()

	b := liveBytes(t, f, name)
	for off := (from + 7) &^ 7; off+raw.SealMarkerSize <= int64(len(b)); off += 8 {
		m := b[off : off+raw.SealMarkerSize]
		if string(m[:4]) == "DJS1" && binary.LittleEndian.Uint64(m[8:16]) == uint64(seg) && int64(binary.LittleEndian.Uint64(m[16:24])) == off {
			return true
		}
	}

	return false
}

// footerParked drives a fresh journal to the sync that makes a sealed
// segment's footer durable and parks it, returning the parked write that
// rolled and the sealed segment's name.
func footerParked(t *testing.T, d *driven) (parked, string) {
	t.Helper()

	d.writeSynced(1, 1, 0, 64<<10)
	p, seg := rollParked(t, d)
	name := segmentName(seg)
	trailed := func() bool { return magicAt(t, d.f, name, -1, "DJT1") }
	advanceUntil(t, d, &p, name, trailed, "no trailer at the end of the live bytes")

	p.check = func() string {
		if !trailed() {
			return "the sealed segment does not end with a trailer"
		}
		return ""
	}

	return p, name
}

// recycleCrashed drives a fresh journal to a recycle of file 0's first
// segment, with spare-making parked for the rest of the check so the pool is
// empty and a retired segment is what refills it, and crashes the storage
// with the recycle's zeroing sync parked: the rename is durable, the zeroing
// of the two slots is not. It returns the recycled segment's name.
func recycleCrashed(t *testing.T, d *driven) string {
	t.Helper()

	const size = 64 << 10
	d.f.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	recs, _, seg := d.fill(0, 0, size)
	others, _, _ := d.fill(1, 1, size)
	d.sync(0, recs[0].id)
	d.sync(1, others[0].id)

	// Superseded: the segment keeps one live record, which the repack copies
	// before it retires the segment.
	for _, r := range recs[:len(recs)-1] {
		d.write(0, 0, r.extent.Off, pattern(fmtTag("o", 0, 0, r.extent.Off), size))
	}

	d.sync(0, recs[0].id)
	name := segmentName(seg)

	files := segmentFiles(t, d.f)
	rename := d.f.Seam.Hold(t, journalv2test.OpRename, name)
	done := make(chan error, 1)
	go func() {
		_, err := d.f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
		done <- err
	}()
	p := parked{held: rename, done: done}
	waitParked(t, p, "the repack")

	// The next sync of a segment file after the rename is the zeroing's.
	p.held = holdNext(t, d.f, journalv2test.OpSync, "*.seg", p.held)
	waitParked(t, p, "the repack")
	r := retirement(t, d.f, name, files)
	if !r.recycled {
		t.Fatalf("precondition: the segment was unlinked with the pool empty, want it recycled")
	}

	if msg := zeroSlots(t, d, r.name)(); msg != "" {
		t.Fatalf("precondition: %s", msg)
	}

	if err := d.f.Seam.Crash(); err != nil {
		t.Fatalf("crash: %v", err)
	}

	p.held.Release()
	<-done // the crash took the repack

	if !slices.Contains(segmentFiles(t, d.f), r.name) {
		t.Fatalf("precondition: %s did not survive the crash: its rename's directory sync did not precede the zeroing sync", r.name)
	}
	return r.name
}

// zeroSlots returns the precondition that the live bytes of the segment file
// name carry zeros in both its header slot and its trailer slot, as they do
// at the sync of a zeroing and at no sync of a seal or a take.
func zeroSlots(t *testing.T, d *driven, name string) func() string {
	return func() string {
		t.Helper()

		b := liveBytes(t, d.f, name)
		if len(b) < raw.SegmentHeaderSize+raw.TrailerSize {
			return fmt.Sprintf("%s is %d bytes in the live view, too short for a header slot and a trailer slot", name, len(b))
		}

		if !zero(b[:raw.SegmentHeaderSize]) || !zero(b[len(b)-raw.TrailerSize:]) {
			return "the slots of " + name + " are not both zeros in the live bytes, so the held sync is not the zeroing's"
		}
		return ""
	}
}

// takeParked drives file 0's stream into the re-adopted segment name with
// writes of file 2 made on a goroutine, until one parks at zero, the hold on
// the segment's sync. The writes that returned before it are applied to the
// model on the test's goroutine once the parked one is released; the parked
// one, which the crash takes, is not.
func takeParked(t *testing.T, d *driven, zero *journalv2test.Hold, name string) parked {
	t.Helper()

	const size = 64 << 10
	id := journalv2test.File(2)
	results := make(chan []wrote, 1)
	done := make(chan error, 1)

	go func() {
		var ws []wrote
		for n := range 2 * journalv2test.FixtureSegmentCap / size {
			off := int64(n) * size
			v, err := d.f.Share(0).WriteAt(t.Context(), id, off, pattern(fmtTag("z", 0, 2, off), size), 0)
			ws = append(ws, wrote{v, err})
			if err != nil {
				break
			}
		}
		results <- ws
		done <- ws[len(ws)-1].err
	}()

	mirror := func() {
		for n, w := range <-results {
			if w.err != nil {
				return
			}

			off := int64(n) * size
			d.mirrorWrite(0, 2, off, pattern(fmtTag("z", 0, 2, off), size), w)
		}
	}

	return parked{held: zero, done: done, check: zeroSlots(t, d, name), mirror: mirror}
}

// syncPoints are the sync points of RFC 28 section 4.4, each a function that
// drives a fresh journal to the operation whose sync it names and parks that
// sync at a hold.
//
// decision: the directory sync of a format replace is no point here. The
// format file check crashes a restamp before the rename of the new file and
// before the directory sync after it, which are that operation's sync points.
var syncPoints = []struct {
	name string
	run  func(t *testing.T, d *driven) parked
}{
	{"write_at", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		d.writeSynced(0, 0, 0, 64<<10)
		w := d.write(0, 0, 64<<10, pattern("w", 64<<10))
		name := segmentName(entryAt(t, d.f, w.id, w.extent.Off).Location.Segment)
		held := d.f.Seam.Hold(t, journalv2test.OpSync, name)
		done := make(chan error, 1)
		go func() { done <- d.f.Share(0).Sync(t.Context(), w.id) }()
		return parked{held: held, done: done}
	}},
	{"seal", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		p, seg := rollParked(t, d)
		name := segmentName(seg)
		// The rolling write is split at the cap and its first piece sits in
		// the sealed segment after the last synced record, so the marker is
		// looked for from the end of the last record the index places there.
		sealed := func() bool { return sealMarkerAfter(t, d.f, name, seg, indexedEnd(t, "seal", d.f, seg)) }
		advanceUntil(t, d, &p, name, sealed, "no seal marker in the live bytes")
		p.check = func() string {
			if !sealed() {
				return "no seal marker after the last record of the sealed segment"
			}
			return ""
		}
		return p
	}},
	{"footer", func(t *testing.T, d *driven) parked {
		p, _ := footerParked(t, d)
		return p
	}},
	{"repack_copy", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		recs, _, seg := d.fill(0, 0, 64<<10)
		d.sync(0, recs[0].id)
		d.writeSynced(0, 0, 0, 64<<10) // the first record superseded: the segment has dead bytes
		name := segmentName(seg)
		held := d.f.Seam.Hold(t, journalv2test.OpSync, "*.seg")
		done := make(chan error, 1)
		go func() {
			_, err := d.f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
			done <- err
		}()
		return parked{held: held, done: done, check: func() string {
			if d.f.Seam.CallsFor(journalv2test.OpRename, name) != 0 || d.f.Seam.CallsFor(journalv2test.OpRemove, name) != 0 {
				return "the segment was retired before its first sync inside the repack"
			}
			return ""
		}}
	}},
	{"repack_unlink", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		var recs []rec
		var seg journalv2.SegmentID
		awaitSpare(t, d.f, func() { recs, _, seg = d.fill(0, 0, 64<<10) })
		d.sync(0, recs[0].id)
		d.f.Reopen() // the pool is at its target as the open counts it
		d.writeSynced(0, 0, 0, 64<<10)
		name := segmentName(seg)
		removes := d.f.Seam.CallsFor(journalv2test.OpRemove, name)
		held := d.f.Seam.Hold(t, journalv2test.OpSyncDir, "")
		done := make(chan error, 1)
		go func() {
			_, err := d.f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
			done <- err
		}()
		return parked{held: held, done: done, check: func() string {
			if d.f.Seam.CallsFor(journalv2test.OpRemove, name) != removes+1 {
				return "the held directory sync does not follow the segment's unlink"
			}
			return ""
		}}
	}},
	{"spare_entry", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		// A take's refill is awaited and the journal reopened, so the pool is
		// at its target as the open counts it and no spare-making of the
		// open's own is in flight: the next directory sync is a refill's.
		awaitSpare(t, d.f, func() { d.writeSynced(0, 2, 0, journalv2test.FixtureSegmentCap) })
		d.f.Reopen()
		before := len(segmentFiles(t, d.f))
		held := d.f.Seam.Hold(t, journalv2test.OpSyncDir, "")
		recs, _, _ := d.fill(0, 0, 64<<10)
		d.sync(0, recs[0].id)
		// The roll returned already; the spare's directory sync, off the
		// write path, is what parks.
		<-held.Reached()
		done := make(chan error, 1)
		done <- nil
		return parked{held: held, done: done, check: func() string {
			if got := len(segmentFiles(t, d.f)); got != before+1 {
				return "the directory did not gain the one segment file a spare's rename adds"
			}
			return ""
		}}
	}},
	{"open_unproven_tail", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		a := d.write(0, 0, 0, pattern("a", 64<<10))
		d.write(0, 0, 64<<10, pattern("b", 64<<10))
		name := segmentName(entryAt(t, d.f, a.id, 0).Location.Segment)
		held := d.f.Seam.Hold(t, journalv2test.OpSync, name)
		done := make(chan error, 1)
		go func() { done <- d.f.TryAbandon() }()
		return parked{held: held, done: done}
	}},
	{"open_torn_tail_truncation", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		a := d.writeSynced(0, 0, 0, 64<<10)
		d.write(0, 0, 64<<10, pattern("b", 64<<10))
		name := segmentName(entryAt(t, d.f, a.id, 0).Location.Segment)
		truncates := d.f.Seam.CallsFor(journalv2test.OpTruncate, name)
		held := d.f.Seam.Hold(t, journalv2test.OpSync, name)
		done := make(chan error, 1)
		go func() { done <- d.f.TryCrash(journalv2test.TornIn(name, 32<<10)) }()
		return parked{held: held, done: done, check: func() string {
			if d.f.Seam.CallsFor(journalv2test.OpTruncate, name) != truncates+1 {
				return "the held sync does not follow the truncation of the torn tail"
			}
			return ""
		}}
	}},
	{"open_seal_after_truncation", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		a := d.writeSynced(0, 0, 0, 64<<10)
		d.write(0, 0, 64<<10, pattern("b", 64<<10))
		loc := entryAt(t, d.f, a.id, 0).Location
		name := segmentName(loc.Segment)
		end := loc.Offset + raw.RecordHeaderSize + 8 + a.extent.Len
		held := d.f.Seam.Hold(t, journalv2test.OpSync, name)
		done := make(chan error, 1)
		go func() { done <- d.f.TryCrash(journalv2test.TornIn(name, 32<<10)) }()
		p := parked{held: held, done: done}
		waitParked(t, p, "the open after a torn crash")
		// A journal that syncs the truncation and the seal together is at the
		// seal's sync already; one that syncs them apart is at the
		// truncation's, and the next sync of the segment is the seal's.
		if !magicAt(t, d.f, name, end, "DJS1") {
			p.held = holdNext(t, d.f, journalv2test.OpSync, name, p.held)
		}

		p.check = func() string {
			if !magicAt(t, d.f, name, end, "DJS1") {
				return "no seal marker after the last record that verified"
			}
			return ""
		}
		return p
	}},
	{"open_reappend", func(t *testing.T, d *driven) parked {
		d.writeSynced(1, 1, 0, 64<<10)
		a := d.write(0, 0, 0, pattern("a", 64<<10))
		name := segmentName(entryAt(t, d.f, a.id, 0).Location.Segment)
		deviceSync(t, d.f, name)
		d.write(0, 0, 64<<10, pattern("b", 64<<10))
		d.f.Drop(journalv2test.OpSync, name)
		syncs := d.f.Seam.CallsFor(journalv2test.OpSync, name)
		held := d.f.Seam.Hold(t, journalv2test.OpSync, "*.seg")
		done := make(chan error, 1)
		go func() { done <- d.f.TryAbandon() }()
		p := parked{held: held, done: done}
		waitParked(t, p, "the open after an abandon")
		if d.f.Seam.CallsFor(journalv2test.OpSync, name) != syncs+1 {
			t.Fatalf("precondition: the open's first segment sync is not the unproven segment's own")
		}
		// The first sync is the segment's own, which the fault drops; the
		// next is the re-append's, on the segment it went to, and the failed
		// segment is never synced again.
		p.held = holdNext(t, d.f, journalv2test.OpSync, "*.seg", p.held)
		p.check = func() string {
			if d.f.Seam.CallsFor(journalv2test.OpSync, name) != syncs+1 {
				return "the unproven segment was synced again after its own sync, so the held sync is not the re-append's"
			}
			return ""
		}
		return p
	}},
	{"open_spare_zeroing", func(t *testing.T, d *driven) parked {
		name := recycleCrashed(t, d)

		// The open after the crash re-adopts the segment and zeroes both its
		// slots, or leaves that to the take, in which case the stream is
		// driven into it.
		zero := d.f.Seam.Hold(t, journalv2test.OpSync, name)
		opened := make(chan error, 1)
		go func() { opened <- d.f.TryCrash() }()
		select {
		case <-zero.Reached():
			return parked{held: zero, done: opened, check: zeroSlots(t, d, name)}
		case err := <-opened:
			stub(t, "Open", err)
			if err != nil {
				t.Fatalf("the open after the recycle's crash: %v", err)
			}
		}

		return takeParked(t, d, zero, name)
	}},
}

// TestDamage_LostWrites checks that a crash dropping every write since the
// last sync, at each sync point of each operation that syncs, the open's own
// included, leaves a journal the reference model allows: everything a
// returned sync covered reads its bytes, anything else is held record by
// record, whole or not at all.
func TestDamage_LostWrites(t *testing.T) {
	for _, sp := range syncPoints {
		t.Run(sp.name, func(t *testing.T) {
			d := newDriven(t, t, sp.name)
			p := sp.run(t, d)
			if p.check == nil {
				p.check = func() string { return "" }
			}

			waitParked(t, p, sp.name)
			crashParked(t, d, p)
		})
	}
}

// iteration is the testing.TB of one pass of a loop inside a test: its
// cleanups run when the pass ends rather than when the test does, so a loop
// over many fixtures holds one at a time, and its temporary directories go
// with it. Failures reach the test.
type iteration struct {
	*testing.T
	cleanups []func()
}

// Cleanup registers f to run when the pass ends, after the functions
// registered after it.
func (it *iteration) Cleanup(f func()) { it.cleanups = append(it.cleanups, f) }

// TempDir returns a new directory that is removed when the pass ends, where
// the test's own would stay until the test does.
func (it *iteration) TempDir() string {
	it.Helper()

	dir, err := os.MkdirTemp("", "journal-iteration-")
	if err != nil {
		it.Fatalf("make a temporary directory: %v", err)
	}

	it.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// iterate runs body as one pass of a loop, with the cleanups it registers
// run when it returns.
func iterate(t *testing.T, body func(tb testing.TB)) {
	t.Helper()

	it := &iteration{T: t}
	defer func() {
		for i := len(it.cleanups) - 1; i >= 0; i-- {
			it.cleanups[i]()
		}
	}()
	body(it)
}

// locate returns where record r is held, failing with where the sweep
// stands if no held extent covers its start.
func locate(t *testing.T, where string, f *journalv2test.Fixture, r rec) journalv2.RecordRef {
	t.Helper()

	entries, err := f.J.Inspect().File(r.id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("%s: inspect a file: %v", where, err)
	}

	for _, e := range entries {
		if e.Kind == journalv2.EntryHeld && e.Extent.Off <= r.extent.Off && r.extent.Off < e.Extent.Off+e.Extent.Len {
			return e.Location
		}
	}

	t.Fatalf("%s: no held extent of the file covers offset %d; entries: %+v", where, r.extent.Off, entries)
	return journalv2.RecordRef{}
}

// segmentOf returns the accounting of segment seg, failing with where the
// sweep stands if the journal does not own it.
func segmentOf(t *testing.T, where string, f *journalv2test.Fixture, seg journalv2.SegmentID) journalv2.SegmentInfo {
	t.Helper()

	for _, info := range f.J.Inspect().Segments() {
		if info.ID == seg {
			return info
		}
	}

	t.Fatalf("%s: segment %d is not among the journal's", where, seg)
	return journalv2.SegmentInfo{}
}

// TestDamage_TornWrites checks that a torn write, cut at every byte, leaves
// a journal the reference model allows: a record cut anywhere is never held,
// and a trailer cut anywhere is no footer. Two sweeps: the final record,
// a 64-byte write on file 0 unsynced last, cut at each of its bytes by a
// crash; and the trailer, the last write of a seal parked at its sync, cut
// at each of its bytes from the end, the segment then read by scan where the
// cut of nothing left it read by its catalog.
//
// decision: the record sweep counts its cuts over one write of the record's
// header, time and payload, 168 bytes. A journal that writes a record in
// more than one call is refused a cut past its last write by the seam, which
// the sweep reports rather than silently cutting less.
func TestDamage_TornWrites(t *testing.T) {
	t.Run("final_record", func(t *testing.T) {
		const size = 64
		const recordBytes = raw.RecordHeaderSize + 8 + size
		for n := 0; n <= recordBytes; n++ {
			iterate(t, func(tb testing.TB) {
				d := newDriven(tb, t, fmt.Sprintf("the final record cut at byte %d", n))
				d.writeSynced(1, 1, 0, 64<<10)
				a := d.writeSynced(0, 0, 0, 64<<10)
				name := segmentName(locate(t, d.label, d.f, a).Segment)
				d.write(0, 0, 64<<10, pattern(fmtTag("t", 0, 0, 64<<10), size))

				if err := d.f.TryCrash(journalv2test.TornIn(name, n)); err != nil {
					stub(t, "Open", err)
					t.Fatalf("%s: %v", d.label, err)
				}

				d.crashed()
			})
		}
	})

	t.Run("trailer", func(t *testing.T) {
		// How the open after the cut of nothing rebuilt the journal: the
		// trailer whole, the segment read by its catalog.
		var base journalv2.RecoveryStats
		for n := 0; n <= raw.TrailerSize; n++ {
			iterate(t, func(tb testing.TB) {
				d := newDriven(tb, t, fmt.Sprintf("the trailer cut %d bytes short", n))
				p, name := footerParked(t, d)
				waitParked(t, p, "the roll")
				if msg := p.check(); msg != "" {
					t.Fatalf("precondition: the held call is not the footer's sync: %s", msg)
				}

				if err := d.f.Seam.Crash(journalv2test.TornEnd(name, n)); err != nil {
					t.Fatalf("%s: %v", d.label, err)
				}

				p.held.Release()
				<-p.done // the crash took the roll
				d.reopened()
				d.crashed()

				r := d.f.J.Stats().Recovery
				switch {
				case n == 0 && r.CatalogSegments == 0:
					t.Fatalf("precondition: with the trailer whole, recovery read no segment by its catalog: %+v", r)
				case n == 0:
					base = r
				default:
					wantScanned(t, d.label, d.f, base, 1)
				}
			})
		}
	})
}

// reorderScript settles the spare pool, parks spare-making for the rest of
// the pass, then writes two synced records and four unsynced ones across
// both streams, the third of them crossing the segment cap so it is split
// into records of two segments. With spare-making parked, the roll's refill
// writes nothing, so the unsynced writes are the script's own and the
// roll's, the same on every pass.
func reorderScript(t *testing.T, tb testing.TB, d *driven) {
	t.Helper()

	const size = 64 << 10
	awaitSpare(t, d.f, func() { d.writeSynced(0, 2, 0, journalv2test.FixtureSegmentCap) })
	if err := d.f.TryReopen(); err != nil { // the pool is at its target as the open counts it
		t.Fatalf("%s: reopen: %v", d.label, err)
	}

	d.f.Seam.Hold(tb, journalv2test.OpCreate, "*.spare.tmp")

	d.writeSynced(0, 0, 0, size)
	d.writeSynced(1, 1, 0, size)
	d.write(0, 0, size, pattern(fmtTag("c", 0, 0, size), size))
	d.write(1, 1, size, pattern(fmtTag("e", 1, 1, size), size))
	d.write(0, 0, 2*size, pattern(fmtTag("g", 0, 0, 2*size), journalv2test.FixtureSegmentCap))
	d.write(1, 1, 2*size, pattern(fmtTag("h", 1, 1, 2*size), size))
}

// maxReorderedWrites bounds the unsynced seam writes the reordered sweep
// enumerates the subsets of: 2^10 crashes, each on a fresh journal.
//
// decision: the script syncs everything but its last four writes, so the
// unsynced seam writes are those four, the pieces of the split one and what
// the roll between them writes. A journal that leaves more than this many
// unsynced fails the sweep naming the count, rather than running for hours
// or skipping subsets; the upgrade is a smaller segment cap for this check.
const maxReorderedWrites = 10

// TestDamage_ReorderedWrites checks that a crash keeping any subset of the
// unsynced writes, every subset in turn, leaves a journal the reference
// model allows: a write split at the segment cap comes back only as a prefix
// of its pieces, and no record in part.
func TestDamage_ReorderedWrites(t *testing.T) {
	var k int
	for mask := uint64(0); ; mask++ {
		iterate(t, func(tb testing.TB) {
			d := newDriven(tb, t, fmt.Sprintf("the crash keeping the unsynced writes %#b", mask))
			reorderScript(t, tb, d)

			n := d.f.Seam.Unsynced()
			switch {
			case mask == 0 && n < 2:
				t.Fatalf("precondition: the script left %d unsynced seam writes, want at least two, so a later one can survive without an earlier one", n)
			case mask == 0 && n > maxReorderedWrites:
				t.Fatalf("precondition: the script left %d unsynced seam writes, above the %d whose subsets this check enumerates", n, maxReorderedWrites)
			case mask == 0:
				k = n
			case n != k:
				t.Fatalf("precondition: the script left %d unsynced seam writes, %d on its first run: the subsets are not of one set", n, k)
			}

			if err := d.f.TryCrash(journalv2test.Keeping(mask)); err != nil {
				stub(t, "Open", err)
				t.Fatalf("%s: %v", d.label, err)
			}

			d.crashed()
		})
		if mask+1 >= 1<<k {
			break
		}
	}
}

// field is one field of an on-disk structure, as RFC 28's tables name it,
// at off of the structure and size bytes long.
type field struct {
	name      string
	off, size int
}

// The fields of each structure, in the order and with the names of RFC 28's
// tables; a reserved field is named by its offset.
var (
	recordHeaderFields = []field{
		{"magic", 0, 4}, {"kind", 4, 1}, {"reserved_5", 5, 3}, {"segment_id", 8, 8}, {"file_id", 16, 16},
		{"file_offset", 32, 8}, {"length", 40, 8}, {"sequence_number", 48, 8}, {"version_epoch", 56, 8},
		{"version_counter", 64, 8}, {"share_tag", 72, 4}, {"synced_through", 76, 8}, {"payload_checksum", 84, 4},
		{"reserved_88", 88, 4}, {"header_checksum", 92, 4},
	}

	catalogEntryFields = []field{
		{"file_id", 0, 16}, {"file_offset", 16, 8}, {"record_offset", 24, 4}, {"length", 28, 8}, {"sequence_number", 36, 8},
		{"version_epoch", 44, 8}, {"version_counter", 52, 8}, {"kind", 60, 1}, {"reserved_61", 61, 3}, {"share_tag", 64, 4},
		{"reserved_68", 68, 4},
	}

	trailerFields = []field{
		{"magic", 0, 4}, {"layout_version", 4, 2}, {"flags", 6, 2}, {"footer_offset", 8, 8}, {"entry_count", 16, 4},
		{"footer_checksum", 20, 4}, {"reserved_24", 24, 4}, {"crc", 28, 4},
	}

	segmentHeaderFields = []field{
		{"magic", 0, 4}, {"format_version", 4, 2}, {"reserved_6", 6, 2}, {"journal_identity", 8, 16}, {"segment_id", 24, 8},
		{"predecessor", 32, 8}, {"stream", 40, 4}, {"crc", 44, 4},
	}

	sealMarkerFields = []field{
		{"magic", 0, 4}, {"reserved_4", 4, 4}, {"segment_id", 8, 8}, {"offset", 16, 8}, {"reserved_24", 24, 4}, {"crc", 28, 4},
	}

	formatFields = []field{
		{"magic", 0, 4}, {"format_version", 4, 2}, {"reserved_6", 6, 2}, {"journal_identity", 8, 16}, {"installation_id", 24, 16},
		{"generation", 40, 8}, {"reserved_48", 48, 4}, {"crc", 52, 4},
	}
)

// flipBit inverts bit b of the byte at off of the file name, through the
// seam, synced; flipping it again puts the byte back.
func flipBit(t *testing.T, f *journalv2test.Fixture, name string, off int64, b uint) {
	t.Helper()

	file, err := f.Seam.Open(name)
	if err != nil {
		t.Fatalf("flip a bit of %s: %v", name, err)
	}

	p := make([]byte, 1)
	_, err = file.ReadAt(p, off)
	if err = errors.Join(err, file.Close()); err != nil {
		t.Fatalf("flip a bit of %s: read offset %d: %v", name, off, err)
	}

	overwrite(t, f, name, off, []byte{p[0] ^ (1 << b)})
}

// flipSite is a journal with one segment sealed full and catalogued, S,
// holding file 0's records, the first marked of them offloaded with their
// marks synced, a record of file 1 on the other stream, and how the last
// open rebuilt the journal. S is read by its catalog, or by scan when scanned
// says so, its trailer's magic then carrying a flipped bit for the whole
// site.
type flipSite struct {
	f        *journalv2test.Fixture
	recs     []rec
	marked   int
	scanned  bool
	seg      journalv2.SegmentID
	name     string
	other    rec
	footer   int64
	entries  [][raw.CatalogEntrySize]byte
	recovery journalv2.RecoveryStats
}

// newFlipSite builds a flipSite of records of size bytes, the first marked
// of them offloaded, read by scan when scanned says so: the segment is
// filled, file 1 written, the marks made and synced, and the journal
// reopened cleanly so its catalog is what the open read; for a scanned site
// the trailer's magic is then flipped and the journal reopened again.
func newFlipSite(t *testing.T, size, marked int, scanned bool) *flipSite {
	t.Helper()

	f := journalv2test.New(t)
	recs, seg := fillSegment(t, f, 0, 0, size)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	syncFiles(t, f.Share(0), recs[0].id)
	if marked > len(recs)/2 {
		t.Fatalf("precondition: %d records to mark of %d in the segment", marked, len(recs))
	}

	for _, r := range recs[:marked] {
		mark(t, f.Share(0), r.id, r.extent, r.version)
	}

	syncFiles(t, f.Share(0), recs[0].id)
	f.Reopen()

	s := &flipSite{f: f, recs: recs, marked: marked, scanned: scanned, seg: seg, name: segmentName(seg), other: other, recovery: f.J.Stats().Recovery}
	if s.recovery.CatalogSegments == 0 {
		t.Fatalf("precondition: recovery read no segment by its catalog: %+v", s.recovery)
	}

	s.footer, s.entries = trailerOf(t, durable(t, f, s.name))
	if scanned {
		flipBit(t, f, s.name, int64(len(durable(t, f, s.name)))-raw.TrailerSize, 0)
		f.Reopen()
		wantScanned(t, "precondition: with its trailer's magic flipped", f, s.recovery, 1)
		s.recovery = f.J.Stats().Recovery
	}

	return s
}

// reopen opens the journal again, failing with where the sweep stands.
func (s *flipSite) reopen(t *testing.T, where string) {
	t.Helper()

	if err := s.f.TryReopen(); err != nil {
		t.Fatalf("%s: reopen: %v", where, err)
	}
}

// entryOf returns the offset in S of the catalog entry of record r.
func (s *flipSite) entryOf(t *testing.T, r rec) int64 {
	t.Helper()

	loc := locate(t, "the site", s.f, r)
	for i, e := range s.entries {
		if int64(binary.LittleEndian.Uint32(e[24:28])) == loc.Offset {
			return s.footer + int64(i)*raw.CatalogEntrySize
		}
	}

	t.Fatalf("no catalog entry names the record at %d", loc.Offset)
	return 0
}

// wantSuccessor fails the test unless the segment file 0's stream rolled
// into names S as its predecessor, which the seal marker's outcome is
// conditional on.
func (s *flipSite) wantSuccessor(t *testing.T) {
	t.Helper()

	roll := rec{0, s.recs[0].id, journalv2.Extent{Off: int64(len(s.recs)) * s.recs[0].extent.Len, Len: s.recs[0].extent.Len}, journalv2.Version{}, nil}
	succ := locate(t, "the site", s.f, roll).Segment
	hdr, err := raw.ParseSegmentHeader(durable(t, s.f, segmentName(succ))[:raw.SegmentHeaderSize])
	if err != nil || hdr.Predecessor != uint64(s.seg) {
		t.Fatalf("precondition: segment %d does not name %d as its predecessor: %+v, %v", succ, s.seg, hdr, err)
	}
}

// wantExact fails the test unless record r reads its exact bytes through
// h, naming where the sweep stands in the failure.
func wantExact(t *testing.T, where string, h *journalv2.Handle, r rec) {
	t.Helper()

	p, missing, err := readAt(t, h, r.id, r.extent.Off, len(r.p))
	switch {
	case err != nil:
		t.Fatalf("%s: read of %+v: %v", where, r.extent, err)
	case len(missing) != 0:
		t.Fatalf("%s: read of %+v reports missing %+v, want none", where, r.extent, missing)
	case string(p) != string(r.p):
		t.Fatalf("%s: read of %+v returned bytes other than the ones written", where, r.extent)
	}
}

// wantRecordMissing fails the test unless a read of record r reports exactly
// its extent missing and leaves the caller's buffer untouched, naming where
// the sweep stands in the failure.
func wantRecordMissing(t *testing.T, where string, h *journalv2.Handle, r rec) {
	t.Helper()

	p, missing, err := readAt(t, h, r.id, r.extent.Off, len(r.p))
	if err != nil || len(missing) != 1 || missing[0] != r.extent {
		t.Fatalf("%s: read of the flipped record %+v reports missing %+v, err %v, want exactly it", where, r.extent, missing, err)
	}

	if i := slices.IndexFunc(p, func(b byte) bool { return b != sentinel }); i >= 0 {
		t.Fatalf("%s: read of the flipped record %+v wrote byte %#x into the buffer at %d, want it untouched", where, r.extent, p[i], i)
	}
}

// wantAllExact fails the test unless every record of S not in dropped, and
// file 1's record, read their exact bytes.
func (s *flipSite) wantAllExact(t *testing.T, where string, dropped map[int]bool) {
	t.Helper()

	for i, r := range s.recs {
		if !dropped[i] {
			wantExact(t, where, s.f.Share(0), r)
		}
	}

	wantExact(t, where, s.f.Share(1), s.other)
}

// wantNoByteChanged fails the test unless the open after a flip read S by
// scan in place of its catalog, reported no loss, and every record reads its
// exact bytes.
func (s *flipSite) wantNoByteChanged(t *testing.T, where string) {
	t.Helper()

	wantScanned(t, where, s.f, s.recovery, 1)
	if losses := s.f.J.Stats().Losses; len(losses) != 0 {
		t.Fatalf("%s: a flip outside any record was reported as a loss: %+v", where, losses)
	}

	s.wantAllExact(t, where, nil)
}

// wantLossNamed fails the test unless one more extent than base counts was
// dropped as corrupt since the open, with its offloaded bit as offloaded
// says, and the newest loss event names r's extent so.
//
// decision: the newest event is read from the ring and the count from the
// counters, rather than searching the ring, which is bounded without a size
// given, or building a fresh site per byte, which costs a segment fill per
// flip. The counters are since open, so base is what the open left.
func wantLossNamed(t *testing.T, where string, f *journalv2test.Fixture, r rec, offloaded bool, base journalv2.Counters) {
	t.Helper()

	st := f.J.Stats()
	lost, repairable := st.Counters.CorruptExtentsDataLoss-base.CorruptExtentsDataLoss, st.Counters.CorruptExtentsRepairable-base.CorruptExtentsRepairable
	if offloaded && (lost != 0 || repairable != 1) || !offloaded && (lost != 1 || repairable != 0) {
		t.Fatalf("%s: %d extents dropped as data loss and %d as repairable since the open, want one as %s", where, lost, repairable, map[bool]string{true: "repairable", false: "data loss"}[offloaded])
	}

	if len(st.Losses) == 0 {
		t.Fatalf("%s: the loss ring is empty after a loss was counted", where)
	}

	ev := st.Losses[len(st.Losses)-1]
	if ev.File != r.id || ev.Extent != r.extent || ev.Reason != journalv2.LossCorrupt || ev.Offloaded != offloaded {
		t.Fatalf("%s: the newest loss event is %+v, want file %v, extent %+v, corrupt, offloaded %t", where, ev, r.id, r.extent, offloaded)
	}
}

// recordRegion is a region of a record: the header, or a part of the body,
// with the byte offsets within the record it flips.
type recordRegion struct {
	name      string
	bytes     func(size int) []int
	offloaded bool // the flips land on marked records
}

// flipRecordBytes flips every bit of each of the bytes of the region in
// turn, each flip on a record of its own, free ones after the marked run or
// marked ones from its start, and checks the outcome: the record reads
// missing with the caller's buffer untouched, and the records not flipped
// still read their bytes. Where named says so, a loss event names the
// record as corrupt with its bit, found at open where S is scanned and on
// the read where it is catalogued, and the loss generation rises by one for
// a record not offloaded and not at all for one that is. Neither the last
// record of S, which may be split at the cap, nor the last marked one, read
// as a neighbour after every flip, is ever flipped.
func flipRecordBytes(t *testing.T, s *flipSite, bytes []int, offloaded, named bool) {
	t.Helper()

	pool := len(s.recs) - s.marked - 1
	if offloaded {
		pool = s.marked - 1
	}

	if n := 8 * len(bytes); n > pool || s.marked < 2 {
		t.Fatalf("precondition: %d flips, %d records to land them on, %d marked", n, pool, s.marked)
	}

	dropped := map[int]bool{}
	k := 0
	for _, off := range bytes {
		for b := range uint(8) {
			i := s.marked + k
			if offloaded {
				i = k
			}

			k++
			r := s.recs[i]
			loc := locate(t, "the site", s.f, r)
			where := fmt.Sprintf("bit %d of byte %d of the record at %d of %s", b, off, loc.Offset, s.name)
			if s.scanned {
				// An open may write a footer for a segment sealed by an
				// earlier process; the site holds only while it did not.
				wantScanned(t, "precondition: before "+where, s.f, s.recovery, 0)
			}

			generation := s.f.J.Stats().LossGeneration
			flipBit(t, s.f, s.name, loc.Offset+int64(off), b)
			s.reopen(t, where)
			dropped[i] = true

			base := s.f.J.Stats().Counters
			if s.scanned && named {
				wantLossNamed(t, where+", at open", s.f, r, offloaded, journalv2.Counters{})
			}

			wantRecordMissing(t, where, s.f.Share(0), r)
			if !s.scanned && named {
				wantLossNamed(t, where+", on the read", s.f, r, offloaded, base)
			}

			switch got := s.f.J.Stats().LossGeneration; {
			case !named:
			case offloaded && got != generation:
				t.Fatalf("%s: LossGeneration moved from %d to %d on a loss the remote tier can repair", where, generation, got)
			case !offloaded && got != generation+1:
				t.Fatalf("%s: LossGeneration moved from %d to %d on a loss of content not yet offloaded, want it raised by one", where, generation, got)
			}

			// A neighbour of each kind still reads after every flip.
			wantExact(t, where, s.f.Share(0), s.recs[s.marked-1])
			wantExact(t, where, s.f.Share(1), s.other)
		}
	}

	s.wantAllExact(t, fmt.Sprintf("after every flip of %s", s.name), dropped)
}

// flipStructure flips every bit of the field fd of the structure starting at
// base in the file name, checking the outcome after each with check, told
// where the sweep stands, and puts each bit back before the next.
func flipStructure(t *testing.T, s *flipSite, name string, base int64, fd field, check func(where string)) {
	t.Helper()

	for i := range fd.size {
		for b := range uint(8) {
			off := base + int64(fd.off+i)
			where := fmt.Sprintf("bit %d of byte %d of %s, in its %s", b, off, name, fd.name)
			flipBit(t, s.f, name, off, b)
			check(where)
			flipBit(t, s.f, name, off, b)
		}
	}
}

// TestDamage_BitFlip checks the required outcome of one bit flipped in each
// field of each on-disk structure, every bit of the field in turn: a record's
// header or body drops that record, reported with its offloaded bit, at open
// where the segment is scanned and on the read where it is catalogued; a
// catalog entry, the trailer or the seal marker turns the catalog into no
// footer and the segment is scanned; a segment header reports the segment
// damaged with its records still attached; a seal marker of a segment a
// successor names reports it corrupt; the format file fails the open. After
// every flip, every record not dropped reads its exact bytes.
//
// decision: the payload is flipped at its first, middle and last byte only,
// one checksum covering it uniformly, where every other field is flipped
// bit by bit. The record regions run on 4 KiB records, so each flip lands
// on a record of its own, since a dropped record stays dropped by its loss
// record once its byte is put back; the other regions run on 64 KiB records
// and put each bit back before the next. A seal marker of a segment no
// successor names is not flipped: the row gives no outcome for it.
//
// decision: a record header flipped in a scanned segment is checked only
// for the record reading missing and no byte of any record changing, with
// no loss event named. RFC 28 section 4.4 and RFC 1 section 3.8 want the
// loss reported naming the file and the extent, while RFC 28 section 3.3
// has a scan refuse the record and resynchronise, and no field of a header
// that fails to verify can name either. Assert the event once that is
// settled.
func TestDamage_BitFlip(t *testing.T) {
	const small, large = 4096, 64 << 10
	const marked = 32

	for _, site := range []struct {
		suffix  string
		scanned bool
	}{{"", false}, {"_scanned", true}} {
		t.Run("record_header"+site.suffix, func(t *testing.T) {
			for _, fd := range recordHeaderFields {
				t.Run(fd.name, func(t *testing.T) {
					s := newFlipSite(t, small, marked, site.scanned)
					var bytes []int
					for i := range fd.size {
						bytes = append(bytes, fd.off+i)
					}

					flipRecordBytes(t, s, bytes, false, !site.scanned)
				})
			}
		})

		t.Run("write_body"+site.suffix, func(t *testing.T) {
			payload := func(size int) []int {
				return []int{raw.WriteMTimeOffset + 8, raw.WriteMTimeOffset + 8 + size/2, raw.WriteMTimeOffset + 8 + size - 1}
			}

			for _, rg := range []recordRegion{
				{"modification_time", func(int) []int {
					var bytes []int
					for i := range 8 {
						bytes = append(bytes, raw.WriteMTimeOffset+i)
					}
					return bytes
				}, false},
				{"payload", payload, false},
				{"payload_of_offloaded_record", payload, true},
			} {
				t.Run(rg.name, func(t *testing.T) {
					s := newFlipSite(t, small, marked, site.scanned)
					flipRecordBytes(t, s, rg.bytes(small), rg.offloaded, true)
				})
			}
		})
	}

	t.Run("catalog_entry", func(t *testing.T) {
		for _, fd := range catalogEntryFields {
			t.Run(fd.name, func(t *testing.T) {
				s := newFlipSite(t, large, 0, false)
				base := s.entryOf(t, s.recs[3])
				flipStructure(t, s, s.name, base, fd, func(where string) {
					s.reopen(t, where)
					s.wantNoByteChanged(t, where)
				})
			})
		}
	})

	t.Run("trailer", func(t *testing.T) {
		for _, fd := range trailerFields {
			t.Run(fd.name, func(t *testing.T) {
				s := newFlipSite(t, large, 0, false)
				base := int64(len(durable(t, s.f, s.name))) - raw.TrailerSize
				flipStructure(t, s, s.name, base, fd, func(where string) {
					s.reopen(t, where)
					s.wantNoByteChanged(t, where)
				})
			})
		}
	})

	t.Run("segment_header", func(t *testing.T) {
		for _, fd := range segmentHeaderFields {
			t.Run(fd.name, func(t *testing.T) {
				s := newFlipSite(t, large, 0, false)
				flipStructure(t, s, s.name, 0, fd, func(where string) {
					s.reopen(t, where)
					if !segmentOf(t, where, s.f, s.seg).Damaged || s.f.J.Stats().DamagedSegments == 0 {
						t.Fatalf("%s: segment %d is not reported damaged with its header changed", where, s.seg)
					}

					s.wantAllExact(t, where, nil)
				})
			})
		}
	})

	t.Run("seal_marker", func(t *testing.T) {
		for _, fd := range sealMarkerFields {
			t.Run(fd.name, func(t *testing.T) {
				s := newFlipSite(t, large, 0, false)
				s.wantSuccessor(t)
				flipStructure(t, s, s.name, s.footer-raw.SealMarkerSize, fd, func(where string) {
					s.reopen(t, where)
					if !segmentOf(t, where, s.f, s.seg).Damaged {
						t.Fatalf("%s: segment %d, named by its successor, is not reported corrupt with its seal marker changed", where, s.seg)
					}

					wantScanned(t, where, s.f, s.recovery, 1)
					s.wantAllExact(t, where, nil)
				})
			})
		}
	})

	t.Run("format", func(t *testing.T) {
		for _, fd := range formatFields {
			t.Run(fd.name, func(t *testing.T) {
				s := newFlipSite(t, large, 0, false)
				flipStructure(t, s, "format", 0, fd, func(where string) {
					writes := s.f.Seam.CallsFor(journalv2test.OpWrite, "format") + s.f.Seam.CallsFor(journalv2test.OpTruncate, "format") + s.f.Seam.CallsFor(journalv2test.OpCreate, "format.tmp")
					err := s.f.TryReopen()
					stub(t, "Open", err)
					// decision: the magic and the version are read before the
					// checksum, and the format says neither which check a bad
					// magic fails first nor that a bad version is a checksum
					// failure, so both fields may be refused as an unknown
					// format instead.
					refused := errors.Is(err, journalv2.ErrCorrupt) || ((fd.name == "magic" || fd.name == "format_version") && errors.Is(err, journalv2.ErrUnknownFormat))
					if !refused {
						t.Fatalf("%s: the open returned %v, want ErrCorrupt", where, err)
					}

					if got := s.f.Seam.CallsFor(journalv2test.OpWrite, "format") + s.f.Seam.CallsFor(journalv2test.OpTruncate, "format") + s.f.Seam.CallsFor(journalv2test.OpCreate, "format.tmp"); got != writes {
						t.Fatalf("%s: the open wrote to the format file; it is never repaired in silence", where)
					}
				})
				s.reopen(t, "with every bit put back")
				s.wantAllExact(t, "with every bit put back", nil)
			})
		}
	})
}

// TestDamage_SegmentDeleted checks the edits table's row for a segment
// deleted from outside: the journal opens, the extents the segment held are
// no longer held, so a read of them reports them missing, and everything
// else is still served. The deleted name is never made again, by a create or
// by the rename a spare reaches its name through: a fresh id exceeds every
// id the directory names, those found at open included (RFC 28 section 2.2,
// lines 316 to 319).
//
// decision: the row says the deletion is not noticed by the journal, and
// nothing here asserts that: a journal that counts the missing predecessor
// its successor names as rejected or damaged passes, since the row's outcome
// column is all it requires.
func TestDamage_SegmentDeleted(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)
	recs, seg := fillSegment(t, f, 0, 0, size)
	other := writeSynced(t, f, 1, 1, 0, size)
	syncFiles(t, f.Share(0), recs[0].id)
	mark(t, f.Share(0), recs[1].id, recs[1].extent, recs[1].version)
	syncFiles(t, f.Share(0), recs[0].id)
	roll := rec{0, recs[0].id, journalv2.Extent{Off: int64(len(recs)) * size, Len: size}, journalv2.Version{}, pattern(fmtTag("r", 0, 0, int64(len(recs))*size), size)}
	name := segmentName(seg)

	f.Shutdown()
	before := callsFor(f, name)
	if err := f.Seam.Remove(name); err != nil {
		t.Fatalf("remove the segment: %v", err)
	}

	if err := f.Seam.SyncDir(); err != nil {
		t.Fatalf("sync the directory: %v", err)
	}

	f.Reopen()

	// The last record of the segment may be split at the cap, its second
	// piece held in the successor.
	for _, r := range recs[:len(recs)-1] {
		wantMissing(t, f.Share(0), r.id, r.extent)
	}

	wantMissingFrom(t, f.Share(0), recs[len(recs)-1].id, recs[len(recs)-1].extent)
	wantBytes(t, f.Share(0), roll.id, roll.extent.Off, roll.p)
	wantBytes(t, f.Share(1), other.id, 0, other.p)

	after := callsFor(f, name)
	for _, op := range []string{journalv2test.OpCreate, journalv2test.OpWrite} {
		if after[op] != before[op] {
			t.Fatalf("the open made %d calls of %s on the deleted segment's name; a fresh id exceeds every id the directory names", after[op]-before[op], op)
		}
	}

	// A spare is made under a temporary name, so a take and its refill show
	// whether the deleted name is drawn again.
	awaitSpare(t, f, func() { writeSynced(t, f, 0, 2, 0, journalv2test.FixtureSegmentCap) })
	if slices.Contains(segmentFiles(t, f), name) {
		t.Fatalf("the deleted segment's name %s was made again by a spare; a fresh id exceeds every id the directory names", name)
	}
}

// TestDamage_UnnamedFileLeftAlone checks the edits table's row for a file
// the journal did not name, added beside it: the journal opens, serves, and
// leaves the file as it is. A file that is no segment is not a segment left
// unattached, which is what RejectedSegments counts (RFC 1 section 3.7), so
// the count stays at zero.
func TestDamage_UnnamedFileLeftAlone(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)
	a := writeSynced(t, f, 0, 0, 0, size)
	b := writeSynced(t, f, 1, 1, 0, size)
	notes := pattern("notes", 4096)
	plant(t, f, "notes.txt", notes)
	before := callsFor(f, "notes.txt")
	f.Reopen()

	if got := f.J.Stats().RejectedSegments; got != 0 {
		t.Fatalf("RejectedSegments = %d with a file that is no segment in the directory, want 0", got)
	}

	wantLeftAlone(t, f, "notes.txt", notes, before)
	wantBytes(t, f.Share(0), a.id, 0, a.p)
	wantBytes(t, f.Share(1), b.id, 0, b.p)
}

// TestDamage_DirectoryRestored checks the edits table's row for a whole
// directory restored from an old copy: at the reseed, content older than
// what the remote tier holds is dropped as stale and reads missing, and new
// versions start above the floor, so a mark at the floor leaves a new write
// unmarked.
func TestDamage_DirectoryRestored(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	a := writeSynced(t, f, 0, 0, 0, size)
	mark(t, h, a.id, a.extent, a.version)
	syncFiles(t, h, a.id)
	other := writeSynced(t, f, 1, 1, 0, size)

	f.Shutdown()
	copied := map[string][]byte{}
	for _, name := range list(t, f) {
		copied[name] = durable(t, f, name)
	}

	f.Reopen()
	h = f.Share(0)
	b := writeSynced(t, f, 0, 0, 0, size)
	mark(t, h, b.id, b.extent, b.version)
	syncFiles(t, h, b.id)

	f.Shutdown()
	emptyDir(t, f)
	for name, data := range copied {
		plant(t, f, name, data)
	}

	f.Reopen()
	h = f.Share(0)

	if e := entryAt(t, f, id, 0); e.Version != a.version || !e.Offloaded {
		t.Fatalf("precondition: A is held as %+v after the restore, want its version %v and its offloaded bit", e, a.version)
	}

	before := f.J.Stats()
	err := h.MarkOffloaded(t.Context(), id, []journalv2.Extent{a.extent}, b.version, b.version)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark at B's versions: %v", err)
	}

	ev := lastLoss(t, f, before)
	if ev.File != id || ev.Extent != a.extent || ev.Reason != journalv2.LossStale {
		t.Fatalf("loss event %+v, want file %v, extent %+v, dropped as stale", ev, id, a.extent)
	}

	wantMissing(t, h, id, a.extent)

	floor := f.Floor()
	c := writeSynced(t, f, 0, 0, size, size)
	if c.version.Compare(floor) <= 0 {
		t.Fatalf("a write after the restore has version %v, not above the floor %v", c.version, floor)
	}

	err = h.MarkOffloaded(t.Context(), id, []journalv2.Extent{c.extent}, floor, floor)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark at the floor: %v", err)
	}

	wantRelease(t, h, c.id, c.extent, false)
	wantBytes(t, f.Share(1), other.id, 0, other.p)
}
