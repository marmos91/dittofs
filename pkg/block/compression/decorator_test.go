package compression

import (
	"bytes"
	"context"
	"crypto/rand"
	"runtime"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

func factoryFor(algo Algo) blockstoretest.RemoteBlockStoreFactory {
	return func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
		t.Helper()
		d, err := NewRemote(remotememory.New(), CompressionPolicy{Algo: algo})
		if err != nil {
			t.Fatalf("NewRemote: %v", err)
		}
		return d, func() { _ = d.Close() }
	}
}

func TestConformance_Zstd(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, factoryFor(AlgoZstd))
}

func TestConformance_LZ4(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, factoryFor(AlgoLZ4))
}

// --- savings (paired raw vs zstd vs lz4) --------------------------------

// hashOf returns the BLAKE3 CAS key for payload — the hash the engine binds to
// a chunk before sealing it.
func hashOf(payload []byte) block.ContentHash {
	sum := blake3.Sum256(payload)
	var h block.ContentHash
	copy(h[:], sum[:])
	return h
}

// sealAndRead seals payload under algo, stages the sealed bytes as a one-chunk
// block, reads that chunk back, and asserts the plaintext survives. It returns
// the wire bytes so callers can compare the compressed size against the
// plaintext — the base store's SealChunk is the identity transform, so the
// returned length is exactly what this decorator's layer put on the wire.
func sealAndRead(t *testing.T, algo Algo, blockID string, payload []byte) []byte {
	t.Helper()
	d, err := NewRemote(remotememory.New(), CompressionPolicy{Algo: algo})
	if err != nil {
		t.Fatalf("NewRemote %v: %v", algo, err)
	}
	ctx := context.Background()
	h := hashOf(payload)
	wire, err := d.SealChunk(ctx, h, payload)
	if err != nil {
		t.Fatalf("SealChunk %v: %v", algo, err)
	}
	if err := d.PutBlock(ctx, blockID, bytes.NewReader(wire)); err != nil {
		t.Fatalf("PutBlock %v: %v", algo, err)
	}
	got, err := d.ReadChunk(ctx, blockID, 0, int64(len(wire)), h)
	if err != nil {
		t.Fatalf("ReadChunk %v: %v", algo, err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("%v round-trip mismatch: %d bytes back, want %d", algo, len(got), len(payload))
	}
	return wire
}

func TestSavings_PairedRawZstdLZ4(t *testing.T) {
	const size = 4 << 20 // 4 MiB

	textPayload := bytes.Repeat([]byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit. "), size/57+1)[:size]
	zeroPayload := make([]byte, size)
	randPayload := make([]byte, size)
	if _, err := rand.Read(randPayload); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name             string
		payload          []byte
		expectShrinkZstd bool
		expectShrinkLZ4  bool
	}{
		{"text_4mib", textPayload, true, true},
		{"zero_4mib", zeroPayload, true, true},
		{"random_4mib", randPayload, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The raw baseline is the plaintext: an undecorated chain seals a
			// chunk verbatim, so that is the wire cost compression must beat.
			rawWire := len(tc.payload)
			zstdWire := len(sealAndRead(t, AlgoZstd, "zstd-"+tc.name, tc.payload))
			lz4Wire := len(sealAndRead(t, AlgoLZ4, "lz4-"+tc.name, tc.payload))

			if tc.expectShrinkZstd {
				if zstdWire >= rawWire {
					t.Errorf("zstd did not shrink %s: %d wire vs %d raw", tc.name, zstdWire, rawWire)
				}
			} else {
				// incompressible: decorator falls back to raw passthrough (no frame).
				if zstdWire != rawWire {
					t.Errorf("zstd on incompressible %s: wire=%d want raw=%d (skip-on-expansion)", tc.name, zstdWire, rawWire)
				}
			}
			if tc.expectShrinkLZ4 {
				if lz4Wire >= rawWire {
					t.Errorf("lz4 did not shrink %s: %d wire vs %d raw", tc.name, lz4Wire, rawWire)
				}
			} else {
				if lz4Wire != rawWire {
					t.Errorf("lz4 on incompressible %s: wire=%d want raw=%d (skip-on-expansion)", tc.name, lz4Wire, rawWire)
				}
			}
			t.Logf("%s: raw=%d  zstd=%d (%.1f%%)  lz4=%d (%.1f%%)",
				tc.name, rawWire,
				zstdWire, 100*float64(zstdWire)/float64(rawWire),
				lz4Wire, 100*float64(lz4Wire)/float64(rawWire))
		})
	}
}

// --- alloc bound --------------------------------------------------------

// TestSealChunk_AllocBounded pins sealLayer's single-buffer design: the frame
// header is reserved up front and the codec streams the body straight after it,
// so sealing a 4 MiB chunk must not cost a second full copy of the plaintext.
func TestSealChunk_AllocBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping alloc bound under -short")
	}
	if raceEnabled {
		t.Skip("skipping alloc bound under -race (instrumentation doubles allocs)")
	}
	d, err := NewRemote(remotememory.New(), CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	const size = 4 << 20
	payload := bytes.Repeat([]byte("alloc-bound-text. "), size/18+1)[:size]
	h := hashOf(payload)
	// Warm pools.
	for range 4 {
		if _, err := d.SealChunk(context.Background(), h, payload); err != nil {
			t.Fatalf("warmup SealChunk: %v", err)
		}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const N = 8
	for range N {
		if _, err := d.SealChunk(context.Background(), h, payload); err != nil {
			t.Fatalf("measured SealChunk: %v", err)
		}
	}
	runtime.ReadMemStats(&after)
	allocPerSeal := (after.TotalAlloc - before.TotalAlloc) / N
	// Plaintext (4 MiB) + codec window (≤ ~256 KiB) — leave generous headroom for runtime noise.
	const budget = (4 << 20) + (1 << 20)
	if allocPerSeal > budget {
		t.Fatalf("alloc per SealChunk = %d bytes, budget %d", allocPerSeal, budget)
	}
	t.Logf("alloc per 4 MiB SealChunk: %d bytes (budget %d)", allocPerSeal, budget)
}
