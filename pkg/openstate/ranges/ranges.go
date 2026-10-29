// Package ranges is lock-range arithmetic over byte ranges in 64 bits: each
// range is judged as the bytes [Offset, last], and no sum wraps. It is a pure
// leaf of open state.
package ranges

// end is the last byte a range can hold.
const end = ^uint64(0)

// Range is Length bytes from Offset, the bytes [Offset, Offset+Length-1]. A
// Length of all ones, or one that carries past the last byte, reaches the
// last byte, 2^64-1. A zero Length holds no byte but still ends, at
// Offset-1, as MS-FSA has a zero-length lock: one at X overlaps a range
// holding both X-1 and X, and one at 0 overlaps nothing.
type Range struct {
	Offset, Length uint64
}

// last is r's last byte; ok is false for a zero-length range at 0, which
// ends before the first byte.
func (r Range) last() (l uint64, ok bool) {
	switch r.Length {
	case end:
		return end, true
	case 0:
		return r.Offset - 1, r.Offset != 0
	}
	l = r.Offset + r.Length - 1
	if l < r.Offset {
		return end, true
	}
	return l, true
}

// span is the range of the bytes [first, last]: one reaching the last byte
// is spelled with a Length of all ones.
func span(first, last uint64) Range {
	if last == end {
		return Range{first, end}
	}
	return Range{first, last - first + 1}
}

// Overlap reports whether a and b overlap: share a byte, or, for a
// zero-length range, span the point where it ends.
func Overlap(a, b Range) bool {
	al, aok := a.last()
	bl, bok := b.last()
	return aok && bok && a.Offset <= bl && b.Offset <= al
}

// Minus is what of a b leaves: a itself, nothing, or a piece on either side.
// A zero-length range holds no byte, so it cuts nothing, and only the same
// zero-length range takes it away.
func Minus(a, b Range) []Range {
	if a.Length == 0 || b.Length == 0 {
		if a == b {
			return nil
		}
		return []Range{a}
	}
	if !Overlap(a, b) {
		return []Range{a}
	}
	var out []Range
	if b.Offset > a.Offset {
		out = append(out, span(a.Offset, b.Offset-1))
	}
	if bl, al := b.mustLast(), a.mustLast(); bl < al {
		out = append(out, span(bl+1, al))
	}
	return out
}

// mustLast is the last byte of r, which is not zero-length.
func (r Range) mustLast() uint64 {
	l, _ := r.last()
	return l
}

// Join is the one range a and b make when they overlap or touch.
func Join(a, b Range) (Range, bool) {
	if a.Length == 0 || b.Length == 0 {
		return Range{}, false
	}
	if a.Offset > b.Offset {
		a, b = b, a
	}
	al := a.mustLast()
	if al != end && al+1 < b.Offset {
		return Range{}, false
	}
	return span(a.Offset, max(al, b.mustLast())), true
}
