//go:build journal_conformance

package conformance

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// The checks here fail by reaching a state the journal cannot leave, or by
// consuming without bound.

// spareNames returns the names of the segment files in the journal's
// directory that hold no segment header: spares no stream has taken, in name
// order.
func spareNames(t *testing.T, f *journalv2test.Fixture) []string {
	t.Helper()

	var names []string
	for _, name := range segmentFiles(t, f) {
		b := durable(t, f, name)
		if len(b) < raw.SegmentHeaderSize {
			t.Fatalf("%s is %d bytes, shorter than a segment header", name, len(b))
		}

		if _, err := raw.ParseSegmentHeader(b[:raw.SegmentHeaderSize]); err != nil {
			names = append(names, name)
		}
	}

	return names
}

// spareCount returns how many spares the journal's directory holds.
func spareCount(t *testing.T, f *journalv2test.Fixture) int {
	t.Helper()
	return len(spareNames(t, f))
}

// TestRFC28B_SegmentReclaimable checks that a segment whose only record was
// dropped as corrupt is retired by repack and its storage recovered, so one
// flipped bit never pins a segment's storage for good.
//
// decision: the retirement may unlink the segment or recycle it as a spare,
// and the check accepts either, because the format decides between them by
// the spare pool's level at the retirement, which a check cannot pin from
// the public surface, and no rule makes a damaged segment one or the other.
// Only a segment whose sync or append failed is never recycled, and this one
// failed neither.
func TestRFC28B_SegmentReclaimable(t *testing.T) {
	f := journalv2test.New(t)
	a := writeSynced(t, f, 0, 0, 0, 128<<10)
	b := writeSynced(t, f, 1, 1, 0, 64<<10)

	// A's segment is sealed while idle, so A is its only record and the
	// segment carries nothing else repack would have to carry forward.
	seg := sealIdle(t, f, a)
	name := segmentName(seg)
	if !segmentInfo(t, f, seg).Sealed {
		t.Fatalf("segment %d holding A is not sealed after %v idle", seg, journalv2test.IdleSealAge)
	}

	corruptAndRead(t, f, a)

	spares, newest, used := spareCount(t, f), newestID(t, f), f.J.Stats().UsedBytes
	_, err := f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
	stub(t, "Repack", err)
	if err != nil {
		t.Fatalf("repack the segment: %v", err)
	}

	removed, renamed := f.Seam.CallsFor(journalv2test.OpRemove, name), f.Seam.CallsFor(journalv2test.OpRename, name)
	switch {
	case removed == 1 && renamed == 0:
		if _, err := f.Seam.Durable(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s was removed, and a crash would still leave it: %v", name, err)
		}

		if got := f.J.Stats().UsedBytes; got != used-journalv2test.FixtureSegmentCap {
			t.Fatalf("UsedBytes = %d after unlinking a segment of %d from %d", got, journalv2test.FixtureSegmentCap, used)
		}
	case renamed == 1 && removed == 0:
		if got := spareCount(t, f); got != spares+1 {
			t.Fatalf("%d spares after recycling the segment, want %d", got, spares+1)
		}

		if got := newestID(t, f); got <= newest {
			t.Fatalf("the recycled segment's id %d is not above every id the directory named, %d", got, newest)
		}

		for id, entries := range f.J.Inspect().All() {
			if id == a.id {
				t.Fatalf("the recycled segment's old record is still attached: %+v", entries)
			}
		}
	default:
		t.Fatalf("segment %s was removed %d times and renamed %d times by repack, want exactly one of the two", name, removed, renamed)
	}

	for _, s := range f.J.Inspect().Segments() {
		if s.ID == seg {
			t.Fatalf("segment %d is still among the journal's after its retirement: %+v", seg, s)
		}
	}
	wantBytes(t, f.Share(1), b.id, 0, b.p)
}
