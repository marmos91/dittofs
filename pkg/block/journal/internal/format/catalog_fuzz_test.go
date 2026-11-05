package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// FuzzFooter drives the footer reader with any bytes read as any segment's.
// Whatever arrives, it either reads or matches ErrNoFooter, never a panic —
// a bytes.Reader within bounds never errors, so any other error fails here;
// and whatever reads holds exactly the bytes' fields: re-encoding the
// entries, the seal marker and the trailer it names reproduces the input's
// own bytes, the entries strictly ascend, and the last record ends at the
// seal marker.
func FuzzFooter(f *testing.F) {
	golden := sealedSegment(f, []CatalogEntry{testEntryA, testEntryB}, 0)

	f.Add(golden, uint64(testSegment))
	f.Add(golden, uint64(testSegment)-1)

	// Then its foils, each a real footer's neighbour: a truncation and a
	// one-byte extension; a later layout version, non-zero flags, a
	// zero-entry footer and a full seal's gap.
	f.Add(slices.Clone(golden[:len(golden)-1]), uint64(testSegment))
	f.Add(append(slices.Clone(golden), 0x00), uint64(testSegment))

	v2 := slices.Clone(golden)
	binary.LittleEndian.PutUint16(v2[len(v2)-TrailerSize+4:len(v2)-TrailerSize+6], 2)
	rechecksumFooter(v2)
	f.Add(v2, uint64(testSegment))

	flags := slices.Clone(golden)
	binary.LittleEndian.PutUint16(flags[len(flags)-TrailerSize+6:len(flags)-TrailerSize+8], 1)
	rechecksumFooter(flags)
	f.Add(flags, uint64(testSegment))

	zeroEntry := sealedSegment(f, nil, 0)
	f.Add(zeroEntry, uint64(testSegment))

	fullSeal := sealedSegment(f, []CatalogEntry{testEntryA}, 4000)
	f.Add(fullSeal, uint64(testSegment))

	f.Add([]byte(nil), uint64(testSegment))
	f.Add(make([]byte, TrailerSize), uint64(testSegment))

	f.Fuzz(func(t *testing.T, in []byte, segment uint64) {
		footer, err := ReadFooter(bytes.NewReader(in), int64(len(in)), testIdentity, core.SegmentID(segment))

		if err != nil {
			if !errors.Is(err, ErrNoFooter) {
				t.Fatalf("ReadFooter = %v, want ErrNoFooter: a reader within bounds never errors", err)
			}
			if footer.Offset != 0 || len(footer.Entries) != 0 {
				t.Fatalf("ReadFooter = %v with the footer %+v, want the zero footer on refusal", err, footer)
			}
			return
		}

		// What reads was written by hand from this layout: re-encoding
		// the footer's parts reproduces the input's own bytes, and the
		// rules the reader holds are seen to hold one more time.
		marker := AppendSealMarker(nil, testIdentity, SealMarker{Segment: core.SegmentID(segment), Offset: footer.Offset - SealMarkerSize})
		if !slices.Equal(marker, in[footer.Offset-SealMarkerSize:footer.Offset]) {
			t.Fatalf("the input's seal marker by hand =\n% x\nwant the input's own\n% x", marker, in[footer.Offset-SealMarkerSize:footer.Offset])
		}

		entries := []byte{}
		for i, e := range footer.Entries {
			if e.RecordOffset%8 != 0 {
				t.Fatalf("decoded a record offset %d that is not a multiple of 8", e.RecordOffset)
			}
			if i > 0 && e.RecordOffset <= footer.Entries[i-1].RecordOffset {
				t.Fatalf("entries do not ascend: %d after %d", e.RecordOffset, footer.Entries[i-1].RecordOffset)
			}
			entries = AppendCatalogEntry(entries, e)
		}
		if !slices.Equal(entries, in[footer.Offset:footer.Offset+int64(len(footer.Entries))*CatalogEntrySize]) {
			t.Fatalf("re-encoded entries =\n% x\nwant the input's own\n% x", entries, in[footer.Offset:footer.Offset+int64(len(footer.Entries))*CatalogEntrySize])
		}

		trailer := AppendTrailer(nil, footer.Offset, entries)
		if !slices.Equal(trailer, in[len(in)-TrailerSize:]) {
			t.Fatalf("AppendTrailer =\n% x\nwant the input's own trailer\n% x", trailer, in[len(in)-TrailerSize:])
		}

		// The extent check the reader holds, seen to hold: the last
		// record ends exactly at the seal marker.
		end := int64(SegmentHeaderSize)
		if len(footer.Entries) > 0 {
			last := footer.Entries[len(footer.Entries)-1]
			end = last.RecordOffset + RecordHeader{Kind: last.Kind, Extent: core.Extent{Len: last.Extent.Len}}.Size()
		}
		if end != footer.Offset-SealMarkerSize {
			t.Fatalf("the records end at %d, want the seal marker's %d", end, footer.Offset-SealMarkerSize)
		}
	})
}
