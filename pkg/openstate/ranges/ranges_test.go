package ranges

import (
	"slices"
	"testing"
)

// Ranges near the last byte saturate rather than wrap, a Length of all ones
// reaches the last byte from any offset, an empty range overlaps nothing, and
// ranges that only touch do not overlap.
func TestOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b Range
		want bool
	}{
		{Range{end - 1, 10}, Range{end, 1}, true},
		{Range{0, end}, Range{end, 1}, true},
		{Range{5, 0}, Range{0, 10}, false},
		{Range{0, 10}, Range{10, 5}, false},
	} {
		if got := Overlap(c.a, c.b); got != c.want {
			t.Errorf("Overlap(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Taking a range out of another leaves the pieces on either side, a piece
// reaching the last byte keeps reaching it, and a cut reaching the last byte
// leaves nothing after it.
func TestMinus(t *testing.T) {
	for _, c := range []struct {
		a, b Range
		want []Range
	}{
		{Range{0, 30}, Range{10, 10}, []Range{{0, 10}, {20, 10}}},
		{Range{0, 10}, Range{0, 5}, []Range{{5, 5}}},
		{Range{0, end}, Range{10, 10}, []Range{{0, 10}, {20, end}}},
		{Range{0, end}, Range{10, end}, []Range{{0, 10}}},
		{Range{0, 10}, Range{20, 5}, []Range{{0, 10}}},
	} {
		if got := Minus(c.a, c.b); !slices.Equal(got, c.want) {
			t.Errorf("Minus(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Ranges that overlap or touch join into one, a range reaching the last byte
// joins what it covers, and a gap or an empty range joins nothing.
func TestJoin(t *testing.T) {
	for _, c := range []struct {
		a, b Range
		want Range
		ok   bool
	}{
		{Range{10, 5}, Range{0, 10}, Range{0, 15}, true},
		{Range{0, end}, Range{5, 1}, Range{0, end}, true},
		{Range{0, 10}, Range{11, 5}, Range{}, false},
		{Range{5, 0}, Range{0, 10}, Range{}, false},
	} {
		if got, ok := Join(c.a, c.b); got != c.want || ok != c.ok {
			t.Errorf("Join(%v, %v) = %v, %v; want %v, %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
