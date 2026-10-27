//go:build journal_conformance

// Package conformance holds the journal's conformance checks: one test per
// check the journal must pass, each driving only the journal's public surface
// and observing the disk only through the fixture's seam and the real files.
//
// Every test here is a known failure until the behaviour it checks lands, and
// is listed in test/journalv2/known.yaml until then.
package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// tryWrite writes p at off of file id through h, retrying a refusal for want
// of a spare, and returns its version.
func tryWrite(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, p []byte) (journalv2.Version, error) {
	var v journalv2.Version
	err := journalv2test.RetrySpare(func() (err error) {
		v, err = h.WriteAt(t.Context(), id, off, p, 0)
		return err
	})
	return v, err
}

// write writes p at off of file id through h and returns its version.
func write(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, p []byte) journalv2.Version {
	t.Helper()

	v, err := tryWrite(t, h, id, off, p)
	stub(t, "WriteAt", err)
	if err != nil {
		t.Fatalf("write %d bytes at %d: %v", len(p), off, err)
	}
	return v
}

// pattern returns n bytes no other call returns, so a test can find them on
// disk without knowing the record layout around them.
func pattern(tag string, n int) []byte {
	head := fmt.Appendf(nil, "<%s>", tag)
	return append(head, bytes.Repeat([]byte{'.'}, n-len(head))...)
}

// mark sets the offloaded bit of extent e of file id, as the offload of
// version v.
func mark(t *testing.T, h *journalv2.Handle, id journalv2.FileID, e journalv2.Extent, v journalv2.Version) {
	t.Helper()

	err := h.MarkOffloaded(t.Context(), id, []journalv2.Extent{e}, v, v)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark %+v of a file offloaded: %v", e, err)
	}
}

// segmentName is the file name of segment id in a journal's directory.
func segmentName(id journalv2.SegmentID) string {
	return fmt.Sprintf("%020d.seg", id)
}

// newestID returns the highest segment id any file in the fixture's
// directory names, segments and spares alike: the set a new segment's id must
// exceed.
func newestID(t *testing.T, f *journalv2test.Fixture) journalv2.SegmentID {
	t.Helper()

	names, err := f.Seam.List()
	if err != nil {
		t.Fatalf("list the journal's directory: %v", err)
	}
	var newest journalv2.SegmentID
	for _, name := range names {
		if !strings.HasSuffix(name, ".seg") && !strings.HasSuffix(name, ".spare.tmp") {
			continue
		}
		id, err := strconv.ParseUint(name[:strings.IndexByte(name, '.')], 10, 64)
		if err != nil {
			continue // not a name the journal gives a segment
		}
		newest = max(newest, journalv2.SegmentID(id))
	}
	return newest
}

// heldEntry returns the first held index entry of file id.
func heldEntry(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID) journalv2.IndexEntry {
	t.Helper()

	entries, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect a file: %v", err)
	}
	for _, e := range entries {
		if e.Kind == journalv2.EntryHeld {
			return e
		}
	}
	t.Fatalf("the file has no held extent; entries: %+v", entries)
	panic("unreachable")
}

// corrupt flips one byte of payload p of file id where the journal stored it,
// behind the journal's back, as a disk fault would: the index still claims
// the extent, and its record no longer verifies. p must be what one write
// stored in one record, synced, and unique on disk. The byte is written and
// synced through the seam, so its durable image holds the damage and a crash
// keeps it.
//
// decision: a check that corrupts and then reads relies on the read going to
// the segment, verifying the record's whole payload as every path that hands
// out a record's bytes must. A journal serving a synced record from a copy in
// memory would never see the flipped byte. If that is ever allowed, those
// checks reopen between the damage and the read.
func corrupt(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID, p []byte) {
	t.Helper()
	corruptAt(t, f, id, p, len(p)/2)
}

