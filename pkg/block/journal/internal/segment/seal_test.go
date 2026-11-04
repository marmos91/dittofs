package segment_test

import (
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// sealAppended appends about five records through the stream and returns
// their places, the warm shape every seal check starts from.
func sealAppended(t *testing.T, e streamEnv) []segment.Placed {
	t.Helper()

	var placed []segment.Placed
	for i := range 5 {
		payload := streamTestBody(64, byte(0x30+i))
		h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, payload, 7, core.Version{Counter: uint64(i + 1)})
		placed = append(placed, streamAppendWrite(t, e, h, body))
	}
	return placed
}

// sealCounters snapshots every seam counter a seal can move, so a check can
// assert a refusal or a second seal made no call at all.
type sealCounters struct {
	writes, syncs, truncates, opens int
}

func sealCountersOf(e streamEnv, seg string) sealCounters {
	return sealCounters{
		writes:    e.seam.CallsFor(journaltest.OpWrite, seg),
		syncs:     e.seam.CallsFor(journaltest.OpSync, seg),
		truncates: e.seam.CallsFor(journaltest.OpTruncate, seg),
		opens:     e.seam.CallsFor(journaltest.OpOpen, seg),
	}
}

// sealFileSize returns the size of the segment's file through the seam's
// durable directory: the one source of truth for every size a seal check
// asserts, read where the cache cannot be asked.
func sealFileSize(t *testing.T, e streamEnv, id core.SegmentID) int64 {
	t.Helper()

	f, err := e.seam.FS().Open(streamSegName(id))
	if err != nil {
		t.Fatalf("opening segment %d's file: %v", id, err)
	}
	defer f.Close()
	size, err := f.Size()
	if err != nil {
		t.Fatalf("the size of segment %d's file: %v", id, err)
	}
	return size
}

// sealRecorderFault is a recording fault: it logs every seam call on the
// segment's file, and can fail chosen ops the first times they arrive.
type sealRecorderFault struct {
	log   *[]string
	seg   string
	fails map[string][]error // op -> the errors to fail in order
}

func (f *sealRecorderFault) At(op string, key []byte) (dl time.Duration, tear int, err error) {
	if string(key) == f.seg {
		*f.log = append(*f.log, op)
		if errs := f.fails[op]; len(errs) > 0 {
			f.fails[op] = errs[1:]
			return 0, 0, errs[0]
		}
	}
	return 0, 0, nil
}

// fail arms op to fail with errs, in the order given, once each.
func (f *sealRecorderFault) fail(op string, errs ...error) {
	if f.fails == nil {
		f.fails = map[string][]error{}
	}
	f.fails[op] = errs
}

// sealEntriesOf builds the catalog entries the appended records describe, in
// offset order, the way the seal must hand them to the footer.
//
// sealEntriesOf decodes the record header at each place, so what it builds
// names what landed rather than what the caller believed.
func sealEntriesOf(t *testing.T, e streamEnv, placed []segment.Placed) []format.CatalogEntry {
	t.Helper()

	entries := make([]format.CatalogEntry, 0, len(placed))
	for _, p := range placed {
		h, _ := streamReadRecord(t, e, p.Loc)
		entries = append(entries, format.CatalogEntry{
			File:         h.File,
			Extent:       h.Extent,
			RecordOffset: p.Loc.Off,
			Seq:          h.Seq,
			Version:      h.Version,
			Kind:         h.Kind,
			Tag:          h.Tag,
		})
	}
	slices.SortFunc(entries, func(a, b format.CatalogEntry) int {
		return int(a.RecordOffset - b.RecordOffset)
	})
	return entries
}

