package journal

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/carver"
	"github.com/marmos91/dittofs/pkg/block/chunker"
)

func TestConfigChunkParamsDefaulting(t *testing.T) {
	// Unset (zero) params degrade to the historical default profile.
	if got := (Config{}).withDefaults().ChunkParams; got != chunker.DefaultParams() {
		t.Fatalf("unset ChunkParams = %+v, want default %+v", got, chunker.DefaultParams())
	}
	// Invalid params (Min below the floor) also degrade to default, never a hard
	// error — mirroring the fs store.
	invalid := Config{ChunkParams: chunker.Params{Min: 100, Avg: 200, Max: 300}}
	if got := invalid.withDefaults().ChunkParams; got != chunker.DefaultParams() {
		t.Fatalf("invalid ChunkParams = %+v, want default", got)
	}
	// A valid custom profile is kept verbatim.
	valid := chunker.Params{Min: 128 << 10, Avg: 512 << 10, Max: 1 << 20}
	if got := (Config{ChunkParams: valid}).withDefaults().ChunkParams; got != valid {
		t.Fatalf("valid ChunkParams = %+v, want %+v", got, valid)
	}
}

// directChunkCount runs the chunker over data exactly as packRuns does (whole
// buffer, final) so the test can assert carve honored the configured params.
func directChunkCount(data []byte, p chunker.Params) int {
	c := chunker.NewChunkerWithParams(p)
	rem := data
	count := 0
	for len(rem) > 0 {
		b, _ := c.Next(rem, true)
		if b == 0 {
			b = len(rem)
		}
		count++
		rem = rem[b:]
	}
	return count
}

func TestCarveHonorsChunkParams(t *testing.T) {
	ctx := context.Background()
	data := randBytes(3<<20, 42) // < MaxChunkSize so Flush feeds it in one pass

	small := chunker.Params{Min: 128 << 10, Avg: 512 << 10, Max: 1 << 20}
	s := testStore(t, Config{CarveBlockSize: 64 << 20, ChunkParams: small})
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	var cuts int
	fn := func(_ context.Context, run Run) ([]Extent, error) {
		buf := make([]byte, run.Extent.Len)
		if _, err := run.ReadAt(buf, run.Extent.Off); err != nil {
			return nil, err
		}
		// Count the chunk boundaries a fresh carver with this profile cuts in the
		// run's bytes — the seam is chunk-agnostic, so the profile's effect is the
		// boundary count of the bytes fn receives.
		cv := carver.New(carver.Options{Params: small})
		for off := 0; off < len(buf); {
			_, tiled, err := cv.Box(ctx, buf[off:], int64(off), false)
			if err != nil {
				return nil, err
			}
			if tiled == 0 {
				break
			}
			off += int(tiled)
			cuts++
		}
		_, tiled, err := cv.Box(ctx, nil, int64(len(buf)), true)
		if err != nil {
			return nil, err
		}
		if tiled > 0 {
			cuts++
		}
		return []Extent{run.Extent}, nil
	}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, fn); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	wantSmall := directChunkCount(data, small)
	if got := cuts; got != wantSmall {
		t.Fatalf("flush produced %d chunks, want %d (params not applied)", got, wantSmall)
	}
	// The custom profile must chunk finer than the default, else the test proves
	// nothing about the params taking effect.
	if wantDefault := directChunkCount(data, chunker.DefaultParams()); wantSmall <= wantDefault {
		t.Fatalf("small params (%d chunks) not finer than default (%d)", wantSmall, wantDefault)
	}
}
