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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// spareWait bounds, in real time, how long a call refused for want of a spare
// segment is retried.
//
// decision: spares are made off the write path, and a roll that finds none
// ready is refused at once, so with the fixture's small segments a correct
// journal can refuse a write now and accept it a moment later. Retrying is
// what its caller does. The wait is real time because spare making runs on
// the journal's own goroutines; it is spent only when a spare is missing.
const spareWait = 2 * time.Second

// retrySpare calls op until it returns anything but a NoSpaceError naming
// LimitSpare, or spareWait has passed, and returns op's last error.
func retrySpare(op func() error) error {
	deadline := time.Now().Add(spareWait)
	for {
		err := op()
		var ns *journalv2.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != journalv2.LimitSpare || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

// tryWrite writes p at off of file id through h, retrying a refusal for want
// of a spare, and returns its version.
func tryWrite(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, p []byte) (journalv2.Version, error) {
	var v journalv2.Version
	err := retrySpare(func() (err error) {
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

	loc := heldEntry(t, f, id).Location
	name := segmentName(loc.Segment)
	data, err := f.Seam.Durable(name)
	if err != nil {
		t.Fatalf("read the segment holding the record: %v", err)
	}

	at := bytes.Index(data[loc.Offset:], p)
	if at < 0 {
		t.Fatalf("the payload is not in segment %d past offset %d", loc.Segment, loc.Offset)
	}
	off := loc.Offset + int64(at) + int64(len(p)/2)

	file, err := f.Seam.Open(name)
	if err != nil {
		t.Fatalf("corrupt %s: %v", name, err)
	}
	_, err = file.WriteAt([]byte{data[off] ^ 0xff}, off)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		t.Fatalf("corrupt %s: %v", name, err)
	}
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
