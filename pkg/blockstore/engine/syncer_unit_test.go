package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Phase 11 Plan 02 unit tests for the rewritten syncer pipeline. These do
// not require Localstack; they drive the syncer against the in-memory
// metadata + remote backends directly so they stay fast and deterministic.

// newUnitSyncer builds a Syncer wired to in-memory backends with a tight
// SyncerConfig. The returned syncer is NOT Started — tests drive
// claimBatch / uploadOne / recoverStaleSyncing directly so the periodic
// goroutine does not race the assertions.
func newUnitSyncer(t *testing.T) (*Syncer, *remotememory.Store, *metadatamemory.MemoryMetadataStore, string) {
	t.Helper()
	tmp := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmp, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	rs := remotememory.New()

	cfg := DefaultConfig()
	cfg.ClaimBatchSize = 4
	cfg.UploadConcurrency = 2
	cfg.ClaimTimeout = 100 * time.Millisecond

	m := NewSyncer(bc, rs, ms, cfg)
	t.Cleanup(func() {
		_ = m.Close()
		_ = rs.Close()
	})
	return m, rs, ms, tmp
}

// seedPendingBlock writes a real local file under tmp and registers a
// FileBlock pointing at it in BlockStatePending so the syncer can pick it up.
func seedPendingBlock(t *testing.T, ms *metadatamemory.MemoryMetadataStore, tmp, id string, payload []byte) *blockstore.FileBlock {
	t.Helper()
	path := filepath.Join(tmp, "blk-"+id)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	b := &blockstore.FileBlock{
		ID:         "share/" + id,
		LocalPath:  path,
		DataSize:   uint32(len(payload)),
		State:      blockstore.BlockStatePending,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}
	if err := ms.PutFileBlock(context.Background(), b); err != nil {
		t.Fatalf("PutFileBlock: %v", err)
	}
	return b
}

// TestClaimBatch_FlipsPendingToSyncing asserts D-13: a single batch flips
// up to N Pending blocks to Syncing in one logical cycle and stamps
// LastSyncAttemptAt = now.
func TestClaimBatch_FlipsPendingToSyncing(t *testing.T) {
	m, _, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	// Seed 6 Pending blocks; the batch should claim at most ClaimBatchSize=4.
	for i := 0; i < 6; i++ {
		seedPendingBlock(t, ms, tmp, idxStr(i), []byte("payload-"+idxStr(i)))
	}

	before := time.Now()
	claimed, err := m.claimBatch(ctx, m.config.ClaimBatchSize)
	if err != nil {
		t.Fatalf("claimBatch: %v", err)
	}
	if len(claimed) == 0 || len(claimed) > m.config.ClaimBatchSize {
		t.Fatalf("claimBatch returned %d blocks, want 1..%d", len(claimed), m.config.ClaimBatchSize)
	}

	// Re-fetch each from storage; each MUST be Syncing with a fresh
	// LastSyncAttemptAt timestamp.
	for _, fb := range claimed {
		fresh, err := ms.GetFileBlock(ctx, fb.ID)
		if err != nil {
			t.Fatalf("GetFileBlock(%s): %v", fb.ID, err)
		}
		if fresh.State != blockstore.BlockStateSyncing {
			t.Errorf("block %s: state=%v, want Syncing", fb.ID, fresh.State)
		}
		if fresh.LastSyncAttemptAt.Before(before) {
			t.Errorf("block %s: LastSyncAttemptAt=%v not refreshed (before=%v)",
				fb.ID, fresh.LastSyncAttemptAt, before)
		}
	}
}

// TestClaimBatch_NoPendingReturnsEmpty asserts the drain-loop terminator:
// when there is no Pending work, claimBatch returns no blocks and no error.
func TestClaimBatch_NoPendingReturnsEmpty(t *testing.T) {
	m, _, _, _ := newUnitSyncer(t)
	claimed, err := m.claimBatch(context.Background(), m.config.ClaimBatchSize)
	if err != nil {
		t.Fatalf("claimBatch: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimBatch returned %d blocks on empty store, want 0", len(claimed))
	}
}

