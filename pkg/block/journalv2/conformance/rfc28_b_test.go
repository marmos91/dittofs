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

// TestRFC28B_SegmentReclaimable checks that a segment holding a record
// dropped as corrupt is retired by repack and its storage recovered, so one
// flipped bit never pins a segment's storage for good. The segment was
// sealed idle, so it is truncated and accounted at its size, and its
// retirement is an unlink: a segment sealed early is never recycled (RFC 28
// section 2.2), and UsedBytes falls by its size after the truncation (RFC 1
// section 7.3).
//
// decision: a spare made while the repack runs is accounted too (RFC 1
// sections 6 and 7.3), so the drop allows one cap for each file created
// across the repack, rather than holding spare making there. The fixture
// does not wait for its spare pool, so a hold could leave the repack's copy
// refused for want of a spare on a correct journal. Each create is read as
// the moment its cap is accounted; a journal accounting a spare before the
// create it is for, with the create still to come at the last read, would
// fail here, and the hold is the upgrade if one does.
func TestRFC28B_SegmentReclaimable(t *testing.T) {
	f := journalv2test.New(t)
	a := writeSynced(t, f, 0, 0, 0, 128<<10)
	// A second live record in A's segment, so the journal does not retire
	// the segment on its own once A is dropped, before the repack.
	live := writeSynced(t, f, 0, 0, 128<<10, 64<<10)
	b := writeSynced(t, f, 1, 1, 0, 64<<10)

	seg := sealIdle(t, f, a)
	name := segmentName(seg)
	info := segmentInfo(t, f, seg)
	size := int64(len(durable(t, f, name)))
	if !info.Sealed || info.Allocated != size || size >= journalv2test.FixtureSegmentCap {
		t.Fatalf("precondition: segment %d is sealed %t, accounted at %d and %d bytes long after its idle seal, want it sealed, accounted at its size and short of its cap %d", seg, info.Sealed, info.Allocated, size, journalv2test.FixtureSegmentCap)
	}

	corruptAndRead(t, f, a)

	creates := f.Seam.Calls(journalv2test.OpCreate)
	used := f.J.Stats().UsedBytes
	_, err := f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
	stub(t, "Repack", err)
	if err != nil {
		t.Fatalf("repack the segment: %v", err)
	}

	if removed, renamed := f.Seam.CallsFor(journalv2test.OpRemove, name), f.Seam.CallsFor(journalv2test.OpRename, name); removed != 1 || renamed != 0 {
		t.Fatalf("segment %s was removed %d times and renamed %d times by repack, want unlinked once and never recycled: it was sealed early", name, removed, renamed)
	}

	if _, err := f.Seam.Durable(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s was removed, and a crash would still leave it: %v", name, err)
	}

	// The copy of the live record may take a spare and the journal make
	// another, accounted at its cap from its creation, so UsedBytes falls by
	// the segment's size less at most one cap per file created meanwhile.
	// The segment is shorter than a cap, so a repack that frees nothing
	// never lands on one of those values.
	got := f.J.Stats().UsedBytes
	made := int64(f.Seam.Calls(journalv2test.OpCreate) - creates)
	if spares := got - (used - size); spares < 0 || spares%journalv2test.FixtureSegmentCap != 0 || spares/journalv2test.FixtureSegmentCap > made {
		t.Fatalf("UsedBytes = %d after unlinking a segment accounted at %d from %d, with %d files created meanwhile, want %d plus at most one cap of %d for each", got, size, used, made, used-size, journalv2test.FixtureSegmentCap)
	}

	for _, s := range f.J.Inspect().Segments() {
		if s.ID == seg {
			t.Fatalf("segment %d is still among the journal's after its retirement: %+v", seg, s)
		}
	}

	wantBytes(t, f.Share(0), live.id, live.extent.Off, live.p)
	wantBytes(t, f.Share(1), b.id, 0, b.p)
}
