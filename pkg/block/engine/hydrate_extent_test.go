package engine_test

import (
	"bytes"
	"context"
	"math/rand"
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// A hydrate writes a fetched chunk back into the local tier at the chunk's file
// offset. These tests drive the whole sequence that makes that dangerous —
// write, carve, upload, shrink the file, evict the local copy, read cold — and
// assert the bytes the shrink removed stay removed. Only a real eviction reaches
// the cold-read resolve, so every case force-evicts and fails if it did not.

// hydrateFixture builds an engine over a memory metadata store and a memory
// remote, and hands back the local tier so a test can force eviction.
func hydrateFixture(t *testing.T, share, name string) (*engine.Store, *fs.FSStore, metadata.Store, string) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, share)
	pid, _ := createRealFile(t, ms, share, name, root)
	local, ok := bs.Local().(*fs.FSStore)
	if !ok {
		t.Fatalf("local tier is %T, not the journal-backed store the eviction path needs", bs.Local())
	}
	return bs, local, ms, pid
}

// manifestRefs projects the per-file FileChunk manifest into the []ChunkRef
// shape the engine's mutating calls take, sorted by offset.
func manifestRefs(t *testing.T, ms metadata.Store, pid string) []block.ChunkRef {
	t.Helper()
	lister, ok := ms.(interface {
		ListFileChunks(context.Context, string) ([]*block.FileChunk, error)
	})
	if !ok {
		t.Fatalf("store %T has no ListFileChunks", ms)
	}
	rows, err := lister.ListFileChunks(context.Background(), pid)
	if err != nil {
		t.Fatalf("ListFileChunks: %v", err)
	}
	refs := make([]block.ChunkRef, 0, len(rows))
	for _, r := range rows {
		abs, ok := block.ParseChunkOffset(r.ID)
		if !ok {
			continue
		}
		refs = append(refs, block.ChunkRef{Hash: r.Hash, Offset: abs, Size: r.DataSize})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Offset < refs[j].Offset })
	return refs
}

// evictAll drops every evictable local segment so the next read of those bytes
// must resolve a manifest row and fetch from the remote.
func evictAll(t *testing.T, local *fs.FSStore) {
	t.Helper()
	res, err := local.Evict(context.Background(), 1<<30)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted == 0 {
		t.Fatal("nothing was evicted — the test never reaches a cold read")
	}
}

func readAt(t *testing.T, bs *engine.Store, pid string, off uint64, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := bs.ReadAt(context.Background(), pid, buf, off); err != nil {
		t.Fatalf("ReadAt at %d: %v", off, err)
	}
	return buf
}

func assertZeros(t *testing.T, got []byte, what string) {
	t.Helper()
	for i, b := range got {
		if b != 0 {
			t.Fatalf("%s: byte %d is %#x, want a zero hole — removed bytes were resurrected", what, i, b)
		}
	}
}

// TestColdReadAfterTruncate_KeepsRemovedTailZeroed is the Truncate half: a chunk
// straddling the new size is kept and nothing re-carves it, so reading its
// surviving prefix cold resolves that row and a full-chunk hydrate would put the
// removed tail back above the truncate marker's version. Re-extending the file
// is what makes it visible — the range between the old and new end owes zeros.
func TestColdReadAfterTruncate_KeepsRemovedTailZeroed(t *testing.T) {
	ctx := context.Background()
	bs, local, ms, pid := hydrateFixture(t, "truncshrink", "shrink.bin")

	const fileSize = 4 << 20
	orig := make([]byte, fileSize)
	rand.New(rand.NewSource(0x7711)).Read(orig) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, orig, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)

	refs := manifestRefs(t, ms, pid)
	if len(refs) == 0 {
		t.Fatal("no manifest rows after carve")
	}
	// Shrink into the middle of the last chunk so exactly one row straddles.
	last := refs[len(refs)-1]
	if last.Size < 8192 {
		t.Fatalf("last chunk is %d bytes, too small to straddle meaningfully", last.Size)
	}
	newSize := last.Offset + uint64(last.Size)/2

	if _, err := bs.Truncate(ctx, pid, refs, newSize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	for _, r := range manifestRefs(t, ms, pid) {
		if end := r.Offset + uint64(r.Size); end > newSize {
			t.Fatalf("manifest row at %d still claims up to %d, past the new size %d", r.Offset, end, newSize)
		}
	}

	evictAll(t, local)

	// Cold read of the surviving prefix: resolves the straddling row, fetches
	// the whole chunk from the remote, and hydrates.
	got := readAt(t, bs, pid, newSize-4096, 4096)
	if !bytes.Equal(got, orig[newSize-4096:newSize]) {
		t.Fatal("surviving prefix did not read back after the cold read")
	}

	// Re-extend past the truncate point, leaving a hole behind the new write.
	const holeLen = 8192
	tailData := bytes.Repeat([]byte{0xEE}, 4096)
	if _, err := bs.WriteAt(ctx, pid, nil, tailData, newSize+holeLen); err != nil {
		t.Fatalf("re-extend WriteAt: %v", err)
	}
	assertZeros(t, readAt(t, bs, pid, newSize, holeLen), "re-extended hole")
	if !bytes.Equal(readAt(t, bs, pid, newSize+holeLen, 4096), tailData) {
		t.Fatal("re-extending write did not read back")
	}
}