// corruptAt is corrupt flipping byte at of p rather than its middle one.
func corruptAt(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID, p []byte, at int) {
	t.Helper()

	corruptRecordAt(t, f, heldEntry(t, f, id).Location, p, at)
}

// corruptRecord is corrupt for the record at loc, whose payload p need not
// be the first its file holds. It finds p in the segment's bytes as a read
// sees them now, so a record no sync made durable, as one a failed sync
// covered, is found too.
func corruptRecord(t *testing.T, f *journalv2test.Fixture, loc journalv2.RecordRef, p []byte) {
	t.Helper()

	corruptRecordAt(t, f, loc, p, len(p)/2)
}

// corruptRecordAt is corruptRecord flipping byte at of p rather than its
// middle one.
func corruptRecordAt(t *testing.T, f *journalv2test.Fixture, loc journalv2.RecordRef, p []byte, at int) {
	t.Helper()

	name := segmentName(loc.Segment)
	data := liveBytes(t, f, name)

	start := bytes.Index(data[loc.Offset:], p)
	if start < 0 {
		t.Fatalf("the payload is not in segment %d past offset %d", loc.Segment, loc.Offset)
	}

	flipAt(t, f, name, loc.Offset+int64(start)+int64(at))
}

// flipAt inverts the byte at off of the segment file name, through the seam,
// and syncs the file, so the durable image holds the damage.
func flipAt(t *testing.T, f *journalv2test.Fixture, name string, off int64) {
	t.Helper()

	file, err := f.Seam.Open(name)
	if err != nil {
		t.Fatalf("flip a byte of %s: %v", name, err)
	}

	b := make([]byte, 1)
	if _, err := file.ReadAt(b, off); err != nil {
		t.Fatalf("flip a byte of %s: read offset %d: %v", name, off, err)
	}

	_, err = file.WriteAt([]byte{b[0] ^ 0xff}, off)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		t.Fatalf("flip a byte of %s: %v", name, err)
	}
}

