package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"lukechampine.com/blake3"
)

// TestMirrorOnce_EvictedUnsyncedHash_NotSilentlyDropped proves that when
// a pending hash has no backing bytes in the local store (the chunk was
// evicted/lost before its first mirror), mirrorOnce retains the hash in
// the pending set for a later retry rather than silently deleting it,
// AND surfaces ErrChunkLostBeforeMirror so Flush/SyncNow do not report
// the payload as durable on remote. Dropping it would permanently lose
// the only copy of never-mirrored data — the data-loss bug this fix
// closes.
func TestMirrorOnce_EvictedUnsyncedHash_NotSilentlyDropped(t *testing.T) {
	ctx := context.Background()

	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = localStore.Close() })

	remote := remotememory.New()
	syncer := NewSyncer(localStore, remote, ms, DefaultConfig())
	// Wire the SyncedHashStore so mirrorOnce actually walks the pending
	// set (it short-circuits to nil when no SyncedHashStore is present).
	syncer.SetSyncedHashStore(ms)

	// Register a hash for upload whose bytes were never stored locally —
	// local.Get(hash) will return block.ErrChunkNotFound, modelling a
	// chunk evicted/lost before its first mirror.
	sum := blake3.Sum256([]byte("never-stored-chunk"))
	var h block.ContentHash
	copy(h[:], sum[:])
	syncer.addPendingHash(h)

	if got := syncer.pendingLen(); got != 1 {
		t.Fatalf("precondition: pendingLen = %d, want 1", got)
	}

	// Sanity: the local store really does miss this hash.
	if _, err := localStore.Get(ctx, h); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("precondition: local.Get should miss, got err=%v", err)
	}

	if err := syncer.mirrorOnce(ctx); !errors.Is(err, block.ErrChunkLostBeforeMirror) {
		t.Fatalf("mirrorOnce err = %v, want ErrChunkLostBeforeMirror (not durable, retained for retry)", err)
	}

	if got := syncer.pendingLen(); got != 1 {
		t.Fatalf("evicted-before-mirror hash was dropped from pending set: pendingLen = %d, want 1 (retained for retry)", got)
	}
}
