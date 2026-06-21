package common

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newMemoryEngine builds an engine with an in-memory local store (NOT durable)
// and the given remote (may be nil). The metadata memory store provides both
// the FileBlockStore and SyncedHashStore so the syncer's mirror loop can run
// and report Finalized=true after a write+flush.
func newMemoryEngine(t *testing.T, remote *remotememory.Store, durableLocalOverride *bool) *engine.Store {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore := localmemory.New()
	if durableLocalOverride != nil {
		localStore.SetDurable(*durableLocalOverride)
	}

	cfg := engine.BlockStoreConfig{
		Local:           localStore,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
	}
	if remote != nil {
		cfg.Remote = remote
		cfg.Syncer = engine.NewSyncer(localStore, remote, ms, engine.DefaultConfig())
	} else {
		cfg.Syncer = engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())
	}

	bs, err := engine.New(cfg)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

func writePayload(t *testing.T, bs *engine.Store, payloadID string) {
	t.Helper()
	data := []byte("durability-matrix-payload-bytes")
	if err := WriteToBlockStore(context.Background(), bs, metadata.PayloadID(payloadID), data, 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}
}

// TestCommitBlockStore_FSLocal_NoRemote_ReturnsNil asserts the FAST path: a
// durable local (fs) store acks immediately regardless of remote state. This is
// the production hot path — ErrNotDurableYet must NEVER occur.
func TestCommitBlockStore_FSLocal_NoRemote_ReturnsNil(t *testing.T) {
	bs := newTestEngine(t) // fs-backed local, nil remote
	payloadID := "fs-local-no-remote"
	writePayload(t, bs, payloadID)

	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("fs-local CommitBlockStore should return nil (durable, fast); got %v", err)
	}
	if !bs.LocalDurable() {
		t.Fatal("fs local store must be durable")
	}
}

// TestCommitBlockStore_MemoryLocal_HealthyDurableRemote_ReturnsNil asserts that
// a volatile local store reaching a durable remote (Finalized=true) commits.
func TestCommitBlockStore_MemoryLocal_HealthyDurableRemote_ReturnsNil(t *testing.T) {
	remote := remotememory.New()
	remote.SetDurable(true) // simulate a durable remote (s3 type-default)
	bs := newMemoryEngine(t, remote, nil)
	payloadID := "mem-local-durable-remote"
	writePayload(t, bs, payloadID)

	if !bs.RemoteDurable() {
		t.Fatal("remote should report durable after SetDurable(true)")
	}
	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("memory-local + durable remote (Finalized) should commit; got %v", err)
	}
}

// TestCommitBlockStore_MemoryLocal_NonDurableRemote_NotDurableYet asserts that
// even a Finalized flush to a NON-durable remote does not commit.
func TestCommitBlockStore_MemoryLocal_NonDurableRemote_NotDurableYet(t *testing.T) {
	remote := remotememory.New() // memory remote: NOT durable by default
	bs := newMemoryEngine(t, remote, nil)
	payloadID := "mem-local-nondurable-remote"
	writePayload(t, bs, payloadID)

	if bs.RemoteDurable() {
		t.Fatal("memory remote should NOT be durable by default")
	}
	err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID))
	if !errors.Is(err, ErrNotDurableYet) {
		t.Fatalf("memory-local + non-durable remote should be ErrNotDurableYet; got %v", err)
	}
}

// TestCommitBlockStore_MemoryLocal_NoRemote_NotDurableYet asserts the honest
// failure when nothing durable backs the data.
func TestCommitBlockStore_MemoryLocal_NoRemote_NotDurableYet(t *testing.T) {
	bs := newMemoryEngine(t, nil, nil)
	payloadID := "mem-local-no-remote"
	writePayload(t, bs, payloadID)

	err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID))
	if !errors.Is(err, ErrNotDurableYet) {
		t.Fatalf("memory-local + no remote should be ErrNotDurableYet; got %v", err)
	}
}

// TestCommitBlockStore_ConfigOverride_FlipsBehavior asserts the per-store
// durable override changes the commit decision: a memory local store marked
// durable=true now acks on the fast path (no remote required).
func TestCommitBlockStore_ConfigOverride_FlipsBehavior(t *testing.T) {
	durable := true
	bs := newMemoryEngine(t, nil, &durable) // memory local, FORCED durable, no remote
	payloadID := "mem-local-override-durable"
	writePayload(t, bs, payloadID)

	if !bs.LocalDurable() {
		t.Fatal("memory local store with override durable=true should report durable")
	}
	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("override durable=true should commit on the fast path; got %v", err)
	}

	// And the inverse: fs local forced durable=false now requires a durable
	// remote that it does not have → ErrNotDurableYet.
	fsBS := newTestEngine(t)
	if fsLocal := fsBS.LocalForTest(); fsLocal != nil {
		if setter, ok := fsLocal.(interface{ SetDurable(bool) }); ok {
			setter.SetDurable(false)
		} else {
			t.Fatal("fs local store must support SetDurable")
		}
	}
	fsPayload := "fs-local-override-nondurable"
	writePayload(t, fsBS, fsPayload)
	err := CommitBlockStore(context.Background(), fsBS, metadata.PayloadID(fsPayload))
	if !errors.Is(err, ErrNotDurableYet) {
		t.Fatalf("fs local forced durable=false + no remote should be ErrNotDurableYet; got %v", err)
	}
}
