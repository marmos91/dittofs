package handlers

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// Use the runtime's engine so CLONE exercises the manifest coordinator and
// journal drain as well as the protocol handler.
func newCloneRangeFixture(t *testing.T) *ioTestFixture {
	t.Helper()
	rt, bsID := newTestShareRuntime(t)
	store := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("clone-meta", store); err != nil {
		t.Fatal(err)
	}
	const share = "/export"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: share, MetadataStore: "clone-meta", BlockStoreID: bsID,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{UID: 1000, GID: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	root, err := rt.GetRootHandle(share)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := rt.GetBlockStoreForShare(share)
	if err != nil {
		t.Fatal(err)
	}
	pfs := pseudofs.New()
	pfs.Rebuild([]string{share})
	rt.GetMetadataService().SetDeferredCommit(false)
	return &ioTestFixture{
		handler: NewHandler(rt, pfs), metaSvc: rt.GetMetadataService(),
		blockStore: bs, store: store, rootHandle: root, shareName: share,
	}
}

func TestCloneDestinationRange(t *testing.T) {
	for _, dstSize := range []int{2048, 4096, 8192} {
		for _, count := range []uint64{0, 4096} {
			t.Run(fmt.Sprintf("dst=%d/count=%d", dstSize, count), func(t *testing.T) {
				fx := newCloneRangeFixture(t)
				src := fx.createRegularFile(t, fx.rootHandle, "src", 0o666, 1000, 1000)
				dst := fx.createRegularFile(t, fx.rootHandle, "dst", 0o666, 1000, 1000)
				source := bytes.Repeat([]byte{0x11}, 4096)
				previous := bytes.Repeat([]byte{0x22}, dstSize)
				fx.writeContent(t, src, source)
				fx.writeContent(t, dst, previous)

				ctx := newRealFSContext(1000, 1000)
				ctx.MinorVersion = 2
				ctx.CurrentFH, ctx.SavedFH = dst, src
				res := fx.handler.handleClone(ctx, encCloneArgs(anonStateid(), anonStateid(), 0, 0, count))
				wantStatus, wantBytes := uint32(types.NFS4_OK), source
				if dstSize > len(source) {
					wantStatus, wantBytes = types.NFS4ERR_NOTSUPP, previous
				}
				if res.Status != wantStatus {
					t.Errorf("CLONE status = %d, want %d", res.Status, wantStatus)
				}
				file, err := fx.metaSvc.GetFile(ctx.Context, dst)
				if err != nil {
					t.Fatal(err)
				}
				if file.Size != uint64(len(wantBytes)) {
					t.Errorf("destination size = %d, want %d", file.Size, len(wantBytes))
				}
				got := make([]byte, len(wantBytes))
				if _, err := fx.blockStore.ReadAt(ctx.Context, string(file.PayloadID), got, 0); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, wantBytes) {
					t.Error("destination content changed outside the supported clone range")
				}
			})
		}
	}
}

func TestClonePreservesPendingDestinationTail(t *testing.T) {
	fx := newCloneRangeFixture(t)
	src := fx.createRegularFile(t, fx.rootHandle, "src", 0o666, 1000, 1000)
	dst := fx.createRegularFile(t, fx.rootHandle, "dst", 0o666, 1000, 1000)
	fx.writeContent(t, src, bytes.Repeat([]byte{0x11}, 4096))
	previous := bytes.Repeat([]byte{0x22}, 4096)
	fx.writeContent(t, dst, previous)
	fx.metaSvc.SetDeferredCommit(true)
	ctx := newRealFSContext(1000, 1000)
	ctx.MinorVersion = 2
	ctx.CurrentFH, ctx.SavedFH = dst, src
	tail := bytes.Repeat([]byte{0x33}, 4096)
	write := fx.handler.handleWrite(ctx, bytes.NewReader(encodeWriteArgs(anonStateid(), 4096, types.UNSTABLE4, tail)))
	if write.Status != types.NFS4_OK {
		t.Fatalf("WRITE status = %d", write.Status)
	}
	if size, ok := fx.metaSvc.GetPendingSize(dst); !ok || size != 8192 {
		t.Fatalf("expected deferred tail: pending size=%d present=%v", size, ok)
	}
	res := fx.handler.handleClone(ctx, encCloneArgs(anonStateid(), anonStateid(), 0, 0, 4096))
	if res.Status != types.NFS4ERR_NOTSUPP {
		t.Fatalf("CLONE status = %d, want NOTSUPP", res.Status)
	}
	file, err := fx.metaSvc.GetFile(ctx.Context, dst)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 8192)
	if _, err := fx.blockStore.ReadAt(ctx.Context, string(file.PayloadID), got, 0); err != nil {
		t.Fatal(err)
	}
	if file.Size != 8192 || !bytes.Equal(got, append(previous, tail...)) {
		t.Error("rejected clone changed the acknowledged destination content")
	}
}
