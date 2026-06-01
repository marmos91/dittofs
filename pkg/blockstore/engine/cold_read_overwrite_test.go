package engine_test

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// runColdReadInPlaceOverwrite is the #953 cold-read repro. It exercises
// the data-correctness risk described in the issue:
//
//  1. Write a multi-MiB file that rolls up into several FastCDC chunks.
//  2. Overwrite a middle sub-range in place with DIFFERENT content so the
//     re-chunked region lands on DIFFERENT FastCDC boundaries than the
//     original write.
//  3. Drain the rollup again. The per-file FileBlock manifest
//     (ListFileBlocks) accumulates overlapping rows from BOTH generations
//     if the persister never deletes the superseded rows.
//  4. Force the COLD read path via ResetLocalState — the append log is
//     gone, so reads MUST resolve through the CAS manifest
//     (readLocalByHash / findRowCoveringOffset), not the warm append-log
//     replay that masks the bug.
//  5. Cold-read the whole file and assert it byte-matches the NEWEST
//     content. A stale-generation chunk surfacing here is silent data
//     corruption.
func runColdReadInPlaceOverwrite(t *testing.T, ms metadata.MetadataStore, sharePrefix string) {
	t.Helper()
	ctx := context.Background()

	shareName := sharePrefix + "-cold-ow"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverStore(t, ms)

	pid, h := createRealFile(t, ms, shareName, "overwrite.bin", rootHandle)

	// 8 MiB original content: with AvgChunkSize=4 MiB / Min=1 MiB this
	// reliably rolls up into multiple chunks. Deterministic seed so the
	// boundaries are reproducible across runs.
	const fileSize = 8 * 1024 * 1024
	orig := make([]byte, fileSize)
	rng := rand.New(rand.NewSource(0xC01D)) //nolint:gosec // deterministic test fixture
	rng.Read(orig)

	if _, err := bs.WriteAt(ctx, pid, nil, orig, 0); err != nil {
		t.Fatalf("initial WriteAt: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups (gen1): %v", err)
	}
	logManifest(t, ms, pid, "gen1")

	// Build the post-overwrite "want" image. Overwrite a middle window
	// with fresh random bytes so the re-chunk produces different
	// boundaries from gen1. The window straddles interior chunk
	// boundaries to maximize re-chunking.
	want := make([]byte, fileSize)
	copy(want, orig)
	const owOff = 2 * 1024 * 1024
	const owLen = 3 * 1024 * 1024
	owData := make([]byte, owLen)
	rng2 := rand.New(rand.NewSource(0xBEEF)) //nolint:gosec // deterministic test fixture
	rng2.Read(owData)
	copy(want[owOff:owOff+owLen], owData)

	if _, err := bs.WriteAt(ctx, pid, nil, owData, owOff); err != nil {
		t.Fatalf("overwrite WriteAt: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups (gen2): %v", err)
	}
	overlaps := logManifest(t, ms, pid, "gen2")
	t.Logf("gen2 overlapping manifest row pairs: %d", overlaps)

	// Force the cold read path: drop the append log so reads resolve via
	// the CAS manifest only. The CAS chunks themselves remain on disk.
	if err := bs.ResetLocalState(ctx); err != nil {
		t.Fatalf("ResetLocalState: %v", err)
	}

	got := make([]byte, fileSize)
	if _, err := bs.ReadAt(ctx, pid, nil, got, 0); err != nil {
		t.Fatalf("cold ReadAt: %v", err)
	}

	if !bytes.Equal(got, want) {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("cold read returned STALE bytes: first mismatch at off=%d got=0x%02x want=0x%02x (overlapping manifest rows=%d) — #953 confirmed",
					i, got[i], want[i], overlaps)
			}
		}
		t.Fatalf("cold read length/content mismatch (len got=%d want=%d)", len(got), len(want))
	}
	_ = h
}

// logManifest dumps the per-file FileBlock manifest rows and returns the
// count of overlapping (stale-superseded) row pairs.
func logManifest(t *testing.T, ms metadata.MetadataStore, pid, label string) int {
	t.Helper()
	fbl, ok := ms.(interface {
		ListFileBlocks(context.Context, string) ([]*metadata.FileBlock, error)
	})
	if !ok {
		return 0
	}
	rows, err := fbl.ListFileBlocks(context.Background(), pid)
	if err != nil {
		t.Fatalf("ListFileBlocks: %v", err)
	}
	type rng struct{ start, end uint64 }
	ranges := make([]rng, 0, len(rows))
	for _, r := range rows {
		abs, ok := blockstore.ParseChunkOffset(r.ID)
		if !ok {
			continue
		}
		ranges = append(ranges, rng{start: abs, end: abs + uint64(r.DataSize)})
		t.Logf("  %s off=%d size=%d", label, abs, r.DataSize)
	}
	overlaps := 0
	for i := 0; i < len(ranges); i++ {
		for j := i + 1; j < len(ranges); j++ {
			if ranges[i].start < ranges[j].end && ranges[j].start < ranges[i].end {
				overlaps++
			}
		}
	}
	t.Logf("%s manifest rows: %d", label, len(rows))
	return overlaps
}

