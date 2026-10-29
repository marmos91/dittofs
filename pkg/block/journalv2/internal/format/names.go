package format

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Names of the journal directory's fixed files.
const (
	FormatFileName = "format"
	FormatTmpName  = "format.tmp"
)

// NameKind is what a file name in the journal's directory is.
type NameKind int

// String names the kinds, so a failing test reads which one it saw.
func (k NameKind) String() string {
	switch k {
	case NameUnrecognised:
		return "unrecognised"
	case NameSegment:
		return "segment"
	case NameSpare:
		return "spare"
	case NameFormat:
		return "format"
	case NameFormatTmp:
		return "format.tmp"
	}
	return "invalid"
}

const (
	// NameUnrecognised means "leave alone, never delete", not that no
	// segment-like file is there: a .seg name of the wrong width or an id
	// of zero is unrecognised too, so a directory the journal must refuse
	// for holding a segment cannot be told from this kind alone.
	NameUnrecognised NameKind = iota
	NameSegment               // <20 digits>.seg
	NameSpare                 // <20 digits>.spare.tmp
	NameFormat                // format
	NameFormatTmp             // format.tmp
)

// SegmentName returns the name of segment id's file: its id in twenty
// decimal digits, zero-padded, so names sort as ids do. The id must not be
// 0; this never panics, but the classifier refuses the name id 0 produces.
func SegmentName(id core.SegmentID) string {
	return fmt.Sprintf("%020d.seg", uint64(id))
}

// SpareName returns the name a spare is made under before its rename into
// place. The id must not be 0; this never panics, but the classifier refuses
// the name id 0 produces.
func SpareName(id core.SegmentID) string {
	return fmt.Sprintf("%020d.spare.tmp", uint64(id))
}

// ClassifyName says what a file name in the journal's directory is, and for
// a segment or a spare, the id in its name; every other kind returns 0.
// Matching is exact and case-sensitive.
//
// decision: a spare is recognised only as twenty decimal digits and
// ".spare.tmp", the width segment names use; any other name ending in
// ".spare.tmp" is unrecognised, so it is left alone rather than unlinked:
// open unlinks stray spares, but never a file whose name the journal could
// not have produced. Widen it if the format's owner rules that open unlinks
// every ".spare.tmp".
func ClassifyName(name string) (NameKind, core.SegmentID) {
	switch name {
	case FormatFileName:
		return NameFormat, 0
	case FormatTmpName:
		return NameFormatTmp, 0
	}

	for _, s := range []struct {
		suffix string
		kind   NameKind
	}{
		{".seg", NameSegment},
		{".spare.tmp", NameSpare},
	} {
		base, matched := strings.CutSuffix(name, s.suffix)
		if !matched || len(base) != 20 {
			continue
		}

		// A base-10 ParseUint takes no sign, underscore or space, so
		// the twenty bytes reach the parse unfiltered: anything else
		// is refused here, as the width check refuses what is longer
		// than a twenty-digit id.
		id, err := strconv.ParseUint(base, 10, 64)
		if err != nil || id == 0 {
			return NameUnrecognised, 0
		}
		return s.kind, core.SegmentID(id)
	}
	return NameUnrecognised, 0
}