// TestColdReadAfterPunchHole_KeepsHoleZeroed is the PunchHole half: the same
// shape at a hole boundary rather than at the end of the file. A block only
// partially inside the punched range is kept, so a cold read inside the hole
// must not come back with the pre-punch bytes.
func TestColdReadAfterPunchHole_KeepsHoleZeroed(t *testing.T) {
	ctx := context.Background()
	bs, local, ms, pid := hydrateFixture(t, "punchhole", "punch.bin")

	const fileSize = 4 << 20
	orig := make([]byte, fileSize)
	rand.New(rand.NewSource(0x5150)).Read(orig) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, orig, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)

	refs := manifestRefs(t, ms, pid)
	// Punch a range that starts inside one chunk and ends inside another, so
	// blocks on both boundaries are kept partially overlapping.
	const holeOff, holeLen = 1 << 20, 256 << 10
	if _, err := bs.PunchHole(ctx, pid, refs, holeOff, holeLen); err != nil {
		t.Fatalf("PunchHole: %v", err)
	}
	// The zeros land through the local write path, so they must be carved and
	// uploaded before anything can be evicted.
	carve(t, bs, ctx, pid)

	// Structural counterpart to the read assertions: the re-carved zeros
	// supersede the punched rows, so nothing in the manifest still covers the
	// hole alongside them. Without that, a covering lookup could pick either row
	// and the reads below would only be passing by luck.
	assertManifestTiles(t, ms, pid, fileSize, "after-punch")

	evictAll(t, local)

	assertZeros(t, readAt(t, bs, pid, holeOff, holeLen), "punched range")
	// The bytes on either side of the hole are untouched and must still read.
	if !bytes.Equal(readAt(t, bs, pid, holeOff-4096, 4096), orig[holeOff-4096:holeOff]) {
		t.Fatal("bytes before the hole did not survive")
	}
	if !bytes.Equal(readAt(t, bs, pid, holeOff+holeLen, 4096), orig[holeOff+holeLen:holeOff+holeLen+4096]) {
		t.Fatal("bytes after the hole did not survive")
	}
}

// TestColdReadSparseFile_HydratesAtHighOffset is the other side of the clamp: a
// genuinely sparse file must keep working. Its data lives only at a high offset
// with no interval below it, so the hydrate has to land at that offset and the
// untouched range in front of it has to keep reading as zeros.
func TestColdReadSparseFile_HydratesAtHighOffset(t *testing.T) {
	ctx := context.Background()
	bs, local, _, pid := hydrateFixture(t, "sparse", "sparse.bin")

	const dataOff = 2 << 20
	data := make([]byte, 256<<10)
	rand.New(rand.NewSource(0x0FF5)).Read(data) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, data, dataOff); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)

	evictAll(t, local)

	if got := readAt(t, bs, pid, dataOff, len(data)); !bytes.Equal(got, data) {
		t.Fatal("high-offset data did not hydrate back")
	}
	assertZeros(t, readAt(t, bs, pid, 0, 4096), "leading hole")
	assertZeros(t, readAt(t, bs, pid, dataOff-4096, 4096), "hole in front of the data")
	assertZeros(t, readAt(t, bs, pid, dataOff+uint64(len(data)), 4096), "hole past the data")
}
