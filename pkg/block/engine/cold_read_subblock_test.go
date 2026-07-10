package engine_test

import (
	"context"
	"math/rand"
	"testing"

	"lukechampine.com/blake3"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestColdReadIntegrity_SubBlockChunks reproduces the blocks-flip SMB cold-read
// corruption: a 16 MiB file rolls up into ~1 MiB FastCDC chunks (random data
// hits breakpoints near MinChunkSize), so each 8 MiB engine block holds several
// chunks. After DrainLocalSynced evicts the local tier, the whole file is read
// back in 1 MiB windows (SMB's max-read shape).
//
// Before the fix, EnsureAvailableAndRead iterated 8 MiB block indices and
// resolved only the chunk covering blockIdx*BlockSize, so every read window
// past a block's first chunk was never fetched — served as zeros/stale bytes
// (and only the block-aligned chunks were staged locally). SMB exercises this
// because it has no page cache; NFS's page cache serves the read locally and
// never drives the server read path, which is why the NFS variant passed.
func TestColdReadIntegrity_SubBlockChunks(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs := newEngineWithRemote(t, ms, mem)

	rootHandle := createShare(t, ms, "coldsub")
	pid, _ := createRealFile(t, ms, "coldsub", "big.bin", rootHandle)

	const oneMiB = 1024 * 1024
	const fileSize = 16 * oneMiB
	src := make([]byte, fileSize)
	rand.New(rand.NewSource(0x5EED)).Read(src) //nolint:gosec // deterministic fixture

	for off := 0; off < fileSize; off += oneMiB {
		if _, err := bs.WriteAt(ctx, pid, nil, src[off:off+oneMiB], uint64(off)); err != nil {
			t.Fatalf("WriteAt off=%d: %v", off, err)
		}
	}
	if _, err := bs.Flush(ctx, pid); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}
	// Force a genuine cold read: seal + evict every synced local block so the
	// read path must round-trip to the remote (mirrors the e2e `evict`).
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}

	buf := make([]byte, oneMiB)
	for i := 0; i < fileSize/oneMiB; i++ {
		for j := range buf {
			buf[j] = 0xAA // poison so an unfilled window fails instead of hiding in zeros
		}
		off := uint64(i) * oneMiB
		n, err := bs.ReadAt(ctx, pid, nil, buf, off)
		if err != nil {
			t.Fatalf("ReadAt off=%d: %v", off, err)
		}
		if n != oneMiB {
			t.Fatalf("ReadAt off=%d short read n=%d", off, n)
		}
		want := src[off : off+oneMiB]
		if blake3.Sum256(buf) != blake3.Sum256(want) {
			for j := range buf {
				if buf[j] != want[j] {
					t.Fatalf("COLD READ MISMATCH window=%d off=%d +%d got=0x%02x want=0x%02x",
						i, off, j, buf[j], want[j])
				}
			}
			t.Fatalf("COLD READ MISMATCH window=%d (hash differs, no byte diff?)", i)
		}
	}
}