// runCrossFileDedupKeepAlive is the #953 over-reap negative control. Two
// files A and B are written with BYTE-IDENTICAL content, so FastCDC
// produces identical chunk hashes and the CAS index dedups them (both
// files' FileBlock rows point at the same hashes). File A is then
// overwritten in place with different content, which reaps A's superseded
// rows. The superseded chunk hashes are still referenced by file B's rows,
// so they MUST NOT be reclaimed: a cold read of file B must still return
// B's original content. A naive by-hash reap (instead of by-exact-ID)
// would strand B's data here.
func runCrossFileDedupKeepAlive(t *testing.T, ms metadata.MetadataStore, sharePrefix string) {
	t.Helper()
	ctx := context.Background()

	shareName := sharePrefix + "-dedup-keepalive"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverStore(t, ms)

	const fileSize = 8 * 1024 * 1024
	shared := make([]byte, fileSize)
	rand.New(rand.NewSource(0x5117)).Read(shared) //nolint:gosec // deterministic test fixture

	pidA, _ := createRealFile(t, ms, shareName, "a.bin", rootHandle)
	pidB, _ := createRealFile(t, ms, shareName, "b.bin", rootHandle)

	// Both files get byte-identical content → identical FastCDC chunks →
	// shared hashes via CAS dedup.
	for _, pid := range []string{pidA, pidB} {
		if _, err := bs.WriteAt(ctx, pid, nil, shared, 0); err != nil {
			t.Fatalf("WriteAt %s: %v", pid, err)
		}
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups (shared): %v", err)
	}

	// Overwrite a middle window of file A only → re-chunks → reaps A's
	// superseded rows whose hashes file B still references.
	const owOff = 2 * 1024 * 1024
	const owLen = 3 * 1024 * 1024
	owData := make([]byte, owLen)
	rand.New(rand.NewSource(0x0FFF)).Read(owData) //nolint:gosec // deterministic test fixture
	if _, err := bs.WriteAt(ctx, pidA, nil, owData, owOff); err != nil {
		t.Fatalf("overwrite A: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups (overwrite A): %v", err)
	}

	// Force the cold path for both files.
	if err := bs.ResetLocalState(ctx); err != nil {
		t.Fatalf("ResetLocalState: %v", err)
	}

	// File B must STILL cold-read its original (shared) content — its
	// chunks were not reaped by file A's overwrite.
	gotB := make([]byte, fileSize)
	if _, err := bs.ReadAt(ctx, pidB, nil, gotB, 0); err != nil {
		t.Fatalf("cold ReadAt B: %v", err)
	}
	if !bytes.Equal(gotB, shared) {
		for i := range shared {
			if gotB[i] != shared[i] {
				t.Fatalf("over-reap: file B lost data at off=%d got=0x%02x want=0x%02x — A's reap reclaimed a chunk B still referenced",
					i, gotB[i], shared[i])
			}
		}
		t.Fatalf("file B content/length mismatch (len got=%d want=%d)", len(gotB), len(shared))
	}

	// File A must read the overwritten image.
	wantA := make([]byte, fileSize)
	copy(wantA, shared)
	copy(wantA[owOff:owOff+owLen], owData)
	gotA := make([]byte, fileSize)
	if _, err := bs.ReadAt(ctx, pidA, nil, gotA, 0); err != nil {
		t.Fatalf("cold ReadAt A: %v", err)
	}
	if !bytes.Equal(gotA, wantA) {
		for i := range wantA {
			if gotA[i] != wantA[i] {
				t.Fatalf("file A stale at off=%d got=0x%02x want=0x%02x", i, gotA[i], wantA[i])
			}
		}
	}
}

func TestMemoryColdRead_InPlaceOverwrite_ReturnsNewestBytes(t *testing.T) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	runColdReadInPlaceOverwrite(t, ms, "mem")
}

func TestMemoryColdRead_CrossFileDedupKeepAlive(t *testing.T) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	runCrossFileDedupKeepAlive(t, ms, "mem")
}

func TestBadgerColdRead_CrossFileDedupKeepAlive(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	// Close badger before the function returns so its value-log file is
	// released before t.TempDir's RemoveAll cleanup runs — Windows cannot
	// unlink a file that is still open.
	defer func() { _ = ms.Close() }()
	runCrossFileDedupKeepAlive(t, ms, "badger")
}

func TestBadgerColdRead_InPlaceOverwrite_ReturnsNewestBytes(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	// Close badger before the function returns so its value-log file is
	// released before t.TempDir's RemoveAll cleanup runs — Windows cannot
	// unlink a file that is still open.
	defer func() { _ = ms.Close() }()
	runColdReadInPlaceOverwrite(t, ms, "badger")
}
