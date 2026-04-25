package chunker

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestParams_ExactValues(t *testing.T) {
	if MinChunkSize != 1*1024*1024 {
		t.Fatalf("MinChunkSize: got %d want %d", MinChunkSize, 1*1024*1024)
	}
	if AvgChunkSize != 4*1024*1024 {
		t.Fatalf("AvgChunkSize: got %d want %d", AvgChunkSize, 4*1024*1024)
	}
	if MaxChunkSize != 16*1024*1024 {
		t.Fatalf("MaxChunkSize: got %d want %d", MaxChunkSize, 16*1024*1024)
	}
	if NormalizationLevel != 2 {
		t.Fatalf("NormalizationLevel: got %d want 2", NormalizationLevel)
	}
	if MaskS == 0 || MaskL == 0 {
		t.Fatalf("masks must be non-zero: MaskS=%#x MaskL=%#x", MaskS, MaskL)
	}
}

func TestGearTable_256Distinct(t *testing.T) {
	if len(gearTable) != 256 {
		t.Fatalf("gearTable length: got %d want 256", len(gearTable))
	}
	seen := make(map[uint64]int, 256)
	for i, v := range gearTable {
		if prev, ok := seen[v]; ok {
			t.Fatalf("gearTable[%d] = %#x is duplicated with gearTable[%d]", i, v, prev)
		}
		seen[v] = i
	}
}

// chunkAll runs the chunker over data (with final=true on each call) and
// returns the list of chunk end offsets in the base frame. Fails the test
// on any zero-length or out-of-range boundary.
func chunkAll(t testing.TB, data []byte) []int {
	t.Helper()
	c := NewChunker()
	var boundaries []int
	pos := 0
	for pos < len(data) {
		b, _ := c.Next(data[pos:], true)
		if b <= 0 || b > len(data)-pos {
			t.Fatalf("bad boundary %d at pos %d (len left=%d)", b, pos, len(data)-pos)
		}
		pos += b
		boundaries = append(boundaries, pos)
	}
	return boundaries
}

func TestChunker_SmallFile_SingleChunk(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 4096)
	c := NewChunker()
	b, done := c.Next(data, true)
	if b != len(data) || !done {
		t.Fatalf("small file: got (%d,%v) want (%d,true)", b, done, len(data))
	}
}

func TestChunker_RespectsMaxChunkSize(t *testing.T) {
	data := make([]byte, MaxChunkSize*2) // all zeros — no breakpoint ever
	c := NewChunker()
	b, _ := c.Next(data, false)
	if b != MaxChunkSize {
		t.Fatalf("constant input must cut at MaxChunkSize; got %d want %d", b, MaxChunkSize)
	}
}

func TestChunker_BoundaryStability_70pct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 64 MiB property test under -short")
	}
	rng := rand.New(rand.NewSource(42))
	base := make([]byte, 64*1024*1024)
	_, _ = rng.Read(base)

	baseBoundaries := chunkAll(t, base)
	baseSet := make(map[int]struct{}, len(baseBoundaries))
	for _, b := range baseBoundaries {
		baseSet[b] = struct{}{}
	}

	const iterations = 20
	var totalRatio float64
	for i := 0; i < iterations; i++ {
		k := 1 + rng.Intn(4096) // [1,4096]
		prefix := make([]byte, k)
		_, _ = rng.Read(prefix)
		shifted := append(prefix, base...)
		shiftedBoundaries := chunkAll(t, shifted)
		// Translate shifted boundaries into "base frame" by subtracting k,
		// then keep only those that landed beyond the prefix.
		preserved := 0
		for _, sb := range shiftedBoundaries {
			if sb-k <= 0 {
				continue
			}
			if _, ok := baseSet[sb-k]; ok {
				preserved++
			}
		}
		ratio := float64(preserved) / float64(len(baseBoundaries))
		totalRatio += ratio
		t.Logf("iter %d: shift=%d preserved=%d/%d ratio=%.3f",
			i, k, preserved, len(baseBoundaries), ratio)
	}
	mean := totalRatio / iterations
	if mean < 0.70 {
		t.Fatalf("D-42 gate failed: mean boundary preservation %.3f < 0.70", mean)
	}
	t.Logf("D-42 gate met: mean preservation %.3f over %d iterations", mean, iterations)
}
