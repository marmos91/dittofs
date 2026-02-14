// Package attrs provides NFSv4 attribute bitmap helpers and FATTR4 encoding.
//
// NFSv4 uses variable-length bitmaps (bitmap4) to represent sets of file
// attributes. This package provides helpers for encoding, decoding, and
// manipulating these bitmaps, as well as encoding attribute values for
// pseudo-filesystem nodes.
//
// Per RFC 7530/7531: typedef uint32_t bitmap4<>;
package attrs

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// Bitmap4 Encode/Decode
// ============================================================================

// EncodeBitmap4 encodes a variable-length bitmap in XDR format.
//
// Format: [numWords:uint32][word0:uint32][word1:uint32]...
//
// An empty bitmap is encoded as a single uint32 with value 0 (zero words).
func EncodeBitmap4(buf *bytes.Buffer, bitmap []uint32) error {
	// Write number of words
	if err := xdr.WriteUint32(buf, uint32(len(bitmap))); err != nil {
		return fmt.Errorf("encode bitmap4 length: %w", err)
	}

	// Write each word
	for i, word := range bitmap {
		if err := xdr.WriteUint32(buf, word); err != nil {
			return fmt.Errorf("encode bitmap4 word %d: %w", i, err)
		}
	}

	return nil
}

// DecodeBitmap4 decodes a variable-length bitmap from XDR format.
//
// Reads the word count first, then each uint32 word. Rejects bitmaps
// with more than 8 words to prevent memory exhaustion from malicious input.
func DecodeBitmap4(reader io.Reader) ([]uint32, error) {
	numWords, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, fmt.Errorf("decode bitmap4 length: %w", err)
	}

	// Reject unreasonably large bitmaps (8 words = 256 attribute bits)
	if numWords > 8 {
		return nil, fmt.Errorf("bitmap4 too large: %d words (max 8)", numWords)
	}

	bitmap := make([]uint32, numWords)
	for i := uint32(0); i < numWords; i++ {
		bitmap[i], err = xdr.DecodeUint32(reader)
		if err != nil {
			return nil, fmt.Errorf("decode bitmap4 word %d: %w", i, err)
		}
	}

	return bitmap, nil
}

// ============================================================================
// Bitmap4 Bit Manipulation
// ============================================================================

// IsBitSet checks if a specific bit is set in the bitmap.
//
// Bits are numbered starting at 0. Bit N is in word N/32, at position N%32.
// Returns false if the word index exceeds the bitmap length (graceful handling).
func IsBitSet(bitmap []uint32, bit uint32) bool {
	word := bit / 32
	if word >= uint32(len(bitmap)) {
		return false
	}
	return bitmap[word]&(1<<(bit%32)) != 0
}

// SetBit sets a specific bit in the bitmap, extending the slice if needed.
//
// Takes a pointer to the slice so it can grow. If bit N requires a word
// beyond the current length, zero-valued words are appended.
func SetBit(bitmap *[]uint32, bit uint32) {
	word := bit / 32

	// Extend slice if needed
	for uint32(len(*bitmap)) <= word {
		*bitmap = append(*bitmap, 0)
	}

	(*bitmap)[word] |= 1 << (bit % 32)
}

// ClearBit clears a specific bit in the bitmap.
//
// No-op if the word index exceeds the bitmap length.
func ClearBit(bitmap []uint32, bit uint32) {
	word := bit / 32
	if word >= uint32(len(bitmap)) {
		return
	}
	bitmap[word] &^= 1 << (bit % 32)
}

// Intersect returns the bitwise AND of two bitmaps.
//
// The result length is the minimum of both input lengths, since any bits
// beyond the shorter bitmap are implicitly zero.
func Intersect(request, supported []uint32) []uint32 {
	minLen := len(request)
	if len(supported) < minLen {
		minLen = len(supported)
	}

	result := make([]uint32, minLen)
	for i := 0; i < minLen; i++ {
		result[i] = request[i] & supported[i]
	}

	return result
}
