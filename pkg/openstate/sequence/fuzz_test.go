package sequence

import "testing"

// Check and Answer move an owner's slot as their doc comments say, for any
// number the slot holds and any the request carries, across the wrap from
// 2^32-1 to 0.
func FuzzSlot(f *testing.F) {
	for _, s := range []struct {
		prior, n uint32
		slot     bool
	}{{0, 0, true}, {^uint32(0), ^uint32(0), true}, {^uint32(0), 0, true}, {5, 4, true}, {5, 6, true}, {7, 1, false}, {^uint32(0), ^uint32(0), false}} {
		for outcome := range uint8(4) {
			f.Add(s.prior, s.slot, false, uint8(Opens), s.n, outcome)
			f.Add(s.prior, s.slot, true, uint8(Locks), s.n, outcome)
		}
	}
	f.Fuzz(func(t *testing.T, prior uint32, slot, confirmed bool, kind uint8, n uint32, outcome uint8) {
		o := Owner{Client: 1, Kind: Kind(kind%2 + 1), Name: "o"}
		b := Book{}
		if slot {
			b[o] = Slot{Next: prior, Confirmed: confirmed}
		}
		if got, want := b.Unconfirmed(o), slot && !confirmed; got != want {
			t.Fatalf("Unconfirmed = %v, want %v", got, want)
		}
		admitted := !slot || prior == n
		if b.Check(o, n) != admitted {
			t.Fatalf("Check(%d) with slot %v at %d = %v", n, slot, prior, !admitted)
		}
		if !admitted {
			return
		}
		a := Outcome(outcome%3 + 1)
		b.Answer(o, n, a)
		s, ok := b[o]
		var want Slot
		wantOK := slot
		switch {
		case a == Exempt && slot, slot && a != Exempt && prior != n:
			want = Slot{prior, confirmed}
		case a == Exempt:
		case !slot && a == Granted:
			want, wantOK = Slot{Next: n + 1, Confirmed: o.Kind != Opens}, true
		case !slot:
		default:
			want = Slot{Next: n + 1, Confirmed: confirmed} // past 2^32-1 is 0
		}
		if ok != wantOK || ok && s != want {
			t.Fatalf("slot %v at %d (confirmed %v), %v of %d: %+v %v, want %+v %v", slot, prior, confirmed, a, n, s, ok, want, wantOK)
		}
	})
}
