package format

import (
	"fmt"
	"math"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestSegmentNameRoundTrips pins the name of a segment and of a spare at both
// ends of the id range: what the journal writes, the classifier reads back
// with the same id.
func TestSegmentNameRoundTrips(t *testing.T) {
	for _, id := range []struct {
		name string
		id   core.SegmentID
	}{
		{"one", 1},
		{"max", math.MaxUint64},
	} {
		if got := SegmentName(id.id); got != wantSegmentName(id.id, ".seg") {
			t.Errorf("SegmentName(%s) = %q", id.name, got)
		}
		if kind, got := ClassifyName(SegmentName(id.id)); kind != NameSegment || got != id.id {
			t.Errorf("ClassifyName(SegmentName(%s)) = (%v, %d)", id.name, kind, got)
		}
		if kind, got := ClassifyName(SpareName(id.id)); kind != NameSpare || got != id.id {
			t.Errorf("ClassifyName(SpareName(%s)) = (%v, %d)", id.name, kind, got)
		}
	}

	if got := SegmentName(1); got != "00000000000000000001.seg" {
		t.Errorf("SegmentName(1) = %q", got)
	}
	if got := SpareName(math.MaxUint64); got != "18446744073709551615.spare.tmp" {
		t.Errorf("SpareName(max) = %q", got)
	}
}

// wantSegmentName formats the id as the format's names do, twenty decimal
// digits zero-padded, so a test pins the width without calling the code under
// test to build its own want.
func wantSegmentName(id core.SegmentID, ext string) string {
	return fmt.Sprintf("%020d%s", id, ext)
}

// TestSegmentNameSortsAsItsIdDoes pins the zero-padding: segment names order
// as strings exactly as their ids do as numbers.
func TestSegmentNameSortsAsItsIdDoes(t *testing.T) {
	if SegmentName(9) >= SegmentName(10) {
		t.Errorf("SegmentName(9) = %q sorts after SegmentName(10) = %q", SegmentName(9), SegmentName(10))
	}
}

// TestClassifyNameReadsTheFixedFiles pins the two names the journal owns
// besides its segments.
func TestClassifyNameReadsTheFixedFiles(t *testing.T) {
	for name, want := range map[string]NameKind{
		FormatFileName: NameFormat,
		FormatTmpName:  NameFormatTmp,
	} {
		if kind, id := ClassifyName(name); kind != want || id != 0 {
			t.Errorf("ClassifyName(%q) = (%v, %d), want (%v, 0)", name, kind, id, want)
		}
	}
}

// TestClassifyNameRefusesANameItCouldNotHaveProduced pins the width and the
// digits: anything but twenty decimal digits and the exact extension is
// unrecognised, so open leaves it alone rather than unlinks it.
func TestClassifyNameRefusesANameItCouldNotHaveProduced(t *testing.T) {
	for _, name := range []string{
		"00000000000000000000.seg", // id 0
		"00000000000000000000.spare.tmp",
		"0000000000000000001.seg", // 19 digits
		"0000000000000000001.spare.tmp",
		"000000000000000000001.seg", // 21 digits
		"+0000000000000000001.seg",  // a sign
		"-0000000000000000001.seg",
		"18446744073709551616.seg", // overflows uint64
		"99999999999999999999.seg",
		"0000000000000000000a.seg",     // not a digit
		"00000000000000000001.SEG",     // case matters
		"00000000000000000001.seg.tmp", // a suffix too many
		"00000000000000000001.tmp",
		"x.spare.tmp",
		"1.spare.tmp",
		".spare.tmp",
		".seg",
		"",
		"format.tmp.1",
		"Format",
		"lost+found",
	} {
		if kind, id := ClassifyName(name); kind != NameUnrecognised || id != 0 {
			t.Errorf("ClassifyName(%q) = (%v, %d), want (NameUnrecognised, 0)", name, kind, id)
		}
	}
}
