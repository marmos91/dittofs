package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newDrainResetFixture builds a full engine.Store over an fs local
// store + memory metadata, with a LARGE stabilization window and the
// rollup worker pool DELIBERATELY NOT started, so the only way dirty
// append-log bytes reach CAS + the FileBlock manifest is via an explicit
// DrainRollups. This reproduces the snapshot race where a snapshot is
// taken before the async rollup catches up.
func newDrainResetFixture(t *testing.T) (*Store, *fs.FSStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 3_600_000, // 1h — async/ticker rollup can never fire
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	// NOTE: intentionally NOT calling StartRollup.

	syncer := NewSyncer(localStore, nil, ms, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Syncer:          syncer,
		FileBlockStore:  ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore
}

// TestEngine_DrainRollups_PopulatesManifest reproduces C1 at the engine
// layer through the REAL write path (bs.WriteAt — NOT pre-populated
// metadata). Before DrainRollups the FileBlock manifest is empty (the
// async rollup never ran), so a metadata Backup taken now would yield an
// empty snapshot manifest. After DrainRollups the manifest is non-empty.
func TestEngine_DrainRollups_PopulatesManifest(t *testing.T) {
	ctx := context.Background()
	bs, _ := newDrainResetFixture(t)

	payloadID := "c1-file"
	data := bytes.Repeat([]byte{0x7E}, 4*1024*1024)
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Pre-drain: manifest must be empty — proves the snapshot race would
	// capture an empty manifest.
	pre, err := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		t.Fatalf("ListFileBlocks (pre): %v", err)
	}
	if len(pre) != 0 {
		t.Fatalf("manifest already populated before DrainRollups (%d rows); cannot prove C1", len(pre))
	}

	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	post, err := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		t.Fatalf("ListFileBlocks (post): %v", err)
	}
	if len(post) == 0 {
		t.Fatal("DrainRollups did not populate the FileBlock manifest (C1: empty snapshot manifest)")
	}
}

// TestEngine_ResetLocalState_RestoreRoundTrip reproduces C2 (restore
// corrupts in-place-modified files) at the engine layer through the REAL
// write path. Sequence:
//
//  1. Write file A "AAAA…", DrainRollups → A's bytes are durable in CAS +
//     manifest (the snapshot state).
//  2. Modify A in place ("BBBB…") via a fresh WriteAt — the new bytes land
//     ONLY in the append log (no DrainRollups). Reads now show "BBBB…".
//  3. ResetLocalState (the restore primitive) drops the stale append-log
//     overlay.
//  4. Read A again: it MUST return the snapshot bytes "AAAA…" resolved
//     purely from the CAS manifest, not the post-snapshot "BBBB…".
//
// Without ResetLocalState the read returns the mutated bytes because
// ReadPayloadAt's replayLogIntoDest overlays the post-snapshot log record
// on top of the restored CAS content ("last record wins").
func TestEngine_ResetLocalState_RestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	bs, _ := newDrainResetFixture(t)

	payloadID := "c2-file"
	const size = 4096
	snapBytes := bytes.Repeat([]byte{'A'}, size)
	mutBytes := bytes.Repeat([]byte{'B'}, size)

	// (1) snapshot state.
	if _, err := bs.WriteAt(ctx, payloadID, nil, snapBytes, 0); err != nil {
		t.Fatalf("WriteAt snapshot bytes: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups (snapshot): %v", err)
	}

	got := make([]byte, size)
	if _, err := bs.ReadAt(ctx, payloadID, nil, got, 0); err != nil {
		t.Fatalf("ReadAt after snapshot drain: %v", err)
	}
	if !bytes.Equal(got, snapBytes) {
		t.Fatal("post-drain read did not return snapshot bytes")
	}

	// (2) in-place modification, log-only.
	if _, err := bs.WriteAt(ctx, payloadID, nil, mutBytes, 0); err != nil {
		t.Fatalf("WriteAt mutation: %v", err)
	}
	clear(got)
	if _, err := bs.ReadAt(ctx, payloadID, nil, got, 0); err != nil {
		t.Fatalf("ReadAt after mutation: %v", err)
	}
	if !bytes.Equal(got, mutBytes) {
		t.Fatal("post-mutation read did not return mutated bytes; test setup invalid")
	}

	// (3) restore: reset block-store local state (metadata reset is modeled
	// by the CAS manifest still holding the snapshot blocks — they were
	// never overwritten because the mutation never rolled up).
	if err := bs.ResetLocalState(ctx); err != nil {
		t.Fatalf("ResetLocalState: %v", err)
	}

	// (4) byte-exact: read resolves through CAS = snapshot bytes.
	clear(got)
	if _, err := bs.ReadAt(ctx, payloadID, nil, got, 0); err != nil {
		t.Fatalf("ReadAt after reset: %v", err)
	}
	if !bytes.Equal(got, snapBytes) {
		t.Fatalf("ResetLocalState did not drop stale log (C2 corruption):\n got[:8]=%q want[:8]=%q",
			got[:8], snapBytes[:8])
	}
}
