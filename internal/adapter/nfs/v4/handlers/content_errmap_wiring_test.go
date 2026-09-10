package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// The block store was closed under an in-flight op (share removed/hot-reloaded
// mid-transfer): the reply must draw NFS4ERR_STALE via the shared content
// mapper, not the literal NFS4ERR_IO the handlers used to return. Each test
// closes the per-share store after setup, then drives the handler and asserts
// the wire status.

// mapperBoundary pins the seam this wiring depends on: the content mapper
// keeps the established classification (unknown/ErrRemoteUnavailable → IO)
// while lifting ErrStoreClosed to STALE. MapToNFS4 would not — its
// lookupErrorRow defaults a non-StoreError to NFS4ERR_SERVERFAULT — which is
// why the handlers route through MapContentToNFS4.
func TestContentMapperBoundary_RemoteUnavailableStaysIO(t *testing.T) {
	if got := common.MapContentToNFS4(block.ErrRemoteUnavailable); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(ErrRemoteUnavailable) = %d, want NFS4ERR_IO (%d)",
			got, nfs4types.NFS4ERR_IO)
	}
	if got := common.MapContentToNFS4(engine.ErrStoreClosed); got != nfs4types.NFS4ERR_STALE {
		t.Errorf("MapContentToNFS4(ErrStoreClosed) = %d, want NFS4ERR_STALE (%d)",
			got, nfs4types.NFS4ERR_STALE)
	}
}

func TestRead_ClosedStore_ReturnsStale(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "closed-read.txt", 0o644, 0, 0)
	fx.writeContent(t, fileHandle, []byte("readable before the store closes"))

	if err := fx.blockStore.Close(); err != nil {
		t.Fatalf("close block store: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeReadArgs(&anonymousStateid, 0, 1024)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != nfs4types.NFS4ERR_STALE {
		t.Fatalf("READ on closed store status = %d, want NFS4ERR_STALE (%d)",
			result.Status, nfs4types.NFS4ERR_STALE)
	}
}

func TestWrite_ClosedStore_ReturnsStale(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "closed-write.txt", 0o644, 0, 0)

	if err := fx.blockStore.Close(); err != nil {
		t.Fatalf("close block store: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeWriteArgs(&anonymousStateid, 0, nfs4types.UNSTABLE4, []byte("arrives after close"))
	result := fx.handler.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != nfs4types.NFS4ERR_STALE {
		t.Fatalf("WRITE on closed store status = %d, want NFS4ERR_STALE (%d)",
			result.Status, nfs4types.NFS4ERR_STALE)
	}
}

func TestCommit_ClosedStore_ReturnsStale(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "closed-commit.txt", 0o644, 0, 0)
	fx.writeContent(t, fileHandle, []byte("dirty data awaiting the flush"))

	if err := fx.blockStore.Close(); err != nil {
		t.Fatalf("close block store: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeCommitArgs(0, 0)
	result := fx.handler.handleCommit(ctx, bytes.NewReader(args))

	if result.Status != nfs4types.NFS4ERR_STALE {
		t.Fatalf("COMMIT on closed store status = %d, want NFS4ERR_STALE (%d)",
			result.Status, nfs4types.NFS4ERR_STALE)
	}
}

func TestReadPlus_ClosedStore_ReturnsStale(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "closed-rp.txt", 0o644, 0, 0)
	// Write through the handler so CommitWrite populates the metadata block
	// list: the segmentation then yields a data segment whose read hits the
	// closed store (a fixture-level write would leave file.Blocks empty and
	// the window would succeed as all-hole).
	wctx := newRealFSContext(0, 0)
	wctx.CurrentFH = make([]byte, len(fileHandle))
	copy(wctx.CurrentFH, fileHandle)
	wres := fx.handler.handleWrite(wctx, bytes.NewReader(encodeWriteArgs(&anonymousStateid, 0, nfs4types.UNSTABLE4, bytes.Repeat([]byte("x"), 4096))))
	if wres.Status != nfs4types.NFS4_OK {
		t.Fatalf("WRITE for READ_PLUS setup: status = %d", wres.Status)
	}
	// Give the file a non-empty CAS block list (one row covering the payload):
	// after a WRITE the bytes live only in the journal tier and the metadata
	// projection is empty, so the segmentation would fall back to nothing and
	// the window would succeed as all-hole without ever touching the store.
	file, err := fx.metaSvc.GetFile(wctx.Context, fileHandle)
	if err != nil {
		t.Fatalf("get file for READ_PLUS setup: %v", err)
	}
	row := &block.FileChunk{
		ID:        string(file.PayloadID) + "/0",
		State:     block.BlockStateRemote,
		DataSize:  4096,
		RefCount:  1,
		CreatedAt: time.Now(),
	}
	if err := fx.store.Put(context.Background(), row); err != nil {
		t.Fatalf("put chunk row: %v", err)
	}
	file.Blocks = []block.ChunkRef{{Hash: row.Hash, Offset: 0, Size: 4096}}
	if err := fx.store.SetManifest(context.Background(), file); err != nil {
		t.Fatalf("set manifest: %v", err)
	}

	if err := fx.blockStore.Close(); err != nil {
		t.Fatalf("close block store: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeReadArgs(&anonymousStateid, 0, 1024)
	result := fx.handler.handleReadPlus(ctx, bytes.NewReader(args))

	if result.Status != nfs4types.NFS4ERR_STALE {
		t.Fatalf("READ_PLUS on closed store status = %d, want NFS4ERR_STALE (%d)",
			result.Status, nfs4types.NFS4ERR_STALE)
	}
}

func TestDeallocate_ClosedStore_ReturnsStale(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "closed-dealloc.txt", 0o644, 0, 0)
	fx.writeContent(t, fileHandle, bytes.Repeat([]byte("punch me"), 512))

	if err := fx.blockStore.Close(); err != nil {
		t.Fatalf("close block store: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	res := fx.handler.handleDeallocate(ctx, encAllocArgs(&anonymousStateid, 0, 1024))
	if res.Status != nfs4types.NFS4ERR_STALE {
		t.Fatalf("DEALLOCATE on closed store status = %d, want NFS4ERR_STALE (%d)",
			res.Status, nfs4types.NFS4ERR_STALE)
	}
}

// contextTests keep the raw context cancellation pass-through covered: a
// canceled read still surfaces IO (the fallback), never STALE, because
// cancellation is not a dead-handle signal.
func TestRead_CanceledContext_FallsBackToIO(t *testing.T) {
	if got := common.MapContentToNFS4(context.Canceled); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(context.Canceled) = %d, want NFS4ERR_IO (%d)",
			got, nfs4types.NFS4ERR_IO)
	}
}

var _ = xdr.DecodeUint32 // keep the xdr import if assertions move to encoded status
