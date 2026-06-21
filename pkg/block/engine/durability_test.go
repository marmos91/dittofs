package engine

import (
	"testing"

	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

func TestEngine_LocalDurable_MemoryDefaultsFalse(t *testing.T) {
	localStore := localmemory.New()
	fbs := newStubFileBlockStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Syncer: syncer, FileBlockStore: fbs})
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
	fbs := newStubFileBlockStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Syncer: syncer, FileBlockStore: fbs})
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
	fbs := newStubFileBlockStore()
	syncer := NewSyncer(localStore, remoteStore, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Remote: remoteStore, Syncer: syncer, FileBlockStore: fbs})
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
	fbs := newStubFileBlockStore()
	syncer := NewSyncer(localStore, remoteStore, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Remote: remoteStore, Syncer: syncer, FileBlockStore: fbs})
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
	fbs := newStubFileBlockStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{Local: localStore, Syncer: syncer, FileBlockStore: fbs})
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
