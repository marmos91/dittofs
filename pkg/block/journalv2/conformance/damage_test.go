//go:build journal_conformance

package conformance

import (
	"encoding/binary"
	"fmt"
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
