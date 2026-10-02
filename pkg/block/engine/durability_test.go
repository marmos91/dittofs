package engine

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal/journaltest"
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
		fx := newCarveFixture(t, mem, defaultTestCarveBlockSize) // durable fs local, ManualSync, wired carve
		bs, err := New(BlockStoreConfig{
			Local:           fx.local,
			Remote:          mem,
			RemoteSync:      fx.syncer,
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

func TestEngine_LocalDurable_JournalDefaultsTrue(t *testing.T) {
	localStore := journaltest.New(t)
	fbs := newStubFileChunkStore()
	syncer := NewRemoteSync(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, RemoteSync: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if !bs.LocalDurable() {
		t.Fatal("journal local store fsyncs its segments, so it must report durable by default")
	}
	if bs.RemoteDurable() {
		t.Fatal("nil remote must report NOT durable")
	}
}

func TestEngine_LocalDurable_OverrideFalse(t *testing.T) {
	localStore := journaltest.New(t)
	localStore.SetDurable(false) // operator override for a store on volatile media
	fbs := newStubFileChunkStore()
	syncer := NewRemoteSync(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, RemoteSync: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if bs.LocalDurable() {
		t.Fatal("journal local store with SetDurable(false) should report NOT durable")
	}
}

func TestEngine_RemoteDurable_MemoryDefaultsFalse(t *testing.T) {
	localStore := journaltest.New(t)
	remoteStore := remotememory.New()
	fbs := newStubFileChunkStore()
	syncer := NewRemoteSync(localStore, remoteStore, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Remote: remoteStore, RemoteSync: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if bs.RemoteDurable() {
		t.Fatal("memory remote store should report NOT durable by default")
	}
}

func TestEngine_RemoteDurable_OverrideTrue(t *testing.T) {
	localStore := journaltest.New(t)
	remoteStore := remotememory.New()
	remoteStore.SetDurable(true) // simulate a durable remote (s3 type-default)
	fbs := newStubFileChunkStore()
	syncer := NewRemoteSync(localStore, remoteStore, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Remote: remoteStore, RemoteSync: syncer, FileChunkStore: fbs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	if !bs.RemoteDurable() {
		t.Fatal("remote store with SetDurable(true) should report durable")
	}
}

func TestEngine_RequireDurableCommit_DefaultsFalse(t *testing.T) {
	localStore := journaltest.New(t)
	fbs := newStubFileChunkStore()
	syncer := NewRemoteSync(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, RemoteSync: syncer, FileChunkStore: fbs})
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
