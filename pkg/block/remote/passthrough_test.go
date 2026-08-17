package remote

import (
	"errors"
	"math"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestSliceRangeHugeLengthDoesNotPanic covers the arithmetic rather than the
// contract: a length near MaxInt64 makes offset+length wrap negative, and sizing
// the copy from that wrapped value panics before any error can be returned. A
// non-zero offset is required — at offset 0 the sum still fits.
func TestSliceRangeHugeLengthDoesNotPanic(t *testing.T) {
	full := []byte("0123456789")
	for _, tc := range []struct {
		name           string
		offset, length int64
		want           string
	}{
		{"max length at nonzero offset", 1, math.MaxInt64, "123456789"},
		{"max length at zero offset", 0, math.MaxInt64, "0123456789"},
		{"max length at final offset", int64(len(full)), math.MaxInt64, ""},
		{"length past end clamps", 6, 100, "6789"},
		{"exact tail", 6, 4, "6789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SliceRange(full, tc.offset, tc.length)
			if err != nil {
				t.Fatalf("SliceRange(%d, %d): %v", tc.offset, tc.length, err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSliceRangeRejectsInvalidInput keeps the two guards ahead of the copy.
func TestSliceRangeRejectsInvalidInput(t *testing.T) {
	full := []byte("0123456789")
	for _, length := range []int64{0, -1, math.MinInt64} {
		if _, err := SliceRange(full, 0, length); !errors.Is(err, block.ErrInvalidSize) {
			t.Errorf("length=%d: got %v, want wraps ErrInvalidSize", length, err)
		}
	}
	for _, offset := range []int64{-1, int64(len(full)) + 1} {
		if _, err := SliceRange(full, offset, 4); !errors.Is(err, block.ErrInvalidOffset) {
			t.Errorf("offset=%d: got %v, want wraps ErrInvalidOffset", offset, err)
		}
	}
}
