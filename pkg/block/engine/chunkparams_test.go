package engine

import (
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestFlushFnAppliesConfiguredChunkParams proves a share's FastCDC profile
// reaches the carver through RemoteSyncConfig — the route a real share
// configures, via the production flushFn rather than a hand-built closure.
//
// It replaces journal's TestCarveHonorsChunkParams, which lane F made vacuous:
// once the flush seam went content-agnostic that test cut the run bytes with
// its own profile on both sides of the assertion, so it passed green with the
// store configured to the opposite profile and asserted nothing about the
// configuration reaching anything.
//
// The assertion is comparative on purpose. One profile's chunk count proves
// nothing alone, but for identical bytes a finer profile must yield strictly
// more manifest rows than the default. Were the configured params ignored,
// both legs would carve identically and the counts would be equal — which is
// exactly the failure the old test could not see.
func TestFlushFnAppliesConfiguredChunkParams(t *testing.T) {
	ctx := context.Background()

	// Identical bytes for both legs: the profile must be the only variable.
	payload := make([]byte, 3<<20)
	if _, err := rand.New(rand.NewSource(42)).Read(payload); err != nil {
		t.Fatalf("seeded rand: %v", err)
	}

	manifestRows := func(t *testing.T, params chunker.Params) int {
		t.Helper()
		// Block target above the payload so the pack size never becomes the
		// thing that splits the file — chunk boundaries are what is on trial.
		f := newChunkedCarveFixture(t, remotememory.New(), 8<<20, params)
		f.storeChunk(t, ctx, payload)

		closure, reap := f.syncer.flushFn()
		if err := f.local.Flush(ctx, carveFixturePayload,
			journal.FlushOptions{Force: true, AfterFile: reap}, closure); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		rows, err := f.ms.ListFileChunks(ctx, carveFixturePayload)
		if err != nil {
			t.Fatalf("ListFileChunks: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("flush committed no manifest rows")
		}
		return len(rows)
	}

	fine := manifestRows(t, chunker.Params{Min: 128 << 10, Avg: 512 << 10, Max: 1 << 20})
	coarse := manifestRows(t, chunker.DefaultParams())

	if fine <= coarse {
		t.Fatalf("fine profile produced %d manifest rows, coarse produced %d: "+
			"configured ChunkParams did not reach the carver", fine, coarse)
	}
}
