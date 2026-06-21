package shares

import (
	"context"
	"errors"
	"testing"

	adaptercommon "github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestNonClosingRemote_DelegatesDurable is the regression guard for #1274:
// production wraps EVERY remote store in *nonClosingRemote before handing it to
// the engine. Because nonClosingRemote embeds remote.RemoteStore (which has no
// Durable method) and previously overrode only Close(), it silently dropped the
// block.DurabilityReporter capability — so engine.Store.RemoteDurable() type-
// asserted to DurabilityReporter, failed, and reported NOT durable for EVERY
// production S3-remote share, yielding a spurious ErrNotDurableYet on every
// COMMIT/CLOSE even though the data reached durable storage.
//
// The pre-existing engine durability tests passed the remote UNWRAPPED, which is
// exactly why they missed this. This test wraps a durable remote in the real
// production wrapper and asserts the capability survives.
func TestNonClosingRemote_DelegatesDurable(t *testing.T) {
	durableRemote := remotememory.New()
	durableRemote.SetDurable(true) // simulate a durable remote (s3 type-default)

	wrapped := &nonClosingRemote{durableRemote}

	// 1. The wrapper itself must implement DurabilityReporter and report the
	//    wrapped store's durability — without this it is not a DurabilityReporter
	//    at all and block.IsDurable falls back to the conservative false.
	if _, ok := interface{}(wrapped).(block.DurabilityReporter); !ok {
		t.Fatal("*nonClosingRemote must implement block.DurabilityReporter")
	}
	if !wrapped.Durable() {
		t.Fatal("nonClosingRemote.Durable() must delegate true through the wrapped durable remote")
	}
	if !block.IsDurable(wrapped) {
		t.Fatal("block.IsDurable must see through *nonClosingRemote to the durable remote")
	}

	// Inverse: a non-durable remote must NOT be reported durable through the wrapper.
	nonDurable := &nonClosingRemote{remotememory.New()} // memory remote: NOT durable by default
	if nonDurable.Durable() {
		t.Fatal("nonClosingRemote wrapping a non-durable remote must report NOT durable")
	}
}

// TestNonClosingRemote_EngineRemoteDurableAndCommit builds the engine the way
// production does — memory-local + the durable remote wrapped in the production
// *nonClosingRemote — and asserts the honest-commit contract holds end to end:
// RemoteDurable() is TRUE and CommitBlockStore returns nil (not ErrNotDurableYet)
// for a Finalized write.
func TestNonClosingRemote_EngineRemoteDurableAndCommit(t *testing.T) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore := localmemory.New() // volatile local — durability must come from the remote
	durableRemote := remotememory.New()
	durableRemote.SetDurable(true)

	// This is the production wrap: engine holds *nonClosingRemote, not the bare remote.
	engineRemote := &nonClosingRemote{durableRemote}

	syncer := engine.NewSyncer(localStore, engineRemote, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          engineRemote,
		Syncer:          syncer,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	// The crux of #1274: with the wrapper in place, the engine must still see the
	// remote as durable.
	if !bs.RemoteDurable() {
		t.Fatal("engine.RemoteDurable() must be TRUE through the production *nonClosingRemote wrapper (#1274)")
	}
	if bs.LocalDurable() {
		t.Fatal("memory local store must report NOT durable (test premise: durability comes from the remote)")
	}

	// Write + commit: a Finalized flush to the durable wrapped remote must commit.
	payloadID := metadata.PayloadID("nonclosing-durable-remote")
	if err := adaptercommon.WriteToBlockStore(context.Background(), bs, payloadID, []byte("durable-via-wrapper"), 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}
	if err := adaptercommon.CommitBlockStore(context.Background(), bs, payloadID); err != nil {
		if errors.Is(err, adaptercommon.ErrNotDurableYet) {
			t.Fatalf("CommitBlockStore returned ErrNotDurableYet through the *nonClosingRemote wrapper — #1274 regression")
		}
		t.Fatalf("CommitBlockStore: %v", err)
	}
}
