package engine_test

import (
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// refCounts returns each of a payload's rows' RefCount, keyed by row ID.
func refCounts(t *testing.T, ctx context.Context, ms *metadatamemory.MemoryMetadataStore, payloadID string) map[string]uint32 {
	t.Helper()
	rows, err := ms.ListFileChunks(ctx, payloadID)
	if err != nil {
		t.Fatalf("ListFileChunks(%s): %v", payloadID, err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s has no rows, so the counts below would say nothing", payloadID)
	}
	out := make(map[string]uint32, len(rows))
	for _, r := range rows {
		out[r.ID] = r.RefCount
	}
	return out
}

// copyFixture writes and carves one source, and returns the engine, the store,
// the two payload IDs and the ChunkRef list a copy would hand the destination.
func copyFixture(t *testing.T) (*engine.Store, *metadatamemory.MemoryMetadataStore, string, string, []block.ChunkRef) {
	t.Helper()
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, _ := openOfflineEngine(t, t.TempDir(), ms, remotememory.New())
	t.Cleanup(func() { _ = bs.Close() })

	root := createShare(t, ms, "refcount")
	src, _ := createRealFile(t, ms, "refcount", "src.bin", root)
	dst, _ := createRealFile(t, ms, "refcount", "dst.bin", root)

	data := make([]byte, 2*1024*1024)
	rand.New(rand.NewSource(5)).Read(data) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, src, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, src)

	var refs []block.ChunkRef
	for _, r := range manifestRefs(t, ms, src) {
		if !r.Hash.IsZero() {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		t.Fatal("the source carved no placeable rows")
	}
	return bs, ms, src, dst, refs
}

// TestCopyPayload_RefCountIncrementIsUnreachable records why a repeated
// server-side copy does not inflate any RefCount today, so nobody sets out to
// fix a leak that cannot happen and reaches for the one fix that would break
// something real.
//
// CopyPayload bumps RefCount once per unique source hash, and that bump is the
// only production call site that raises one — the stores also expose AddRef and
// a store-level IncrementRefCount, but nothing in production calls AddRef and
// the coordinator this goes through is the only caller of the other. The
// coordinator resolves the hash with GetByHash — and every backend scopes GetByHash
// to rows in the Remote state (memory checks IsRemote; sqlite and postgres both
// spell it `state = 2`). Nothing in production ever puts a row in that state:
// the carve path records its sync markers through SyncedHashStore and leaves
// FileChunk.State at Pending for the life of the payload, which the syncer's
// own size and existence checks already say in as many words. So the lookup
// resolves nothing, the increment is skipped as a tolerated miss, and the count
// stays where it started however many times the copy runs.
func TestCopyPayload_RefCountIncrementIsUnreachable(t *testing.T) {
	ctx := context.Background()
	bs, ms, src, dst, refs := copyFixture(t)

	before := refCounts(t, ctx, ms, src)
	for i := 1; i <= 3; i++ {
		if _, err := bs.CopyPayload(ctx, src, dst, refs); err != nil {
			t.Fatalf("CopyPayload #%d: %v", i, err)
		}
	}
	for id, want := range before {
		if got := refCounts(t, ctx, ms, src)[id]; got != want {
			t.Errorf("row %s went from RefCount %d to %d over three copies; "+
				"the increment is reachable after all, and it is not idempotent", id, want, got)
		}
	}
}

// TestCopyPayload_RefCountWouldInflateOnceReachable is the other half, and the
// one that matters later: it opens the gate by hand and shows what the copy
// does on the far side of it.
//
// With a source row moved to Remote, the coordinator's lookup resolves, the
// increment lands, and each repeat of the same copy counts the hash one higher
// with nothing to bring it back down — the copy's manifest work is idempotent
// and its counting is not. If the carve path is ever changed to transition rows
// to Remote, this test is what says the increment just came alive and still has
// no way to tell a repeat from two files that happen to share a chunk.
func TestCopyPayload_RefCountWouldInflateOnceReachable(t *testing.T) {
	ctx := context.Background()
	bs, ms, src, dst, refs := copyFixture(t)

	rows, err := ms.ListFileChunks(ctx, src)
	if err != nil {
		t.Fatalf("ListFileChunks: %v", err)
	}
	opened := rows[0]
	opened.State = block.BlockStateRemote
	if err := ms.Put(ctx, opened); err != nil {
		t.Fatalf("Put(%s) to open the gate: %v", opened.ID, err)
	}
	resolved, err := ms.GetByHash(ctx, opened.Hash)
	if err != nil || resolved == nil {
		t.Fatalf("the gate did not open: GetByHash(%x) = %v, %v", opened.Hash[:4], resolved, err)
	}

	before := refCounts(t, ctx, ms, src)[opened.ID]
	for i := 1; i <= 3; i++ {
		if _, err := bs.CopyPayload(ctx, src, dst, refs); err != nil {
			t.Fatalf("CopyPayload #%d: %v", i, err)
		}
	}
	after := refCounts(t, ctx, ms, src)[opened.ID]
	if after <= before {
		t.Fatalf("row %s stayed at RefCount %d with the gate open; "+
			"this test no longer demonstrates anything", opened.ID, after)
	}
	if after != before+3 {
		t.Errorf("row %s went from %d to %d over three copies, want %d — "+
			"the copy counts once per run, so a repeat is what inflates it",
			opened.ID, before, after, before+3)
	}
}
