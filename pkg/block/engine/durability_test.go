package engine

import (
	"context"
	"testing"

	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestEngine_Flush_DurableLocalDefault_NoSyncRemote proves the #1621 fix: on a
// durable local store under the default (async-remote) policy, Flush satisfies
// durability with the local fsync alone and does NOT block the ack on a
// synchronous remote carve/upload — that inline S3 PutObject-per-FILE_SYNC-WRITE
// was the multi-second write stall. A strict share still drains inline.
//
// Discriminator: the store's background carve loop is never Start()ed, so the
// only path to the remote is the synchronous carve inside Flush itself. Rollup
// is forced first so there are carve-ready chunks; a non-empty remote after
// Flush therefore means the drain ran synchronously on the ack path.
func TestEngine_Flush_DurableLocalDefault_NoSyncRemote(t *testing.T) {
	writeAndFlush := func(t *testing.T, strict bool) int {
		t.Helper()
		ctx := context.Background()
		mem := remotememory.New()
		mem.SetDurable(true)
		fx := newCarveFixture(t, mem, DefaultBlockCarveBytes) // durable fs local, ManualSync, wired carve
		bs, err := New(BlockStoreConfig{
			Local:           fx.local,
			Remote:          mem,
			Syncer:          fx.syncer,
			FileChunkStore:  fx.ms,
			SyncedHashStore: fx.ms,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = bs.Close() })
		bs.SetRequireDurableCommit(strict)

		// Register a carve-ready CAS chunk exactly as the rollup's onChunkComplete
		// hook would — the state a FILE_SYNC WRITE reaches by the time it flushes.
		fx.storeChunk(t, ctx, []byte("hello world"))
		if _, err := bs.Flush(ctx, "share/p1"); err != nil {
			t.Fatalf("Flush(strict=%v): %v", strict, err)
		}
		return countRemoteBlocks(t, ctx, mem)
	}

	if n := writeAndFlush(t, false); n != 0 {
		t.Fatalf("default policy: Flush must not synchronously upload to remote, got %d block(s)", n)
	}
	if n := writeAndFlush(t, true); n == 0 {
		t.Fatal("strict policy: Flush must synchronously drain to remote, got 0 blocks")
	}
}

func TestEngine_LocalDurable_MemoryDefaultsFalse(t *testing.T) {
	localStore := localmemory.New()
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Syncer: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if bs.LocalDurable() {
		t.Fatal("memory local store should report NOT durable by default")
	}
	if bs.RemoteDurable() {
		t.Fatal("nil remote must report NOT durable")
	}
}

func TestEngine_LocalDurable_OverrideTrue(t *testing.T) {
	localStore := localmemory.New()
	localStore.SetDurable(true) // operator override
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Syncer: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if !bs.LocalDurable() {
		t.Fatal("memory local store with SetDurable(true) should report durable")
	}
}

func TestEngine_RemoteDurable_MemoryDefaultsFalse(t *testing.T) {
	localStore := localmemory.New()
	remoteStore := remotememory.New()
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, remoteStore, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Remote: remoteStore, Syncer: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if bs.RemoteDurable() {
		t.Fatal("memory remote store should report NOT durable by default")
	}
}

func TestEngine_RemoteDurable_OverrideTrue(t *testing.T) {
	localStore := localmemory.New()
	remoteStore := remotememory.New()
	remoteStore.SetDurable(true) // simulate a durable remote (s3 type-default)
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, remoteStore, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Remote: remoteStore, Syncer: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if !bs.RemoteDurable() {
		t.Fatal("remote store with SetDurable(true) should report durable")
	}
}

func TestEngine_RequireDurableCommit_DefaultsFalse(t *testing.T) {
	localStore := localmemory.New()
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Syncer: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if bs.RequireDurableCommit() {
		t.Fatal("require_durable_commit must default to false")
	}
	bs.SetRequireDurableCommit(true)
	if !bs.RequireDurableCommit() {
		t.Fatal("SetRequireDurableCommit(true) should flip the policy")
	}
	bs.SetRequireDurableCommit(false)
	if bs.RequireDurableCommit() {
		t.Fatal("SetRequireDurableCommit(false) should clear the policy")
	}
}
