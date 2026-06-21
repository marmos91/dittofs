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

// --- Default policy (require_durable_commit = false) ---------------------
//
// This is the shipped default and the case that previously EIO'd pjdfstest:
// strict durability enforcement is OPT-IN, so after a successful Flush the
// commit seam acks unconditionally regardless of local/remote durability.

// TestCommitBlockStore_Default_MemoryLocal_NoRemote_ReturnsNil is the exact
// pjdfstest-breaking case: a volatile memory-local store with NO remote and
// the default policy must return nil (NOT ErrNotDurableYet) from a CLOSE/
// COMMIT after a successful flush.
func TestCommitBlockStore_Default_MemoryLocal_NoRemote_ReturnsNil(t *testing.T) {
	bs := newMemoryEngine(t, nil, nil) // memory local, no remote, default policy
	payloadID := "default-mem-local-no-remote"
	writePayload(t, bs, payloadID)

	if bs.RequireDurableCommit() {
		t.Fatal("default policy must be require_durable_commit=false")
	}
	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("default policy: memory-local + no remote should ack (nil); got %v", err)
	}
}

// TestCommitBlockStore_Default_MemoryLocal_NonDurableRemote_ReturnsNil asserts
// that under the default policy even a non-durable remote does not block the
// ack — the mirror stays async.
func TestCommitBlockStore_Default_MemoryLocal_NonDurableRemote_ReturnsNil(t *testing.T) {
	remote := remotememory.New() // memory remote: NOT durable by default
	bs := newMemoryEngine(t, remote, nil)
	payloadID := "default-mem-local-nondurable-remote"
	writePayload(t, bs, payloadID)

	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("default policy: memory-local + non-durable remote should ack (nil); got %v", err)
	}
}

// --- Strict policy (require_durable_commit = true) ------------------------
//
// Opt-in honest enforcement: CLOSE/COMMIT only succeeds when the data is on a
// durable store (localDurable || (Finalized && remoteDurable)).

// strict enables the opt-in honest-durability policy on bs and returns it.
func strict(bs *engine.Store) *engine.Store {
	bs.SetRequireDurableCommit(true)
	return bs
}

// TestCommitBlockStore_Strict_FSLocal_NoRemote_ReturnsNil asserts the FAST
// path under strict mode: a durable local (fs) store acks immediately
// regardless of remote state. fs-local is always durable so the strict flag is
// a no-op there.
func TestCommitBlockStore_Strict_FSLocal_NoRemote_ReturnsNil(t *testing.T) {
	bs := strict(newTestEngine(t)) // fs-backed local, nil remote
	payloadID := "strict-fs-local-no-remote"
	writePayload(t, bs, payloadID)

	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("strict fs-local CommitBlockStore should return nil (durable, fast); got %v", err)
	}
	if !bs.LocalDurable() {
		t.Fatal("fs local store must be durable")
	}
}

// TestCommitBlockStore_Strict_MemoryLocal_HealthyDurableRemote_ReturnsNil
// asserts that under strict mode a volatile local store reaching a durable
// remote (Finalized=true) commits.
func TestCommitBlockStore_Strict_MemoryLocal_HealthyDurableRemote_ReturnsNil(t *testing.T) {
	remote := remotememory.New()
	remote.SetDurable(true) // simulate a durable remote (s3 type-default)
	bs := strict(newMemoryEngine(t, remote, nil))
	payloadID := "strict-mem-local-durable-remote"
	writePayload(t, bs, payloadID)

	if !bs.RemoteDurable() {
		t.Fatal("remote should report durable after SetDurable(true)")
	}
	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("strict memory-local + durable remote (Finalized) should commit; got %v", err)
	}
}

// TestCommitBlockStore_Strict_MemoryLocal_NonDurableRemote_NotDurableYet
// asserts that under strict mode even a Finalized flush to a NON-durable
// remote does not commit.
func TestCommitBlockStore_Strict_MemoryLocal_NonDurableRemote_NotDurableYet(t *testing.T) {
	remote := remotememory.New() // memory remote: NOT durable by default
	bs := strict(newMemoryEngine(t, remote, nil))
	payloadID := "strict-mem-local-nondurable-remote"
	writePayload(t, bs, payloadID)

	if bs.RemoteDurable() {
		t.Fatal("memory remote should NOT be durable by default")
	}
	err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID))
	if !errors.Is(err, ErrNotDurableYet) {
		t.Fatalf("strict memory-local + non-durable remote should be ErrNotDurableYet; got %v", err)
	}
}

// TestCommitBlockStore_Strict_MemoryLocal_NoRemote_NotDurableYet asserts the
// honest failure under strict mode when nothing durable backs the data.
func TestCommitBlockStore_Strict_MemoryLocal_NoRemote_NotDurableYet(t *testing.T) {
	bs := strict(newMemoryEngine(t, nil, nil))
	payloadID := "strict-mem-local-no-remote"
	writePayload(t, bs, payloadID)

	err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID))
	if !errors.Is(err, ErrNotDurableYet) {
		t.Fatalf("strict memory-local + no remote should be ErrNotDurableYet; got %v", err)
	}
}

// TestCommitBlockStore_Strict_ConfigOverride_FlipsBehavior asserts the
// per-store durable override changes the commit decision under strict mode: a
// memory local store marked durable=true now acks on the fast path (no remote
// required).
func TestCommitBlockStore_Strict_ConfigOverride_FlipsBehavior(t *testing.T) {
	durable := true
	bs := strict(newMemoryEngine(t, nil, &durable)) // memory local, FORCED durable, no remote
	payloadID := "strict-mem-local-override-durable"
	writePayload(t, bs, payloadID)

	if !bs.LocalDurable() {
		t.Fatal("memory local store with override durable=true should report durable")
	}
	if err := CommitBlockStore(context.Background(), bs, metadata.PayloadID(payloadID)); err != nil {
		t.Fatalf("strict override durable=true should commit on the fast path; got %v", err)
	}

	// And the inverse: fs local forced durable=false under strict mode now
	// requires a durable remote that it does not have → ErrNotDurableYet.
	fsBS := strict(newTestEngine(t))
	if fsLocal := fsBS.LocalForTest(); fsLocal != nil {
		if setter, ok := fsLocal.(interface{ SetDurable(bool) }); ok {
			setter.SetDurable(false)
		} else {
			t.Fatal("fs local store must support SetDurable")
		}
	}
	fsPayload := "strict-fs-local-override-nondurable"
	writePayload(t, fsBS, fsPayload)
	err := CommitBlockStore(context.Background(), fsBS, metadata.PayloadID(fsPayload))
	if !errors.Is(err, ErrNotDurableYet) {
		t.Fatalf("strict fs local forced durable=false + no remote should be ErrNotDurableYet; got %v", err)
	}
}
