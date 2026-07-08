package chunker

import (
	"math/rand"
	"testing"
)

func TestParams_Validate(t *testing.T) {
	cases := []struct {
		name string
		p    Params
		ok   bool
	}{
		{"default", DefaultParams(), true},
		{"small-random", Params{Min: 64 << 10, Avg: 256 << 10, Max: 512 << 10}, true},
		{"below-floor", Params{Min: 1 << 10, Avg: 4 << 10, Max: 8 << 10}, false},
		{"unordered", Params{Min: 256 << 10, Avg: 128 << 10, Max: 512 << 10}, false},
		{"max-below-avg", Params{Min: 64 << 10, Avg: 256 << 10, Max: 128 << 10}, false},
	}
	for _, c := range cases {
		if got := c.p.Validate() == nil; got != c.ok {
			t.Errorf("%s: Validate ok=%v, want %v", c.name, got, c.ok)
		}
	}
}

// TestNewChunkerWithParams_InvalidFallsBackToDefault guards the degrade-safe
// contract: a bad Params must not produce degenerate chunks.
func TestNewChunkerWithParams_InvalidFallsBackToDefault(t *testing.T) {
	ck := NewChunkerWithParams(Params{Min: 1, Avg: 2, Max: 3}) // below floor
	if ck.p != DefaultParams() {
		t.Fatalf("invalid params should fall back to default, got %+v", ck.p)
	}
}

// TestChunker_MinDrivesEffectiveAverage is the #1569 regression guard: the
// effective average chunk size tracks Min (the dominant knob under the shared
// masks), and Max is respected as a hard ceiling. Also pins the default profile
// to its historical ~1 MiB so existing data re-chunks identically.
func TestChunker_MinDrivesEffectiveAverage(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	data := make([]byte, 64*1024*1024)
	r.Read(data)

	measure := func(p Params) (effAvg, maxSeen int) {
		ck := NewChunkerWithParams(p)
		pos, n, total := 0, 0, 0
		for pos < len(data) {
			b, _ := ck.Next(data[pos:], true)
			if b <= 0 {
				break
			}
			if b > maxSeen {
				maxSeen = b
			}
			if b > p.Max {
				t.Fatalf("chunk %d exceeds Max %d", b, p.Max)
			}
			n++
			total += b
			pos += b
		}
		return total / n, maxSeen
	}

	// Effective average tracks Min: within [Min, ~Min*2] for the small-random band.
	for _, min := range []int{64 << 10, 128 << 10, 256 << 10} {
		p := Params{Min: min, Avg: min * 4, Max: min * 8}
		effAvg, _ := measure(p)
		if effAvg < min || effAvg > 2*min {
			t.Errorf("min=%dK: effAvg=%dK, want in [%dK, %dK]",
				min>>10, effAvg>>10, min>>10, (2*min)>>10)
		}
	}

	// Default profile stays ~1 MiB (byte-identical chunking to pre-#1569).
	effAvg, _ := measure(DefaultParams())
	if effAvg < 900<<10 || effAvg > 1200<<10 {
		t.Errorf("default effAvg=%dK, want ~1024K (dedup continuity)", effAvg>>10)
	}
}