// liveBytes returns the bytes of the segment file name as a read of it sees
// them now, unsynced and lost changes included, unlike the seam's durable
// image.
func liveBytes(t *testing.T, f *journalv2test.Fixture, name string) []byte {
	t.Helper()

	file, err := f.Seam.Open(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	size, err := file.Size()
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	b := make([]byte, size)
	_, err = file.ReadAt(b, 0)
	if errors.Is(err, io.EOF) {
		err = nil
	}

	if err = errors.Join(err, file.Close()); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return b
}

// sentinel fills a read's buffer before the read, so a byte the journal did
// not write is told from one it did: no pattern byte has this value.
const sentinel byte = 0xa5

// readAt reads n bytes at off of file id through h into a buffer filled with
// sentinel, and returns the buffer, the extents reported missing and the
// read's error, which a read that drops a corrupt record may return.
func readAt(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, n int) ([]byte, []journalv2.Extent, error) {
	t.Helper()

	p := bytes.Repeat([]byte{sentinel}, n)
	_, missing, _, _, err := h.ReadAt(t.Context(), id, off, p)
	stub(t, "ReadAt", err)
	return p, missing, err
}

// wantBytes fails the test unless a read at off of file id returns want
// exactly, with nothing missing.
func wantBytes(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, want []byte) {
	t.Helper()

	p, missing, err := readAt(t, h, id, off, len(want))
	if err != nil {
		t.Fatalf("read %d bytes at %d: %v", len(want), off, err)
	}
	if len(missing) != 0 {
		t.Fatalf("read %d bytes at %d: missing %+v, want none", len(want), off, missing)
	}
	if !bytes.Equal(p, want) {
		t.Fatalf("read %d bytes at %d: got % x..., want % x...", len(want), off, p[:16], want[:16])
	}
}

// wantMissing fails the test unless a read of extent e of file id reports
// exactly e missing and leaves every byte of its buffer untouched.
func wantMissing(t *testing.T, h *journalv2.Handle, id journalv2.FileID, e journalv2.Extent) {
	t.Helper()

	p, missing, _ := readAt(t, h, id, e.Off, int(e.Len))
	if len(missing) != 1 || missing[0] != e {
		t.Fatalf("read of %+v reports missing %+v, want exactly it", e, missing)
	}

	if i := bytes.IndexFunc(p, func(r rune) bool { return r != rune(sentinel) }); i >= 0 {
		t.Fatalf("read of %+v wrote byte %#x into p at %d, want p untouched", e, p[i], i)
	}
}

// syncFiles syncs ids through h.
func syncFiles(t *testing.T, h *journalv2.Handle, ids ...journalv2.FileID) {
	t.Helper()

	err := h.Sync(t.Context(), ids...)
	stub(t, "Sync", err)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// entryAt returns the held index entry of file id that covers offset off.
func entryAt(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID, off int64) journalv2.IndexEntry {
	t.Helper()

	entries, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect a file: %v", err)
	}

	for _, e := range entries {
		if e.Kind == journalv2.EntryHeld && e.Extent.Off <= off && off < e.Extent.Off+e.Extent.Len {
			return e
		}
	}

	t.Fatalf("no held extent of the file covers offset %d; entries: %+v", off, entries)
	panic("unreachable")
}

// segmentInfo returns the accounting of segment id, which the journal must
// own.
func segmentInfo(t *testing.T, f *journalv2test.Fixture, id journalv2.SegmentID) journalv2.SegmentInfo {
	t.Helper()

	for _, s := range f.J.Inspect().Segments() {
		if s.ID == id {
			return s
		}
	}

	t.Fatalf("segment %d is not among the journal's", id)
	panic("unreachable")
}

// lastLoss fails the test unless Stats holds exactly one loss event more than
// before, and returns it.
func lastLoss(t *testing.T, f *journalv2test.Fixture, before journalv2.Stats) journalv2.LossEvent {
	t.Helper()

	losses := f.J.Stats().Losses
	if len(losses) != len(before.Losses)+1 {
		t.Fatalf("%d loss events, want %d: %+v", len(losses), len(before.Losses)+1, losses)
	}

	return losses[len(losses)-1]
}

// plant writes data to a new, durable file name in the fixture's directory,
// through the seam, as a file someone copied in would be.
func plant(t *testing.T, f *journalv2test.Fixture, name string, data []byte) {
	t.Helper()

	file, err := f.Seam.Create(name)
	if err != nil {
		t.Fatalf("plant %s: %v", name, err)
	}
	if _, err := file.WriteAt(data, 0); err != nil {
		t.Fatalf("plant %s: %v", name, err)
	}
	err = errors.Join(file.Sync(), file.Close(), f.Seam.SyncDir())
	if err != nil {
		t.Fatalf("plant %s: %v", name, err)
	}
}

// stub fails the test with "stub: <call>" when err is the journal's
// ErrNotImplemented, so a check that fails on a journal call not yet written
// never reads as failing its own assertion. Every journal call whose error a
// check reads passes it here first.
func stub(t *testing.T, call string, err error) {
	t.Helper()

	if errors.Is(err, journalv2.ErrNotImplemented) {
		t.Fatalf("stub: %s", call)
	}
}

// callError is the error of the journal call named call, made on a goroutine
// that cannot fail the test itself: the test passes it to stub once it has
// joined the goroutine.
type callError struct {
	call string
	err  error
}

func (e *callError) Error() string { return e.call + ": " + e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// called returns err as the error of the journal call named call.
func called(call string, err error) error { return &callError{call, err} }

// wantNoSpace fails the test unless err is a NoSpaceError naming limit.
func wantNoSpace(t *testing.T, err error, limit journalv2.Limit, what string) {
	t.Helper()

	var ns *journalv2.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("%s returned %v, want a NoSpaceError naming %v", what, err, limit)
	}
	if ns.Limit != limit {
		t.Fatalf("%s was refused by the %v limit, want %v", what, ns.Limit, limit)
	}
}
