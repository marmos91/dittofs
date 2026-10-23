// Package ranges is lock-range arithmetic over byte ranges in 64 bits: each
// range is judged as the bytes [Offset, last], and no sum wraps. It is a pure
// leaf of open state.
package ranges

// end is the last byte a range can hold.
const end = ^uint64(0)

// Range is Length bytes from Offset. A zero Length is empty: it overlaps
// nothing. A Length of all ones, or one that carries past the last byte,
// reaches the last byte, 2^64-1.
type Range struct {
	Offset, Length uint64
}

// last is r's last byte; r is not empty.
func (r Range) last() uint64 {
	if r.Length == end {
		return end
	}
	l := r.Offset + r.Length - 1
	if l < r.Offset {
		return end
	}
	return l
}

// span is the range of the bytes [first, last]: one reaching the last byte
// is spelled with a Length of all ones.
func span(first, last uint64) Range {
	if last == end {
		return Range{first, end}
	}
	return Range{first, last - first + 1}
}

// Overlap reports whether a and b share a byte.
func Overlap(a, b Range) bool {
	return a.Length != 0 && b.Length != 0 && a.Offset <= b.last() && b.Offset <= a.last()
}

// Minus is what of a b leaves: a itself, nothing, or a piece on either side.
func Minus(a, b Range) []Range {
	if !Overlap(a, b) {
		return []Range{a}
	}
	var out []Range
	if b.Offset > a.Offset {
		out = append(out, span(a.Offset, b.Offset-1))
	}
	if bl, al := b.last(), a.last(); bl < al {
		out = append(out, span(bl+1, al))
	}
	return out
}

// Join is the one range a and b make when they overlap or touch.
func Join(a, b Range) (Range, bool) {
	if a.Length == 0 || b.Length == 0 {
		return Range{}, false
	}
	if a.Offset > b.Offset {
		a, b = b, a
	}
	al := a.last()
	if al != end && al+1 < b.Offset {
		return Range{}, false
	}
	return span(a.Offset, max(al, b.last())), true
}
