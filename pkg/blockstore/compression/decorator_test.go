package compression

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"runtime"
	"sync"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/blockstoretest"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
)

func factoryFor(algo Algo) blockstoretest.Factory {
	return func(t *testing.T) (blockstore.BlockStore, func()) {
		t.Helper()
		inner := remotememory.New()
		d, err := NewRemote(inner, CompressionPolicy{Algo: algo})
		if err != nil {
			t.Fatalf("NewRemote: %v", err)
		}
		return d, func() { _ = d.Close() }
	}
}

func TestConformance_Zstd(t *testing.T) {
	blockstoretest.BlockStoreConformance(t, factoryFor(AlgoZstd))
}

func TestConformance_LZ4(t *testing.T) {
	blockstoretest.BlockStoreConformance(t, factoryFor(AlgoLZ4))
}

// --- savings (paired raw vs zstd vs lz4) --------------------------------

// spyRemote wraps an inner RemoteStore and records the wire bytes
// observed at Put, so the savings test can compare the compressed wire
// size against the plaintext.
type spyRemote struct {
	remote.RemoteStore
	mu       sync.Mutex
	lastWire []byte
}

func (s *spyRemote) Put(ctx context.Context, h blockstore.ContentHash, data []byte) error {
	s.mu.Lock()
	s.lastWire = append([]byte(nil), data...)
	s.mu.Unlock()
	return s.RemoteStore.Put(ctx, h, data)
}

func (s *spyRemote) wireLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lastWire)
}

// hashOf returns the BLAKE3 CAS key for payload — the same shape all
// tests use to derive the plaintext hash before Put.
func hashOf(payload []byte) blockstore.ContentHash {
	sum := blake3.Sum256(payload)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

func putAndGet(t *testing.T, bs blockstore.BlockStore, payload []byte) blockstore.ContentHash {
	t.Helper()
	h := hashOf(payload)
	if err := bs.Put(context.Background(), h, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := bs.Get(context.Background(), h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: %d bytes back, want %d", len(got), len(payload))
	}
	return h
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
			// raw baseline
			rawSpy := &spyRemote{RemoteStore: remotememory.New()}
			rawHash := putThroughSpy(t, rawSpy, tc.payload)
			rawWire := rawSpy.wireLen()

			zstdSpy := &spyRemote{RemoteStore: remotememory.New()}
			zstdDec, err := NewRemote(zstdSpy, CompressionPolicy{Algo: AlgoZstd})
			if err != nil {
				t.Fatalf("NewRemote zstd: %v", err)
			}
			zstdHash := putAndGet(t, zstdDec, tc.payload)
			zstdWire := zstdSpy.wireLen()

			lz4Spy := &spyRemote{RemoteStore: remotememory.New()}
			lz4Dec, err := NewRemote(lz4Spy, CompressionPolicy{Algo: AlgoLZ4})
			if err != nil {
				t.Fatalf("NewRemote lz4: %v", err)
			}
			lz4Hash := putAndGet(t, lz4Dec, tc.payload)
			lz4Wire := lz4Spy.wireLen()

			// CAS invariant: hash is over plaintext, same across all variants.
			if rawHash != zstdHash || rawHash != lz4Hash {
				t.Fatalf("hashes differ: raw=%s zstd=%s lz4=%s", rawHash, zstdHash, lz4Hash)
			}

			if rawWire != len(tc.payload) {
				t.Fatalf("raw wire size: got %d want %d", rawWire, len(tc.payload))
			}

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

func putThroughSpy(t *testing.T, spy *spyRemote, payload []byte) blockstore.ContentHash {
	t.Helper()
	h := hashOf(payload)
	if err := spy.Put(context.Background(), h, payload); err != nil {
		t.Fatalf("spy Put: %v", err)
	}
	got, err := spy.Get(context.Background(), h)
	if err != nil {
		t.Fatalf("spy Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("spy round-trip mismatch")
	}
	return h
}

// --- ReadBlockVerified --------------------------------------------------

func TestReadBlockVerified_RoundTrip(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("verify-me-please. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	out, err := d.ReadBlockVerified(context.Background(), h, h)
	if err != nil {
		t.Fatalf("ReadBlockVerified: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatal("plaintext mismatch")
	}
}

func TestReadBlockVerified_MismatchTrips(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("trip-me. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	bogus := h
	bogus[0] ^= 0xff
	_, err = d.ReadBlockVerified(context.Background(), h, bogus)
	if !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("err: got %v want wraps ErrCASContentMismatch", err)
	}
}

// --- Head + Walk plaintext-size contract --------------------------------

func TestHead_ReportsPlaintextSize(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("compressible-text. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	m, err := d.Head(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != int64(len(payload)) {
		t.Fatalf("Head.Size: got %d want %d (plaintext)", m.Size, len(payload))
	}
}

func TestGetRange_InvalidLength(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("range. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	for _, length := range []int64{0, -1, -1024} {
		_, err := d.GetRange(context.Background(), h, 0, length)
		if !errors.Is(err, blockstore.ErrInvalidSize) {
			t.Errorf("length=%d: got %v, want wraps ErrInvalidSize", length, err)
		}
	}
}

// failingRangeStore wraps an inner RemoteStore and forces inner.GetRange
// to return an error. Used to exercise the plaintext-size probe failure
// path.
type failingRangeStore struct {
	remote.RemoteStore
}

func (f *failingRangeStore) GetRange(ctx context.Context, hash blockstore.ContentHash, offset, length int64) ([]byte, error) {
	return nil, errors.New("simulated backend range failure")
}

func TestHead_ProbeFailurePropagates(t *testing.T) {
	// Stage a real framed block in the inner store via a normally
	// functioning decorator, then re-wrap the same inner with a
	// failingRangeStore so the plaintext-size probe path is exercised
	// against actual framed bytes on the wire.
	inner := remotememory.New()
	payload := bytes.Repeat([]byte("compressible. "), 4096)
	h := hashOf(payload)
	staging, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	if err := staging.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	d, err := NewRemote(&failingRangeStore{RemoteStore: inner}, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Head(context.Background(), h); err == nil {
		t.Fatal("Head: expected probe error, got nil")
	}
}

// --- alloc bound --------------------------------------------------------

func TestPut_AllocBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping alloc bound under -short")
	}
	if raceEnabled {
		t.Skip("skipping alloc bound under -race (instrumentation doubles allocs)")
	}
	inner := remotememory.New()
	d, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	const size = 4 << 20
	payload := bytes.Repeat([]byte("alloc-bound-text. "), size/18+1)[:size]
	h := hashOf(payload)
	// Warm pools.
	for range 4 {
		if err := d.Put(context.Background(), h, payload); err != nil {
			t.Fatalf("warmup Put: %v", err)
		}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const N = 8
	for range N {
		if err := d.Put(context.Background(), h, payload); err != nil {
			t.Fatalf("measured Put: %v", err)
		}
	}
	runtime.ReadMemStats(&after)
	allocPerPut := (after.TotalAlloc - before.TotalAlloc) / N
	// Plaintext (4 MiB) + codec window (≤ ~256 KiB) — leave generous headroom for runtime noise.
	const budget = (4 << 20) + (1 << 20)
	if allocPerPut > budget {
		t.Fatalf("alloc per Put = %d bytes, budget %d", allocPerPut, budget)
	}
	t.Logf("alloc per 4 MiB Put: %d bytes (budget %d)", allocPerPut, budget)
}
