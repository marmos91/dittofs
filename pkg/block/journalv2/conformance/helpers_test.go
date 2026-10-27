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
	"os"
	"path/filepath"
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

	if err := h.MarkOffloaded(t.Context(), id, []journalv2.Extent{e}, v, v); err != nil {
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
// stored in one record, and unique on disk.
//
// decision: a check that corrupts and then reads relies on the read going to
// the segment, verifying the record's whole payload as every path that hands
// out a record's bytes must. A journal serving a synced record from a copy in
// memory would never see the flipped byte. If that is ever allowed, those
// checks reopen between the damage and the read.
func corrupt(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID, p []byte) {
	t.Helper()

	loc := heldEntry(t, f, id).Location
	path := filepath.Join(f.Dir, segmentName(loc.Segment))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the segment holding the record: %v", err)
	}

	at := bytes.Index(data[loc.Offset:], p)
	if at < 0 {
		t.Fatalf("the payload is not in segment %d past offset %d", loc.Segment, loc.Offset)
	}
	off := loc.Offset + int64(at) + int64(len(p)/2)

	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{data[off] ^ 0xff}, off); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
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