// TestUploadOne_PutsCASKeyAndFlipsRemote asserts D-11/D-15: a successful
// uploadOne writes the bytes under the CAS key (BSCAS-01), records the
// content-hash header (BSCAS-06) and only then promotes the row to Remote.
func TestUploadOne_PutsCASKeyAndFlipsRemote(t *testing.T) {
	m, rs, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	payload := []byte("uploadOne-payload-bytes")
	fb := seedPendingBlock(t, ms, tmp, "0", payload)

	// uploadOne expects the block to be in Syncing; emulate the claim batch.
	fb.State = blockstore.BlockStateSyncing
	fb.LastSyncAttemptAt = time.Now()
	if err := ms.PutFileBlock(ctx, fb); err != nil {
		t.Fatalf("PutFileBlock(syncing): %v", err)
	}

	if err := m.uploadOne(ctx, fb); err != nil {
		t.Fatalf("uploadOne: %v", err)
	}

	// Persisted row MUST be Remote with a CAS key set.
	fresh, err := ms.GetFileBlock(ctx, fb.ID)
	if err != nil {
		t.Fatalf("GetFileBlock: %v", err)
	}
	if fresh.State != blockstore.BlockStateRemote {
		t.Fatalf("State=%v, want Remote", fresh.State)
	}
	if fresh.BlockStoreKey == "" {
		t.Fatal("BlockStoreKey is empty after uploadOne")
	}
	if _, err := blockstore.ParseCASKey(fresh.BlockStoreKey); err != nil {
		t.Fatalf("BlockStoreKey %q is not a CAS key: %v", fresh.BlockStoreKey, err)
	}

	// Object exists in remote with the matching content-hash metadata.
	if _, err := rs.ReadBlock(ctx, fresh.BlockStoreKey); err != nil {
		t.Fatalf("ReadBlock(%s) after uploadOne: %v", fresh.BlockStoreKey, err)
	}
	meta := rs.GetObjectMetadata(fresh.BlockStoreKey)
	if meta["content-hash"] == "" {
		t.Fatalf("WriteBlockWithHash did not record content-hash metadata: %v", meta)
	}
}

// TestUploadOne_RejectsNonSyncing protects D-15 single-owner: uploadOne
// must only act on rows already in Syncing.
func TestUploadOne_RejectsNonSyncing(t *testing.T) {
	m, _, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	fb := seedPendingBlock(t, ms, tmp, "0", []byte("p"))
	if err := m.uploadOne(ctx, fb); err == nil {
		t.Fatal("uploadOne returned nil for Pending block, want error")
	}
}

// TestRecoverStaleSyncing_RequeuesOldRows asserts D-14: rows older than
// ClaimTimeout are flipped back to Pending with a zero LastSyncAttemptAt.
func TestRecoverStaleSyncing_RequeuesOldRows(t *testing.T) {
	m, _, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	stale := seedPendingBlock(t, ms, tmp, "stale", []byte("s"))
	stale.State = blockstore.BlockStateSyncing
	stale.LastSyncAttemptAt = time.Now().Add(-time.Hour) // way past ClaimTimeout
	if err := ms.PutFileBlock(ctx, stale); err != nil {
		t.Fatalf("PutFileBlock(stale): %v", err)
	}

	fresh := seedPendingBlock(t, ms, tmp, "fresh", []byte("f"))
	fresh.State = blockstore.BlockStateSyncing
	fresh.LastSyncAttemptAt = time.Now()
	if err := ms.PutFileBlock(ctx, fresh); err != nil {
		t.Fatalf("PutFileBlock(fresh): %v", err)
	}

	if err := m.recoverStaleSyncing(ctx); err != nil {
		t.Fatalf("recoverStaleSyncing: %v", err)
	}

	staleAfter, err := ms.GetFileBlock(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(stale): %v", err)
	}
	if staleAfter.State != blockstore.BlockStatePending {
		t.Errorf("stale.State=%v, want Pending (requeued by janitor)", staleAfter.State)
	}
	if !staleAfter.LastSyncAttemptAt.IsZero() {
		t.Errorf("stale.LastSyncAttemptAt=%v, want zero", staleAfter.LastSyncAttemptAt)
	}

	freshAfter, err := ms.GetFileBlock(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(fresh): %v", err)
	}
	if freshAfter.State != blockstore.BlockStateSyncing {
		t.Errorf("fresh.State=%v, want Syncing (within ClaimTimeout)", freshAfter.State)
	}
}

// TestSyncNow_DrainsAllPendingViaCAS exercises the full SyncNow drain loop
// end-to-end: claim → parallel upload → second claim returns 0 → return.
func TestSyncNow_DrainsAllPendingViaCAS(t *testing.T) {
	m, rs, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	const n = 5
	for i := 0; i < n; i++ {
		seedPendingBlock(t, ms, tmp, idxStr(i), []byte("payload-"+idxStr(i)))
	}

	if err := m.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}

	// All n rows should be Remote and present in the remote store.
	for i := 0; i < n; i++ {
		fresh, err := ms.GetFileBlock(ctx, "share/"+idxStr(i))
		if err != nil {
			t.Fatalf("GetFileBlock(%d): %v", i, err)
		}
		if fresh.State != blockstore.BlockStateRemote {
			t.Errorf("block %d: state=%v, want Remote", i, fresh.State)
		}
		if _, err := rs.ReadBlock(ctx, fresh.BlockStoreKey); err != nil {
			t.Errorf("block %d not in remote at %q: %v", i, fresh.BlockStoreKey, err)
		}
	}
}

func idxStr(i int) string {
	return string(rune('a' + i))
}
