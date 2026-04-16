// NFSv4 PUTFH tests — REST-02 adapter-side enforcement (Plan 05-09 D-02).
//
// PUTFH resolves a client-presented filehandle into the CompoundContext's
// CurrentFH for subsequent operations. When the owning share has been
// quiesced for restore (Share.Enabled=false), PUTFH must refuse with
// NFS4ERR_STALE so clients re-acquire fresh handles after the restore
// completes and the operator explicitly re-enables the share.
package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	memorymeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newPutFHTestHandler builds a v4 Handler with a single share.
// Returns the handler, the encoded share root handle (valid PUTFH input),
// and the runtime share so tests can toggle Enabled.
func newPutFHTestHandler(t *testing.T, shareName string) (*Handler, []byte, *runtime.Share) {
	t.Helper()

	ctx := context.Background()
	rt := runtime.New(nil)

	metaStore := memorymeta.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	cfg := &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		Enabled:       true,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}
	if err := rt.AddShare(ctx, cfg); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	share, err := rt.GetShare(shareName)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}

	pfs := pseudofs.New()
	pfs.Rebuild([]string{shareName})
	h := NewHandler(rt, pfs)

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	return h, []byte(rootHandle), share
}

// encodePutFHArgsBytes encodes a filehandle as the XDR opaque arg expected
// by handlePutFH (the PUTFH body is a single opaque filehandle).
func encodePutFHArgsBytes(t *testing.T, fh []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := xdr.WriteXDROpaque(&buf, fh); err != nil {
		t.Fatalf("encode PUTFH arg: %v", err)
	}
	return buf.Bytes()
}

func TestPUTFH_DisabledShare_ReturnsStale(t *testing.T) {
	h, rootHandle, share := newPutFHTestHandler(t, "/disabled")
	share.Enabled = false

	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: "127.0.0.1:1234"}
	res := h.handlePutFH(ctx, bytes.NewReader(encodePutFHArgsBytes(t, rootHandle)))
	if res == nil {
		t.Fatal("handlePutFH returned nil result")
	}
	if res.Status != types.NFS4ERR_STALE {
		t.Errorf("Status = %d, want NFS4ERR_STALE (%d)", res.Status, types.NFS4ERR_STALE)
	}
	if res.OpCode != types.OP_PUTFH {
		t.Errorf("OpCode = %d, want OP_PUTFH (%d)", res.OpCode, types.OP_PUTFH)
	}
	if ctx.CurrentFH != nil {
		t.Errorf("CurrentFH was set despite refusal: %x", ctx.CurrentFH)
	}
}

func TestPUTFH_EnabledShare_Succeeds(t *testing.T) {
	h, rootHandle, _ := newPutFHTestHandler(t, "/export")
	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: "127.0.0.1:1234"}
	res := h.handlePutFH(ctx, bytes.NewReader(encodePutFHArgsBytes(t, rootHandle)))
	if res.Status != types.NFS4_OK {
		t.Fatalf("Status = %d, want NFS4_OK (%d)", res.Status, types.NFS4_OK)
	}
	if !bytes.Equal(ctx.CurrentFH, rootHandle) {
		t.Errorf("CurrentFH mismatch: got %x, want %x", ctx.CurrentFH, rootHandle)
	}
}