// TestSealFullWritesTheTrailerAtTheCapInTwoSyncs checks the full seal's
// contract: after five records the seal issues exactly two syncs, no
// truncation, at least one write; the file stays at its cap with the trailer
// in its last 32 bytes; the footer verifies through the format's own reader
// and names every record; the segment is Sealed in the ledger; and the
// footprint did not move. It catches an extra flush (the floor D11 pin), a
// trailer written after the footer on a full seal, and a full seal that
// truncates (R26, R27, R31, R35, R72).
func TestSealFullWritesTheTrailerAtTheCapInTwoSyncs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	placed := sealAppended(t, e)
	marker := placed[len(placed)-1].Loc.Off + placed[len(placed)-1].Loc.Len
	before := sealCountersOf(e, seg)

	res, err := e.s.Seal(false)
	if err != nil {
		t.Fatalf("the full Seal: %v", err)
	}
	if res.Marker != marker {
		t.Fatalf("the seal's marker is at %d, want the last record's end %d", res.Marker, marker)
	}
	if res.Size != streamCap {
		t.Fatalf("a full seal is accounted at %d, want the cap %d", res.Size, streamCap)
	}
	if res.TailErr != nil {
		t.Fatalf("a full seal's TailErr is %v, want nil", res.TailErr)
	}

	after := sealCountersOf(e, seg)
	if got := after.syncs - before.syncs; got != 2 {
		t.Fatalf("the full seal issued %d syncs, want exactly 2", got)
	}
	if got := after.truncates - before.truncates; got != 0 {
		t.Fatalf("the full seal made %d truncations, want none", got)
	}
	if after.writes < before.writes+1 {
		t.Fatalf("the full seal wrote nothing, want at least the marker, footer and trailer")
	}

	// The file is at its cap, its trailer in the last 32 bytes.
	size := sealFileSize(t, e, 1)
	if size != streamCap {
		t.Fatalf("after the full seal the file is %d bytes, want the cap %d", size, streamCap)
	}

	// The footer verifies and describes every record. A Ref is an
	// io.ReaderAt, so the format's reader runs over it directly.
	r := sealReaderOf(t, e, 1)
	defer r.Close()
	footer, err := format.ReadFooter(r, size, streamIdentity, 1)
	if err != nil {
		t.Fatalf("the sealed segment's footer does not verify: %v", err)
	}
	if want := marker + format.SealMarkerSize; footer.Offset != want {
		t.Fatalf("the footer starts at %d, want the marker's end %d", footer.Offset, want)
	}
	want := sealEntriesOf(t, e, placed)
	if len(footer.Entries) != len(want) {
		t.Fatalf("the footer holds %d entries, want the %d records appended", len(footer.Entries), len(want))
	}
	for i := range want {
		if footer.Entries[i] != want[i] {
			t.Fatalf("entry %d is %+v, want %+v", i, footer.Entries[i], want[i])
		}
	}

	// The segment is sealed in the ledger, at its cap, with no share charged
	// a byte it does not hold.
	if state, err := e.set.State(1); err != nil || state != segment.Sealed {
		t.Fatalf("the segment's state after the seal is %v, %v, want Sealed", state, err)
	}
	c := e.set.Counters()
	if c.Sealed != 1 {
		t.Fatalf("the ledger counts %d sealed segments, want 1", c.Sealed)
	}
	if c.UsedBytes != streamCap {
		t.Fatalf("the footprint after the full seal is %d, want the cap %d", c.UsedBytes, streamCap)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("the ledger after the seal: %v", err)
	}
}

// sealReaderOf acquires the segment and hands back a reader that releases
// the reference on Close, so format.ReadFooter runs over a held descriptor
// and the reference never leaks.
func sealReaderOf(t *testing.T, e streamEnv, id core.SegmentID) sealReader {
	t.Helper()

	ref, err := e.cache.Acquire(id)
	if err != nil {
		t.Fatalf("acquiring segment %d: %v", id, err)
	}
	return sealReader{ref: ref}
}

// sealReader is the segment's whole file as an io.ReaderAt: a Ref already
// reads at offsets, and the closer releases it.
type sealReader struct {
	ref segment.Ref
}

func (r sealReader) ReadAt(p []byte, off int64) (int, error) {
	return r.ref.ReadAt(p, off)
}

func (r sealReader) Close() error {
	r.ref.Release()
	return nil
}
