package engine_test

import (
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// These tests reproduce the #953 silent cold-read corruption re-opened by the
// journal switchover: after a partial in-place overwrite of an already-carved
// file, carve re-chunks ONLY the dirty sub-range and never reaps the superseded
// FileChunk rows. The per-file FileChunk manifest is the SOLE resolver for cold
// (evicted) reads — an evicted interval carries no remote locator, so
// EnsureAvailableAndRead / GetFileChunkAtOffset must find a covering row. So a
// manifest that no longer tiles [0, fileSize) exactly is silent data corruption:
//
//   - a GAP  → cold read of the uncovered range returns zero-fill (shrink case)
//   - an OVERLAP → the greatest-start-<=-offset lookup can select a stale
//     surviving row and return pre-overwrite bytes (boundary-shift case)
//
// The assertion is on the manifest coverage invariant rather than on a forced
// cold read, because forcing a real cold read requires whole-segment eviction
// (sealed + synced segments) which does not fire at unit-test scale. The gap /
// overlap the assertion catches is exactly what a cold read would surface.

func carve(t *testing.T, bs *engine.Store, ctx context.Context, pid string) {
	t.Helper()
	if _, err := bs.Flush(ctx, pid); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}
}

// runShrinkToZeros: overwrite the first 512 KiB in place. The dirty carve run is
// [0, 512Ki), emitted as one final 512 KiB chunk — smaller than the original
// first chunk (>= 1 MiB). The old row "<pid>/0" is rewritten in place with the
// new, smaller DataSize, so [512Ki, oldFirstChunkEnd) loses its only manifest
// pointer: a GAP → cold read zero-fill.
func runShrinkToZeros(t *testing.T, ms metadata.Store) {
	ctx := context.Background()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	rootHandle := createShare(t, ms, "coldshrink")
	pid, _ := createRealFile(t, ms, "coldshrink", "shrink.bin", rootHandle)

	const fileSize = 8 * 1024 * 1024
	orig := make([]byte, fileSize)
	rand.New(rand.NewSource(0xC01D)).Read(orig) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, orig, 0); err != nil {
		t.Fatalf("initial WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)
	assertManifestTiles(t, ms, pid, fileSize, "gen1")

	const owLen = 512 * 1024
	owData := make([]byte, owLen)
	rand.New(rand.NewSource(0xBEEF)).Read(owData) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, owData, 0); err != nil {
		t.Fatalf("overwrite WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)
	assertManifestTiles(t, ms, pid, fileSize, "gen2-after-shrink")
}

// runBoundaryShiftStale: overwrite a 3 MiB middle window. Carve re-chunks the
// dirty sub-range onto boundaries that do not align with the old chunks, and the
// old higher-offset rows straddling the region survive un-reaped: an OVERLAP →
// cold read may serve stale pre-overwrite bytes.
func runBoundaryShiftStale(t *testing.T, ms metadata.Store) {
	ctx := context.Background()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	rootHandle := createShare(t, ms, "coldstale")
	pid, _ := createRealFile(t, ms, "coldstale", "stale.bin", rootHandle)

	const fileSize = 8 * 1024 * 1024
	orig := make([]byte, fileSize)
	rand.New(rand.NewSource(0x5117)).Read(orig) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, orig, 0); err != nil {
		t.Fatalf("initial WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)
	assertManifestTiles(t, ms, pid, fileSize, "gen1")

	const owOff = 2 * 1024 * 1024
	const owLen = 3 * 1024 * 1024
	owData := make([]byte, owLen)
	rand.New(rand.NewSource(0x0FFF)).Read(owData) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, owData, owOff); err != nil {
		t.Fatalf("overwrite WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)
	assertManifestTiles(t, ms, pid, fileSize, "gen2-after-boundary-shift")
}

// assertManifestTiles verifies the per-file FileChunk manifest tiles
// [0, fileSize) with no gap and no overlap — the invariant every cold read
// depends on.
func assertManifestTiles(t *testing.T, ms metadata.Store, pid string, fileSize int64, label string) {
	t.Helper()
	fbl, ok := ms.(interface {
		ListFileChunks(context.Context, string) ([]*block.FileChunk, error)
	})
	if !ok {
		t.Fatalf("%s: store %T has no ListFileChunks", label, ms)
	}
	rows, err := fbl.ListFileChunks(context.Background(), pid)
	if err != nil {
		t.Fatalf("%s: ListFileChunks: %v", label, err)
	}
	type span struct{ start, end int64 }
	spans := make([]span, 0, len(rows))
	for _, r := range rows {
		abs, ok := block.ParseChunkOffset(r.ID)
		if !ok {
			continue
		}
		spans = append(spans, span{int64(abs), int64(abs) + int64(r.DataSize)})
	}
	for i := 1; i < len(spans); i++ { // insertion sort by start (few rows)
		for j := i; j > 0 && spans[j].start < spans[j-1].start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	var cursor int64
	for _, s := range spans {
		if s.start > cursor {
			t.Errorf("%s: manifest GAP [%d, %d) — cold read zero-fills this range", label, cursor, s.start)
		}
		if s.start < cursor {
			t.Errorf("%s: manifest OVERLAP at [%d, %d) — cold read may serve stale bytes", label, s.start, cursor)
		}
		if s.end > cursor {
			cursor = s.end
		}
	}
	if cursor < fileSize {
		t.Errorf("%s: manifest GAP [%d, %d) at tail", label, cursor, fileSize)
	}
}

func TestMemoryColdRead_ShrinkToZeros(t *testing.T) {
	runShrinkToZeros(t, metadatamemory.NewMemoryMetadataStoreWithDefaults())
}

func TestMemoryColdRead_BoundaryShiftStale(t *testing.T) {
	runBoundaryShiftStale(t, metadatamemory.NewMemoryMetadataStoreWithDefaults())
}

func TestBadgerColdRead_ShrinkToZeros(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	defer func() { _ = ms.Close() }()
	runShrinkToZeros(t, ms)
}

func TestBadgerColdRead_BoundaryShiftStale(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	defer func() { _ = ms.Close() }()
	runBoundaryShiftStale(t, ms)
}
