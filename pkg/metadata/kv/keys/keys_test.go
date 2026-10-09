package keys

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestGolden(t *testing.T) {
	share := bytes.Repeat([]byte{0xAA}, IDLen)
	file := bytes.Repeat([]byte{0xBB}, IDLen)
	node := bytes.Repeat([]byte{0xCC}, IDLen)
	f := ID(ID([]byte{KindF}, share), file)

	for _, c := range []struct {
		name string
		key  []byte
		want string
	}{
		{"format", Format, "00666f726d6174"},
		{"uint64", Uint64(nil, 0x0102030405060708), "0102030405060708"},
		{"string", String(nil, []byte("a\x00b")), "6100ff620001"},
		{"empty string", String(nil, nil), "0001"},
		{"File", f, "01" + hex.EncodeToString(share) + hex.EncodeToString(file)},
		{"F‖id‖e‖digest‖key", String(Uint64(append(bytes.Clone(f), FE), 7), []byte("x")),
			"01" + hex.EncodeToString(share) + hex.EncodeToString(file) + "04" + "0000000000000007" + "780001"},
		{"F‖id‖ref‖offset", Uint64(append(bytes.Clone(f), FRef), 4096),
			"01" + hex.EncodeToString(share) + hex.EncodeToString(file) + "09" + "0000000000001000"},
		{"N‖node‖clk", append(ID([]byte{KindN}, node), NClk), "2c" + hex.EncodeToString(node) + "02"},
		{"NX‖share‖name", String([]byte{KindNX, NXShare}, []byte("data")), "2503646174610001"},
	} {
		if got := hex.EncodeToString(c.key); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// Every kind code is distinct, and so is every sub-kind among its siblings: a
// code given twice would merge two record kinds under one prefix.
func TestCodesUnique(t *testing.T) {
	for name, codes := range map[string][]byte{
		"kind": {KindStore, KindF, KindS, KindC, KindCR, KindB, KindBR, KindBD, KindBC, KindI, KindNS,
			KindU, KindG, KindM, KindMR, KindGR, KindNX, KindNP, KindNG, KindSL, KindIN, KindNSM, KindPX,
			KindN, KindSH, KindJ, KindRS, KindMV, KindSLOT, KindCFG, KindCFGGEN, KindSEC, KindCL, KindCLO},
		"F": {FAcl, FX, FS, FE, FT, FH, FRm, FOv, FRef, FHis, FFx, FFo, FRel, FOp, FDp},
		"S": {SInfo, SSt, SNsg, SG, SXp, SSnap, SCut, SLive, SSc, SHd, SHr, SUse, SRh, SOr, SUt, SPu,
			SPj, SUd, SQt, SQx, SVf, SFnc},
		"NS": {NSGc, NSClaim, NSPk, NSKey, NSBk},
		"gc": {GcLease, GcRecheck, GcHold, GcSuspect, GcForward, GcPause, GcCursor},
	} {
		seen := map[byte]bool{}
		for _, c := range codes {
			if seen[c] {
				t.Errorf("%s: code %#x assigned twice", name, c)
			}
			seen[c] = true
		}
	}
}

// Encoded (string, integer) tuples sort as the tuples do, and decode back.
func TestOrderAndRoundTrip(t *testing.T) {
	type tuple struct {
		s []byte
		n uint64
	}
	r := rand.New(rand.NewPCG(1, 2))
	var ts []tuple
	for range 2000 {
		s := make([]byte, r.IntN(4))
		for i := range s {
			s[i] = []byte{0x00, 0x01, 0xFE, 0xFF, 'a'}[r.IntN(5)]
		}
		ts = append(ts, tuple{s, uint64(r.IntN(3)) << (8 * r.IntN(8))})
	}
	enc := func(x tuple) []byte { return Uint64(String(nil, x.s), x.n) }
	for _, a := range ts {
		s, rest, err := ReadString(enc(a))
		if err != nil || !bytes.Equal(s, a.s) {
			t.Fatalf("string %x: got %x, %v", a.s, s, err)
		}
		n, rest, err := ReadUint64(rest)
		if err != nil || n != a.n || len(rest) != 0 {
			t.Fatalf("uint64 %d: got %d, rest %x, %v", a.n, n, rest, err)
		}
	}
	slices.SortFunc(ts, func(a, b tuple) int {
		if c := bytes.Compare(a.s, b.s); c != 0 {
			return c
		}
		return cmp.Compare(a.n, b.n)
	})
	for i := 1; i < len(ts); i++ {
		if bytes.Compare(enc(ts[i-1]), enc(ts[i])) > 0 {
			t.Fatalf("%v sorts after %v once encoded", ts[i-1], ts[i])
		}
	}
}

func TestReadStringRefusesMalformed(t *testing.T) {
	for _, b := range []string{"", "61", "6100", "610002"} {
		raw, _ := hex.DecodeString(b)
		if _, _, err := ReadString(raw); err != ErrMalformed {
			t.Errorf("%s: got %v, want ErrMalformed", b, err)
		}
	}
}
