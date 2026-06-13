package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// fakeCall implements the authCall interface consumed by ExtractV4HandlerContext.
type fakeCall struct {
	flavor   uint32
	authBody []byte
}

func (f *fakeCall) GetAuthFlavor() uint32 { return f.flavor }
func (f *fakeCall) GetAuthBody() []byte   { return f.authBody }

// TestExtractV4HandlerContext_GSSNoIdentityReturnsWRONGSEC verifies that when
// AuthFlavor==AuthRPCSECGSS but no GSS identity is present in the context,
// ExtractV4HandlerContext returns (nil, NFS4ERR_WRONGSEC) instead of a context
// with nil UID/GID that would let the COMPOUND proceed as anonymous.
func TestExtractV4HandlerContext_GSSNoIdentityReturnsWRONGSEC(t *testing.T) {
	ctx := context.Background() // no gss.ContextWithIdentity call — identity is nil

	call := &fakeCall{flavor: rpc.AuthRPCSECGSS}
	compCtx, status := ExtractV4HandlerContext(ctx, call, "127.0.0.1:1234")

	if status != types.NFS4ERR_WRONGSEC {
		t.Errorf("status = %d, want NFS4ERR_WRONGSEC (%d)", status, types.NFS4ERR_WRONGSEC)
	}
	if compCtx != nil {
		t.Errorf("compCtx = %+v, want nil on WRONGSEC rejection", compCtx)
	}
}

// TestExtractV4HandlerContext_GSSWithIdentitySucceeds verifies that when a
// verified GSS identity IS present in the context, ExtractV4HandlerContext
// propagates it and returns NFS4_OK.
func TestExtractV4HandlerContext_GSSWithIdentitySucceeds(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)
	identity := &metadata.Identity{UID: &uid, GID: &gid, GIDs: []uint32{1000, 2000}}
	ctx := gss.ContextWithIdentity(context.Background(), identity)

	call := &fakeCall{flavor: rpc.AuthRPCSECGSS}
	compCtx, status := ExtractV4HandlerContext(ctx, call, "127.0.0.1:1234")

	if status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK (%d)", status, types.NFS4_OK)
	}
	if compCtx == nil {
		t.Fatal("compCtx = nil, want non-nil context on successful GSS auth")
	}
	if compCtx.UID == nil || *compCtx.UID != 1000 {
		t.Errorf("UID = %v, want 1000", compCtx.UID)
	}
	if compCtx.GID == nil || *compCtx.GID != 1000 {
		t.Errorf("GID = %v, want 1000", compCtx.GID)
	}
	if len(compCtx.GIDs) != 2 {
		t.Errorf("len(GIDs) = %d, want 2", len(compCtx.GIDs))
	}
}

// TestExtractV4HandlerContext_AuthUnixSucceeds is a regression guard to confirm
// that AUTH_UNIX paths are not disturbed by this change.
func TestExtractV4HandlerContext_AuthUnixSucceeds(t *testing.T) {
	call := &fakeCall{flavor: rpc.AuthUnix} // no body → warns but returns OK
	compCtx, status := ExtractV4HandlerContext(context.Background(), call, "127.0.0.1:1234")
	if status != types.NFS4_OK {
		t.Errorf("AUTH_UNIX empty-body status = %d, want NFS4_OK", status)
	}
	if compCtx == nil {
		t.Error("compCtx = nil for AUTH_UNIX, want non-nil")
	}
}
