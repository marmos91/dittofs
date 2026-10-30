package ranges

import "testing"

// interval is a range as the bytes it holds, [first, last], judged by
// comparisons alone: a sum past the last byte never wraps here. zero marks a
// zero-length range, which holds no byte and ends at Offset-1.
type interval struct {
	first, last uint64
	zero, ok    bool
}

// whole is the whole space, spelled as [0, 2^64-2] is too: span's decision.
var whole = Range{0, end}

func bytesOf(r Range) interval {
	if r.Length == 0 {
		return interval{first: r.Offset, last: r.Offset - 1, zero: true, ok: r.Offset != 0}
	}
	if r.Length == end || r.Length-1 > end-r.Offset {
		return interval{first: r.Offset, last: end, ok: true}
	}
	return interval{first: r.Offset, last: r.Offset + (r.Length - 1), ok: true}
}

// Overlap, Minus and Join agree with the bytes each range holds, at the top
// of the 64-bit space too.
func FuzzRanges(f *testing.F) {
	for _, s := range [][4]uint64{
		{0, 10, 5, 10}, {0, 10, 10, 5}, {0, end, end, 1}, {end - 1, 10, end, 1},
		{end, 1, end, 1}, {end - 5, end, 0, end}, {15, 0, 10, 10}, {0, 0, 0, end},
		{end, 0, end - 1, end}, {5, end - 4, 3, 4}, {1, end, 0, 1}, {1, end - 1, 0, 1},
	} {
		f.Add(s[0], s[1], s[2], s[3])
	}
	f.Fuzz(func(t *testing.T, off1, len1, off2, len2 uint64) {
		a, b := Range{off1, len1}, Range{off2, len2}
		ia, ib := bytesOf(a), bytesOf(b)
		// Two ranges overlap when they share a byte; a zero-length one at X
		// overlaps a range holding both X-1 and X, and no zero-length one.
		holds := func(i interval, x uint64) bool { return !i.zero && i.first <= x && x <= i.last }
		var overlap bool
		switch {
		case ia.zero && ib.zero:
		case ia.zero:
			overlap = ia.ok && holds(ib, a.Offset-1) && holds(ib, a.Offset)
		case ib.zero:
			overlap = ib.ok && holds(ia, b.Offset-1) && holds(ia, b.Offset)
		default:
			overlap = max(ia.first, ib.first) <= min(ia.last, ib.last)
		}
		if got := Overlap(a, b); got != overlap {
			t.Fatalf("Overlap(%v, %v) = %v, want %v", a, b, got, overlap)
		}

		pieces := Minus(a, b)
		if ia.zero || ib.zero {
			if want := a != b; (len(pieces) == 1 && pieces[0] == a) != want || !want && len(pieces) != 0 {
				t.Fatalf("Minus(%v, %v) = %v of a zero-length range", a, b, pieces)
			}
		} else {
			var held uint64 // bytes of a the pieces hold
			for i, p := range pieces {
				ip := bytesOf(p)
				if p == whole && ib.first == end {
					ip.last = end - 1
				}
				if ip.zero || ip.first < ia.first || ip.last > ia.last {
					t.Fatalf("Minus(%v, %v) piece %v is not inside a", a, b, p)
				}
				if max(ip.first, ib.first) <= min(ip.last, ib.last) {
					t.Fatalf("Minus(%v, %v) piece %v meets b", a, b, p)
				}
				if i > 0 && bytesOf(pieces[i-1]).last >= ip.first {
					t.Fatalf("Minus(%v, %v) pieces %v overlap or are out of order", a, b, pieces)
				}
				held += ip.last - ip.first + 1
			}
			// a's bytes are those b covers and those the pieces hold.
			lo, hi := max(ia.first, ib.first), min(ia.last, ib.last)
			cut := uint64(0)
			if lo <= hi {
				cut = hi - lo + 1
			}
			if held+cut != ia.last-ia.first+1 {
				t.Fatalf("Minus(%v, %v) = %v holds %d bytes, want %d", a, b, pieces, held, ia.last-ia.first+1-cut)
			}
		}

		j, ok := Join(a, b)
		touch := !ia.zero && !ib.zero &&
			(overlap || ia.last != end && ia.last+1 == ib.first || ib.last != end && ib.last+1 == ia.first)
		if ok != touch {
			t.Fatalf("Join(%v, %v) ok %v, want %v", a, b, ok, touch)
		}
		ij, wantLast := bytesOf(j), max(ia.last, ib.last)
		if j == whole && wantLast == end-1 {
			ij.last = wantLast
		}
		if ok && (ij.first != min(ia.first, ib.first) || ij.last != wantLast) {
			t.Fatalf("Join(%v, %v) = %v", a, b, j)
		}
	})
}
