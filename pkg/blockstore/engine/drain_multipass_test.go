package engine_test

import (
	"context"
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// loggingCoordinator wraps testCoordinator to record every PersistFileBlocks
// call so a multi-pass rollup is observable: how many calls, and the block
// offsets carried by each.
type loggingCoordinator struct {
	*testCoordinator
	t     *testing.T
	calls [][]blockstore.BlockRef
}

func (c *loggingCoordinator) PersistFileBlocks(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, objectID blockstore.ObjectID) error {
	offs := make([]uint64, len(blocks))
	for i, b := range blocks {
		offs[i] = b.Offset
	}
	c.t.Logf("PersistFileBlocks(payload=%s, nblocks=%d, offsets=%v, objID=%s)",
		payloadID, len(blocks), offs, objectID.String()[:12])
	cp := append([]blockstore.BlockRef(nil), blocks...)
	c.calls = append(c.calls, cp)
	return c.testCoordinator.PersistFileBlocks(ctx, payloadID, blocks, objectID)
}

// runMultiPassAppend writes a file in TWO separate write+drain cycles at
// DISTINCT offsets (a real append), forcing two rollup passes, then asserts
// the persisted FileAttr.Blocks cover the WHOLE file [0, size) — not just the
// last pass's region. This is the assertion prior conformance tests lacked
// (they checked fileBlocks ⊆ manifest, both derived from the same replaced
// rows, so a replace-loses-prior-passes bug stayed invisible).
func runMultiPassAppend(t *testing.T, ms metadata.MetadataStore, sharePrefix string) {
	t.Helper()
	ctx := context.Background()

	shareName := sharePrefix + "-mp"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverStore(t, ms)

	const half = 2 * 1024 * 1024
	first := distinctContent(0x40, half)
	second := distinctContent(0x41, half)

	pid, h := createRealFile(t, ms, shareName, "append.bin", rootHandle)

	// Pass 1: write [0, 2MB), drain.
	if _, err := bs.WriteAt(ctx, pid, nil, first, 0); err != nil {
		t.Fatalf("WriteAt pass1: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups pass1: %v", err)
	}
	p1 := fileBlocks(t, ms, h)
	t.Logf("after pass1: fileBlocks=%d", len(p1))

	// Pass 2: append [2MB, 4MB), drain.
	if _, err := bs.WriteAt(ctx, pid, nil, second, half); err != nil {
		t.Fatalf("WriteAt pass2: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups pass2: %v", err)
	}
	p2 := fileBlocks(t, ms, h)
	t.Logf("after pass2: fileBlocks=%d", len(p2))

	// Probe the per-file FileBlock index (the read-path source) to see if
	// IT accumulates across passes even though FileAttr.Blocks does not.
	if fbl, ok := ms.(interface {
		ListFileBlocks(context.Context, string) ([]*metadata.FileBlock, error)
	}); ok {
		rows, err := fbl.ListFileBlocks(ctx, pid)
		if err != nil {
			t.Logf("ListFileBlocks err: %v", err)
		} else {
			roffs := make([]uint64, 0, len(rows))
			for _, r := range rows {
				if off, ok := blockstore.ParseChunkOffset(r.ID); ok {
					roffs = append(roffs, off)
				}
			}
			t.Logf("after pass2: FileBlock-index rows=%d offsets=%v", len(rows), roffs)
		}
	}

	// Assert the persisted blocks cover the whole [0, 4MB) byte range
	// contiguously. A replace-loses-prior-passes bug leaves a hole at the
	// front (only [2MB,4MB) survives).
	blocks := append([]blockstore.BlockRef(nil), p2...)
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Offset < blocks[j].Offset })
	var cursor uint64
	for _, b := range blocks {
		if b.Offset != cursor {
			t.Fatalf("coverage gap: next block at offset %d, expected %d (file_block_refs lost a prior pass; have %d blocks covering up to %d of %d)",
				b.Offset, cursor, len(blocks), cursor, uint64(2*half))
		}
		cursor += uint64(b.Size)
	}
	if cursor != uint64(2*half) {
		t.Fatalf("blocks cover [0,%d) but file is %d bytes", cursor, 2*half)
	}
	t.Logf("OK: %d blocks cover full [0,%d)", len(blocks), cursor)
}

func TestMemoryDrainRollups_MultiPassAppend(t *testing.T) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	runMultiPassAppend(t, ms, "mem")
}

func TestBadgerDrainRollups_MultiPassAppend(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	runMultiPassAppend(t, ms, "badger")
}
