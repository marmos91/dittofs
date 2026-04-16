// Package handlers — Mount procedure tests.
//
// These tests cover REST-02 adapter-side enforcement: a disabled share must
// refuse MOUNT requests with MNT3ERR_ACCES (MountErrAccess=13) so NFS clients
// cannot acquire a root handle via MOUNT when the backing metadata store has
// been quiesced for restore.
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newTestMountHandler constructs a Mount handler backed by a runtime with a
// single share. `enabled` controls the runtime Share.Enabled flag post-add
// so the REST-02 gate can be exercised without touching the control-plane DB.
func newTestMountHandler(t *testing.T, shareName string, enabled bool) (*Handler, context.Context) {
	t.Helper()

	ctx := context.Background()
	rt := runtime.New(nil)

	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	shareCfg := &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		Enabled:       true, // AddShare validates we can build root handle; flip below.
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}
	if err := rt.AddShare(ctx, shareCfg); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	// Flip runtime Enabled directly to model a disabled share without
	// round-tripping through DisableShare (which would need a ShareStore).
	share, err := rt.GetShare(shareName)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	share.Enabled = enabled

	return &Handler{Registry: rt}, ctx
}

// newMountCtx builds a minimal MountHandlerContext for the given request ctx.
func newMountCtx(reqCtx context.Context) *MountHandlerContext {
	uid := uint32(1000)
	gid := uint32(1000)
	return &MountHandlerContext{
		Context:    reqCtx,
		ClientAddr: "127.0.0.1:12345",
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{gid},
	}
}

// TestMount_DisabledShare_ReturnsAccess covers REST-02: a runtime share with
// Enabled=false must refuse MOUNT with MountErrAccess (MNT3ERR_ACCES=13) and
// must NOT return a root file handle. This is the adapter-side belt in the
// share-disabled-for-restore workflow (Plan 05-09 D-02).
func TestMount_DisabledShare_ReturnsAccess(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/disabled", false)

	resp, err := h.Mount(newMountCtx(ctx), &MountRequest{DirPath: "/disabled"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("Mount returned nil response")
	}
	if resp.Status != MountErrAccess {
		t.Errorf("Status = %d, want MountErrAccess (%d)", resp.Status, MountErrAccess)
	}
	if len(resp.FileHandle) != 0 {
		t.Errorf("FileHandle = %x, want empty on access-denied response", resp.FileHandle)
	}
}

// TestMount_EnabledShare_AllowsMount is the positive counterpart — a share
// with Enabled=true must continue to succeed (regression guard so the REST-02
// gate doesn't accidentally refuse all MOUNTs).
func TestMount_EnabledShare_AllowsMount(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)

	resp, err := h.Mount(newMountCtx(ctx), &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountOK {
		t.Fatalf("Status = %d, want MountOK (%d)", resp.Status, MountOK)
	}
	if len(resp.FileHandle) == 0 {
		t.Error("FileHandle is empty, want a non-empty root handle on success")
	}
}
