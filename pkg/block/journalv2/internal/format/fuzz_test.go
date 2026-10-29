package format

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// FuzzFormatFile drives the decoder with any bytes. Whatever arrives, it
// either decodes or matches exactly one outcome class, never a panic; and
// whatever decodes is exactly what the bytes contain, so encoding it again
// reproduces the input with the generation zeroed.
func FuzzFormatFile(f *testing.F) {
	golden := readGolden(f, "format_v1")

	f.Add(golden)
	f.Add(slices.Clone(golden[:55]))
	f.Add(slices.Clone(golden[:5]))
	f.Add(append(slices.Clone(golden), 0x00))
	f.Add(raw.Format{Version: 2, Identity: testIdentity, Installation: testInstallation}.Bytes())

	generation := slices.Clone(golden)
	copy(generation[40:48], []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48})
	rechecksum(generation)
	f.Add(generation)

	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, in []byte) {
		file, err := DecodeFormatFile(in)

		if err == nil {
			if len(in) != FormatFileSize {
				t.Fatalf("decoded %d bytes, want %d", len(in), FormatFileSize)
			}
			if got, want := binary.LittleEndian.Uint32(in[52:56]), wantCRC(in[0:52]); got != want {
				t.Fatalf("decoded bytes whose checksum is 0x%08x, want the CRC32C of [0,52): 0x%08x", got, want)
			}

			// The decoder must yield only what the bytes contain: the
			// input again, with the never-read generation zeroed and
			// the checksum recomputed for it.
			want := slices.Clone(in)
			clear(want[40:48]) // the generation is never read
			rechecksum(want)
			if got := AppendFormatFile(nil, file); !slices.Equal(got, want) {
				t.Fatalf("AppendFormatFile =\n% x\nwant the input with a zero generation\n% x", got, want)
			}
			return
		}

		damaged := errors.Is(err, ErrDamaged)
		version := errors.Is(err, ErrUnknownVersion)
		if damaged == version {
			t.Fatalf("DecodeFormatFile = %v, want an error matching exactly one of ErrDamaged and ErrUnknownVersion", err)
		}
	})
}

// FuzzSegmentHeader drives the decoder with any bytes. Whatever arrives, it
// either decodes or matches exactly one outcome class, never a panic; and
// whatever decodes holds exactly the bytes' fields: on a clean decode
// encoding it again reproduces the input, and on a foreign one the header is
// returned filled — its identity, id, predecessor and stream match the
// input's bytes, though its reserved bytes, which the header has no field
// for, are never reproduced.
func FuzzSegmentHeader(f *testing.F) {
	pred := readGolden(f, "segheader_pred")
	nopred := readGolden(f, "segheader_nopred")

	f.Add(pred)
	f.Add(nopred)
	f.Add(slices.Clone(pred[:47]))
	f.Add(append(slices.Clone(pred), 0x00))

	v2 := slices.Clone(pred)
	binary.LittleEndian.PutUint16(v2[4:6], 2)
	f.Add(v2)

	other := testSegmentHeader
	other.Identity = otherIdentity
	f.Add(AppendSegmentHeader(nil, other))

	// A foreign header whose reserved bytes are not zero: its checksum
	// verifies, its identity is another journal's, and its reserved bytes
	// are never judged here, so it must be told apart as foreign with the
	// fields its bytes carry.
	foreignReserved := AppendSegmentHeader(nil, other)
	copy(foreignReserved[6:8], []byte{0xff, 0xff})
	rechecksumHeader(foreignReserved)
	f.Add(foreignReserved)

	f.Add([]byte{})
	f.Add(make([]byte, SegmentHeaderSize))

	f.Fuzz(func(t *testing.T, in []byte) {
		h, err := DecodeSegmentHeader(in, testIdentity)

		if err == nil {
			if len(in) != SegmentHeaderSize {
				t.Fatalf("decoded %d bytes, want %d", len(in), SegmentHeaderSize)
			}
			if got, want := binary.LittleEndian.Uint32(in[44:48]), wantCRC(in[0:44]); got != want {
				t.Fatalf("decoded bytes whose checksum is 0x%08x, want the CRC32C of [0,44): 0x%08x", got, want)
			}
			if got := AppendSegmentHeader(nil, h); !slices.Equal(got, in) {
				t.Fatalf("AppendSegmentHeader =\n% x\nwant the input\n% x", got, in)
			}
			return
		}

		if errors.Is(err, ErrForeign) {
			// A foreign header is returned filled, so recovery can report
			// whose it is: every field must say what the bytes hold. The
			// reserved bytes are not reproduced — the header has no field
			// for them, and another journal's are not this one's to read.
			if h.Identity != core.JournalID(in[8:24]) {
				t.Fatalf("a foreign header returned the identity % x, want the input's % x", h.Identity, in[8:24])
			}
			if h.ID != core.SegmentID(binary.LittleEndian.Uint64(in[24:32])) {
				t.Fatalf("a foreign header returned segment id %d, want the input's %d", h.ID, core.SegmentID(binary.LittleEndian.Uint64(in[24:32])))
			}
			if h.Predecessor != core.SegmentID(binary.LittleEndian.Uint64(in[32:40])) {
				t.Fatalf("a foreign header returned predecessor %d, want the input's %d", h.Predecessor, core.SegmentID(binary.LittleEndian.Uint64(in[32:40])))
			}
			if h.Stream != binary.LittleEndian.Uint32(in[40:44]) {
				t.Fatalf("a foreign header returned stream %d, want the input's %d", h.Stream, binary.LittleEndian.Uint32(in[40:44]))
			}
		}

		matched := 0
		for _, class := range []error{ErrDamaged, ErrUnknownVersion, ErrForeign} {
			if errors.Is(err, class) {
				matched++
			}
		}
		if matched != 1 {
			t.Fatalf("DecodeSegmentHeader = %v, want an error matching exactly one of ErrDamaged, ErrUnknownVersion and ErrForeign", err)
		}
	})
}
