package format

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"

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
