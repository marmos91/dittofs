package fs

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// TestRollup_ChunkParams_SmallChunksReadBack is the #1569 threading guard: an
// FSStore configured with a small-chunk profile must (a) actually chunk newly
// written data into many small chunks (proving FSStoreOptions.ChunkParams
// reaches the rollup chunker), and (b) read that data back byte-for-byte
// (proving small chunks round-trip through rollup + the CAS-manifest read path).
func TestRollup_ChunkParams_SmallChunksReadBack(t *testing.T) {
	fbs := newRollupMemFileChunkStore()
	bc := newFSStoreForTestWithFBS(t, fbs, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   1,
		StabilizationMS: 1,
		// Persist the rollup manifest into fbs so the post-rollup read resolves
		// via the CAS-manifest path (fillFromCASManifest → blockStore). Without
		// it ListFileChunks is empty and the read only succeeds while the bytes
		// still sit in the un-trimmed append log — a race that flaked on CI once
		// the rollup won (ReadPayloadAt "file chunk not found").
		ObjectIDPersister: func(ctx context.Context, payloadID string, blocks []block.ChunkRef, _ block.ObjectID) error {
			return fbs.persist(ctx, payloadID, blocks)
		},
		// 64 KiB floor → effective avg ~94 KiB (see chunker distribution test).
		ChunkParams: chunker.Params{Min: 64 << 10, Avg: 256 << 10, Max: 512 << 10},
	})
	ctx := context.Background()
	const pid = "perf/1569/small-chunks"

	payload := make([]byte, 2<<20) // 2 MiB
	rand.New(rand.NewSource(7)).Read(payload)
	if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	if err := bc.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("ForceRollupForTest: %v", err)
	}

	// The configured small-chunk params must have threaded to the rollup
	// chunker (FSStoreOptions.ChunkParams → bc.chunkParams).
	if got := bc.ChunkParamsForTest(); got.Min != 64<<10 || got.Max != 512<<10 {
		t.Fatalf("ChunkParams did not thread: got %+v", got)
	}

	got := make([]byte, len(payload))
	n, err := bc.ReadPayloadAt(ctx, pid, got, 0)
	if err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("read-back mismatch: n=%d want=%d equal=%v", n, len(payload), bytes.Equal(got, payload))
	}
}
