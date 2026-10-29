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

const (
	NameUnrecognised NameKind = iota // left alone, never deleted
	NameSegment                      // <20 digits>.seg
	NameSpare                        // <20 digits>.spare.tmp
	NameFormat                       // format
	NameFormatTmp                    // format.tmp
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

		// The digits are checked one byte at a time first, so only
		// ASCII '0'..'9' ever reaches the parse: a sign, an underscore
		// or a non-ASCII digit is unrecognised here, never a parse
		// error.
		decimal := true
		for i := range 20 {
			if base[i] < '0' || base[i] > '9' {
				decimal = false
				break
			}
		}
		if !decimal {
			return NameUnrecognised, 0
		}

		id, err := strconv.ParseUint(base, 10, 64)
		if err != nil || id == 0 {
			return NameUnrecognised, 0
		}
		return s.kind, core.SegmentID(id)
	}
	return NameUnrecognised, 0
}
