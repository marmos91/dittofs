package runtime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestDeferredSizeCommit_NonsenseExtentWithholdsEverything covers a resolver
// that breaks its contract and reports a negative extent. The resolver is
// installed through exported API, so the value crossing into the clamp is not
// fixed by the journal's construction — and widening a negative straight to
// uint64 yields a maximal bound, which would pass every size through and turn
// the clamp into a silent no-op. An unusable answer has to withhold everything
// rather than everything-but-nothing.
func TestDeferredSizeCommit_NonsenseExtentWithholdsEverything(t *testing.T) {
	const n = 64 * 1024
	meta, metaType := newBadgerByteVerifyBackend(t).open(t)
	fx := newByteVerifyFixture(t, meta, metaType)
	defer fx.close()

	ctx := context.Background()
	pid := fx.createEmptyFile(ctx, "nonsense.bin")
	root, err := fx.meta.GetRootHandle(ctx, fx.shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	handle, err := fx.meta.GetChild(ctx, root, "nonsense.bin")
	if err != nil {
		t.Fatalf("GetChild: %v", err)
	}

	metaSvc := fx.rt.GetMetadataService()
	metaSvc.SetDurableExtentResolver(func(string, metadata.PayloadID) (int64, bool) {
		return -1, true
	})

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}

	writeOp, err := metaSvc.PrepareWrite(authCtx, handle, uint64(n))
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if err := common.WriteToBlockStore(ctx, fx.bs, pid, distinctBytes(n, 0x77), 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle, false); err != nil {
		t.Fatalf("FlushPendingWriteForFile: %v", err)
	}

	if got := fx.getFile(ctx, "nonsense.bin").Size; got != 0 {
		t.Fatalf("persisted size = %d on a negative extent, want 0 — the widened "+
			"negative became a maximal bound and disabled the clamp", got)
	}
}

// TestDeferredSizeCommit_NeverOutrunsDurableData is the merge gate for the
// crash-ordering half of the silent-zeros family.
//
// Modelled scenario: the ordinary write ack. An SMB WRITE (and an NFS UNSTABLE
// WRITE) buffers the bytes in the journal without an fsync, then flushes the
// pending metadata on the deferred path — and that metadata commit reaches
// stable storage on its own schedule, within the metadata store's background
// sync interval or immediately on a backend with no relaxed mode. If the size
// gets there and the bytes do not, the crash leaves a file whose size covers a
// range the block store has nothing for: an ordinary sparse hole, which every
// read zero-fills and reports as success. The client is handed plausible zeros
// in place of its data with no error anywhere.
//
// Gate property: without the commit-time clamp the persisted size is the full
// write while the durable extent is zero, and the first assertion fails. The
// second half then proves the clamp is a delay and not a truncation — once the
// data is committed, the very next flush publishes the full size.
func TestDeferredSizeCommit_NeverOutrunsDurableData(t *testing.T) {
	const n = 256 * 1024
	for _, bk := range byteVerifyBackends(t) {
		bk := bk
		t.Run(bk.name, func(t *testing.T) {
			if bk.skip != "" {
				t.Skip(bk.skip)
			}

			meta, metaType := bk.open(t)
			fx := newByteVerifyFixture(t, meta, metaType)
			defer fx.close()

			ctx := context.Background()
			data := distinctBytes(n, 0x91)
			pid := fx.createEmptyFile(ctx, "acked.bin")

			root, err := fx.meta.GetRootHandle(ctx, fx.shareName)
			if err != nil {
				t.Fatalf("GetRootHandle: %v", err)
			}
			handle, err := fx.meta.GetChild(ctx, root, "acked.bin")
			if err != nil {
				t.Fatalf("GetChild: %v", err)
			}
			authCtx := &metadata.AuthContext{
				Context:    ctx,
				AuthMethod: "unix",
				Identity: &metadata.Identity{
					UID:  metadata.Uint32Ptr(0),
					GID:  metadata.Uint32Ptr(0),
					GIDs: []uint32{0},
				},
				ClientAddr: "127.0.0.1",
			}
			metaSvc := fx.rt.GetMetadataService()

			// The write ack, exactly as the adapters perform it: prepare, put the
			// bytes in the block store, commit the intent, flush the metadata on
			// the deferred path. No CommitBlockStore — nothing fsynced the data.
			writeOp, err := metaSvc.PrepareWrite(authCtx, handle, uint64(n))
			if err != nil {
				t.Fatalf("PrepareWrite: %v", err)
			}
			if err := common.WriteToBlockStore(ctx, fx.bs, pid, data, 0); err != nil {
				t.Fatalf("WriteToBlockStore: %v", err)
			}
			if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
				t.Fatalf("CommitWrite: %v", err)
			}
			if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle, false); err != nil {
				t.Fatalf("FlushPendingWriteForFile: %v", err)
			}

			// Precondition: the ack really did leave the bytes non-durable.
			durable, ok := fx.bs.DurableExtent(ctx, pid)
			if !ok {
				t.Fatalf("block store cannot report a durable extent — the test cannot model the bug")
			}
			if durable != 0 {
				t.Fatalf("durable extent = %d after an uncommitted write, want 0 "+
					"(the write path fsynced, so this no longer models a deferred ack)", durable)
			}

			// The gate. A persisted size above the durable extent is the bug: the
			// size survives the crash, the bytes do not, and the gap reads as zeros.
			if got := fx.getFile(ctx, "acked.bin").Size; got > uint64(durable) {
				t.Fatalf("persisted size %d exceeds the durable extent %d: a crash here leaves "+
					"the range readable as zeros with no error", got, durable)
			}

			// Readers still see the acknowledged size — it is held in the pending
			// state, not thrown away.
			if pending, ok := metaSvc.GetPendingSize(handle); !ok || pending != uint64(n) {
				t.Fatalf("pending size = (%d, %v), want (%d, true) — the ACK'd size must stay visible",
					pending, ok, n)
			}

			// And it is a delay, not a truncation: commit the data and the next
			// flush publishes the full size.
			if err := common.CommitBlockStore(ctx, fx.bs, pid); err != nil {
				t.Fatalf("CommitBlockStore: %v", err)
			}
			if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle, false); err != nil {
				t.Fatalf("FlushPendingWriteForFile after commit: %v", err)
			}
			if got := fx.getFile(ctx, "acked.bin").Size; got != uint64(n) {
				t.Fatalf("persisted size = %d after the data was committed, want %d "+
					"(the clamp must release the size, not drop it)", got, n)
			}
		})
	}
}
