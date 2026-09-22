package compression

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// FrameMagic is the 5-byte prefix every framed block starts with.
var FrameMagic = [5]byte{'D', 'F', 'C', 'M', 'P'}

// FrameHeaderFixedSize is the byte count of the fixed-width portion of
// the frame header (magic + algo). The orig_size uvarint that follows
// occupies 1..10 additional bytes.
const FrameHeaderFixedSize = len(FrameMagic) + 1

// maxOrigSizeVarint is the worst-case byte count of an encoded uvarint.
const maxOrigSizeVarint = binary.MaxVarintLen64

// MaxFramedPlaintextSize caps the declared plaintext size a frame
// header is allowed to claim. Guards against decompression-bomb /
// huge-preallocation attacks via a corrupt wire header. The bound is
// generous vs FastCDC's ~16 MiB max chunk; tune if larger chunks land.
const MaxFramedPlaintextSize = 64 * 1024 * 1024

// appendFrameHeader appends the header of a frame declaring origSize plaintext
// bytes; the compressed body follows it directly on the wire:
//
//	[magic | algo | uvarint(origSize) | body]
//
// Appending lets the caller reserve the header in the same buffer the codec
// streams the body into, so no second allocate-and-copy is needed.
func appendFrameHeader(out []byte, algo Algo, origSize uint64) []byte {
	out = append(out, FrameMagic[:]...)
	out = append(out, byte(algo))
	var sizeBuf [maxOrigSizeVarint]byte
	n := binary.PutUvarint(sizeBuf[:], origSize)
	return append(out, sizeBuf[:n]...)
}

// hasFrameMagic reports whether b begins with the 5-byte DFCMP prefix.
// It does NOT validate the algo byte or the uvarint; callers that need
// a full parse use tryDecodeFrame.
func hasFrameMagic(b []byte) bool {
	return len(b) >= FrameHeaderFixedSize && bytes.Equal(b[:len(FrameMagic)], FrameMagic[:])
}

// tryDecodeFrame parses b and returns the framed payload metadata. When
// b does not carry the DFCMP magic, framed is false and (algo, origSize
// body) are zero-valued — callers treat b as raw plaintext in that
// case.
//
// When the magic is present but the rest of the header is malformed
// (unknown algo byte, missing or truncated uvarint), an error wrapping
// ErrCompressedFrameCorrupt or ErrUnsupportedCompressionAlgo is
// returned.
func tryDecodeFrame(b []byte) (algo Algo, origSize uint64, body []byte, framed bool, err error) {
	if !hasFrameMagic(b) {
		return 0, 0, nil, false, nil
	}
	algoByte := b[len(FrameMagic)]
	switch Algo(algoByte) {
	case AlgoZstd, AlgoLZ4:
		algo = Algo(algoByte)
	default:
		return 0, 0, nil, true, fmt.Errorf("%w: byte 0x%02x", ErrUnsupportedCompressionAlgo, algoByte)
	}
	rest := b[FrameHeaderFixedSize:]
	size, n := binary.Uvarint(rest)
	if n <= 0 {
		return 0, 0, nil, true, fmt.Errorf("%w: bad orig_size uvarint", ErrCompressedFrameCorrupt)
	}
	body = rest[n:]
	return algo, size, body, true, nil
}
