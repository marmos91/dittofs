package index

import (
	"math"
	"slices"
	"testing"
)

// FuzzBTree decodes the input as a sequence of operations, two bytes each, and
// runs them against the tree and the sorted-slice oracle. The low two bits of
// the first byte pick set, delete, floor or ascend; the rest of it and the
// second byte make a 13-bit key, which the top bit mirrors to the top of the
// key space. Thirteen bits are few enough for operations to collide and many
// enough for a tree several levels deep. Any input must leave the tree equal
// to the oracle and structurally sound.
func FuzzBTree(f *testing.F) {
	const set, del = 0, 1
	ascending := make([]byte, 0, 512)
	for i := range 256 {
		ascending = append(ascending, set, byte(i))
	}
	f.Add(ascending)
	drain := slices.Clone(ascending)
	for i := range 256 {
		drain = append(drain, del, byte(i))
	}
	f.Add(drain)
	f.Add([]byte{0x80, 0, 0, 0, 0x82, 0xff, 0x03, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		var tr btree[int]
		var o oracle
		for i := 0; i+1 < len(data); i += 2 {
			op, k := data[i]&3, uint64(data[i]&0x7c)<<6|uint64(data[i+1])
			if data[i]&0x80 != 0 {
				k = math.MaxUint64 - k
			}
			switch op {
			case 0:
				gotOld, gotRep := tr.set(k, i)
				wantOld, wantRep := o.set(k, i)
				if gotOld != wantOld || gotRep != wantRep {
					t.Fatalf("op %d set(%d) = (%d, %v), want (%d, %v)", i, k, gotOld, gotRep, wantOld, wantRep)
				}
			case 1:
				gotV, gotOK := tr.delete(k)
				wantV, wantOK := o.delete(k)
				if gotV != wantV || gotOK != wantOK {
					t.Fatalf("op %d delete(%d) = (%d, %v), want (%d, %v)", i, k, gotV, gotOK, wantV, wantOK)
				}
			case 2:
				gk, gv, gok := tr.floor(k)
				wk, wv, wok := o.floor(k)
				if gk != wk || gv != wv || gok != wok {
					t.Fatalf("op %d floor(%d) = (%d, %d, %v), want (%d, %d, %v)", i, k, gk, gv, gok, wk, wv, wok)
				}
			case 3:
				if got, want := collect(&tr, k, 8), o.ascend(k, 8); !slices.Equal(got, want) {
					t.Fatalf("op %d ascend(%d) = %v, want %v", i, k, got, want)
				}
			}
			checkInvariants(t, &tr)
		}
		if got, want := collect(&tr, 0, math.MaxInt), o.ascend(0, math.MaxInt); !slices.Equal(got, want) {
			t.Fatalf("final contents differ: %d keys, want %d", len(got), len(want))
		}
	})
}
