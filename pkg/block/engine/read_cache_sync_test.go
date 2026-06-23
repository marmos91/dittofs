package engine

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestInlineFetch_MarksFetchedChunkSyncedAndCancelsReupload pins the #1362
// fix for read-fetch sync accounting. A chunk pulled from the remote tier on a
// read miss is verbatim remote content, so the syncer must NOT schedule it for
// re-upload (which turns a read-heavy workload into a write-heavy one) and must
// mark it synced so eviction can reclaim it immediately rather than after a
// mirror pass. inlineFetchOrWait persists via local.Put then calls
// markFetchedSynced, which (1) cancels the pending-upload entry StoreChunk's
// onChunkComplete callback registers and (2) marks the hash synced.
func TestInlineFetch_MarksFetchedChunkSyncedAndCancelsReupload(t *testing.T) {
	ctx := context.Background()
	payloadID := "payload-readcache"
	data := []byte("verbatim-remote-bytes-fetched-on-a-read-miss-0123456789")

	loc := memorylocal.New()
	rs := remotememory.New()
	fbs := newStubFileBlockStore()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	hash, _ := seedFileBlock(t, fbs, rs, payloadID, data)

	m := &Syncer{
		local:           loc,
		remoteStore:     rs,
		fileBlockStore:  fbs,
		inFlight:        make(map[string]*fetchResult),
		stopCh:          make(chan struct{}),
		config:          DefaultConfig(),
		pendingHashes:   make(map[block.ContentHash]int64),
		syncedHashStore: mds,
	}

	// The memory LocalStore used here does not fire StoreChunk's
	// onChunkComplete callback, so register the pending-upload entry
	// explicitly to model the real read-fetch path that markFetchedSynced
	// must undo.
	m.addPendingHash(hash, int64(len(data)))
	if got := m.UnsyncedBytes(); got != int64(len(data)) {
		t.Fatalf("precondition unsyncedBytes=%d, want %d", got, len(data))
	}

	rows, err := m.listFileBlocksSnapshot(ctx, payloadID)
	if err != nil {
		t.Fatalf("listFileBlocksSnapshot: %v", err)
	}
	gotData, downloaded, err := m.inlineFetchOrWait(ctx, payloadID, 0, rows)
	if err != nil {
		t.Fatalf("inlineFetchOrWait: %v", err)
	}
	if !downloaded || gotData == nil {
		t.Fatalf("inlineFetchOrWait downloaded=%v data=%v; want a successful inline fetch", downloaded, gotData)
	}

	// (1) Re-upload canceled: nothing left pending, so mirrorOnce uploads nothing.
	m.pendingMu.Lock()
	_, stillPending := m.pendingHashes[hash]
	m.pendingMu.Unlock()
	if stillPending {
		t.Errorf("fetched chunk still pending upload; the mirror loop would re-upload remote bytes (#1362)")
	}
	if got := m.UnsyncedBytes(); got != 0 {
		t.Errorf("unsyncedBytes=%d after fetch, want 0 (pending entry must be canceled)", got)
	}

	// (2) Marked synced: eviction's IsSynced gate can reclaim it immediately.
	synced, err := mds.IsSynced(ctx, hash)
	if err != nil {
		t.Fatalf("IsSynced: %v", err)
	}
	if !synced {
		t.Errorf("fetched chunk not marked synced; eviction would skip it on a read-only workload (#1362)")
	}
}
