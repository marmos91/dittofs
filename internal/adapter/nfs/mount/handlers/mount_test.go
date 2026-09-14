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

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
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
	// GetShare returns a snapshot copy, so set via the locked setter.
	if err := rt.SetEnabledForTesting(shareName, enabled); err != nil {
		t.Fatalf("SetEnabledForTesting: %v", err)
	}

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

// newGSSMountCtx builds a MountHandlerContext for an RPCSEC_GSS mount carrying
// the given negotiated service level (gss.RPCGSSSvc*) in the request context,
// matching how the GSS DATA dispatch threads it through.
func newGSSMountCtx(reqCtx context.Context, service uint32) *MountHandlerContext {
	uid := uint32(1000)
	gid := uint32(1000)
	return &MountHandlerContext{
		Context:    gss.ContextWithSessionInfo(reqCtx, &gss.GSSSessionInfo{Service: service}),
		ClientAddr: "127.0.0.1:12345",
		AuthFlavor: rpc.AuthRPCSECGSS,
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{gid},
	}
}

// TestMount_MinKerberosLevel_RejectsBelowFloor verifies a krb5p share refuses a
// plain krb5 (authentication-only) GSS mount with MountErrAccess (#1283).
func TestMount_MinKerberosLevel_RejectsBelowFloor(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)
	if err := h.Registry.(*runtime.Runtime).SetMinKerberosLevelForTesting("/export", models.KerberosLevelKrb5p); err != nil {
		t.Fatalf("SetMinKerberosLevelForTesting: %v", err)
	}

	resp, err := h.Mount(newGSSMountCtx(ctx, gss.RPCGSSSvcNone), &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountErrAccess {
		t.Fatalf("Status = %d, want MountErrAccess (%d)", resp.Status, MountErrAccess)
	}
	if len(resp.FileHandle) != 0 {
		t.Errorf("FileHandle = %x, want empty on access-denied response", resp.FileHandle)
	}
}

// TestMount_MinKerberosLevel_AllowsAtFloor verifies a krb5p share accepts a
// privacy-level GSS mount.
func TestMount_MinKerberosLevel_AllowsAtFloor(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)
	if err := h.Registry.(*runtime.Runtime).SetMinKerberosLevelForTesting("/export", models.KerberosLevelKrb5p); err != nil {
		t.Fatalf("SetMinKerberosLevelForTesting: %v", err)
	}

	resp, err := h.Mount(newGSSMountCtx(ctx, gss.RPCGSSSvcPrivacy), &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountOK {
		t.Fatalf("Status = %d, want MountOK (%d)", resp.Status, MountOK)
	}
}

// TestMount_DisabledShare_ReturnsAccess covers the disabled-share path:
// a runtime share with Enabled=false must refuse MOUNT with
// MountErrAccess (MNT3ERR_ACCES=13) and must NOT return a root file
// handle. This is the adapter-side belt in the share-disabled-for-restore
// workflow.
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

// newUnverifiedGSSMountCtx builds a MountHandlerContext for a request that
// claims RPCSEC_GSS but carries no session info — the shape a forged flavor-6
// credential takes when the server has no GSS processor configured, so the
// dispatch never intercepts or verifies it.
func newUnverifiedGSSMountCtx(reqCtx context.Context) *MountHandlerContext {
	return &MountHandlerContext{
		Context:    reqCtx,
		ClientAddr: "127.0.0.1:12345",
		AuthFlavor: rpc.AuthRPCSECGSS,
	}
}

// TestMount_UnverifiedGSSCredentialDenied pins the GSS floor against a request
// that was never processed as GSS.
//
// The dispatch intercepts flavor 6 only when a GSS processor exists, so with
// Kerberos unconfigured a forged RPCSEC_GSS credential reaches this handler
// unverified and carries no session info. Skipping the protection-level check
// in that state clears the share's whole Kerberos policy: RequireKerberos is
// satisfied because the flavor *is* GSS, AllowAuthSys does not apply because
// the flavor is not AUTH_UNIX, and the min-level floor is the only gate left.
func TestMount_UnverifiedGSSCredentialDenied(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)
	if err := h.Registry.(*runtime.Runtime).SetMinKerberosLevelForTesting("/export", models.KerberosLevelKrb5p); err != nil {
		t.Fatalf("SetMinKerberosLevelForTesting: %v", err)
	}

	resp, err := h.Mount(newUnverifiedGSSMountCtx(ctx), &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountErrAccess {
		t.Fatalf("Status = %d, want MountErrAccess (%d): an unverified GSS credential must not mount a krb5p share", resp.Status, MountErrAccess)
	}
	if len(resp.FileHandle) != 0 {
		t.Errorf("FileHandle = %x, want empty — an unverified credential must not receive a root handle", resp.FileHandle)
	}
}

// TestMount_UnverifiedGSSDeniedOnUnrestrictedShare is the same denial on a share
// with no Kerberos floor configured: the credential is unverifiable regardless
// of what level the share asks for, so the deny must not depend on the floor.
func TestMount_UnverifiedGSSDeniedOnUnrestrictedShare(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)

	resp, err := h.Mount(newUnverifiedGSSMountCtx(ctx), &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountErrAccess {
		t.Fatalf("Status = %d, want MountErrAccess (%d)", resp.Status, MountErrAccess)
	}
}

// newKerberosMountCtx is newGSSMountCtx with KerberosEnabled set, matching a
// server that has a GSS processor configured.
func newKerberosMountCtx(reqCtx context.Context, service uint32) *MountHandlerContext {
	ctx := newGSSMountCtx(reqCtx, service)
	ctx.KerberosEnabled = true
	return ctx
}

// TestMount_AuthFlavors_OmitsAuthSysWhenShareForbidsIt pins the MNT reply's
// flavor list against the share's own policy.
//
// RFC 1813 App I makes fhstatus3.auth_flavors the list of flavors this export
// accepts, and mount.nfs and automounters negotiate sec= from it. The handler
// denies an AUTH_SYS mount on a RequireKerberos share a few lines earlier, so
// advertising AUTH_UNIX there points the client at a mount that cannot succeed.
func TestMount_AuthFlavors_OmitsAuthSysWhenShareForbidsIt(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)
	// allowAuthSys stays true; RequireKerberos is what refuses AUTH_SYS here.
	if err := h.Registry.(*runtime.Runtime).SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	resp, err := h.Mount(newKerberosMountCtx(ctx, gss.RPCGSSSvcPrivacy), &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountOK {
		t.Fatalf("Status = %d, want MountOK (%d)", resp.Status, MountOK)
	}
	for _, f := range resp.AuthFlavors {
		if f == 1 {
			t.Fatalf("AuthFlavors = %v, must not offer AUTH_UNIX on a RequireKerberos share", resp.AuthFlavors)
		}
	}
	if len(resp.AuthFlavors) == 0 {
		t.Fatal("AuthFlavors is empty; the Kerberos pseudoflavors should still be offered")
	}
}

// TestMount_AuthFlavors_OmitsPseudoflavorsBelowFloor is the same agreement on
// the Kerberos side: a krb5p export denies a krb5 or krb5i mount, so it must
// not advertise them.
func TestMount_AuthFlavors_OmitsPseudoflavorsBelowFloor(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)
	if err := h.Registry.(*runtime.Runtime).SetMinKerberosLevelForTesting("/export", models.KerberosLevelKrb5p); err != nil {
		t.Fatalf("SetMinKerberosLevelForTesting: %v", err)
	}

	resp, err := h.Mount(newKerberosMountCtx(ctx, gss.RPCGSSSvcPrivacy), &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountOK {
		t.Fatalf("Status = %d, want MountOK (%d)", resp.Status, MountOK)
	}
	for _, f := range resp.AuthFlavors {
		if f == int32(gss.PseudoFlavorKrb5) || f == int32(gss.PseudoFlavorKrb5i) {
			t.Fatalf("AuthFlavors = %v, must not offer a pseudoflavor below the krb5p floor", resp.AuthFlavors)
		}
	}
	var hasKrb5p bool
	for _, f := range resp.AuthFlavors {
		if f == int32(gss.PseudoFlavorKrb5p) {
			hasKrb5p = true
		}
	}
	if !hasKrb5p {
		t.Fatalf("AuthFlavors = %v, want krb5p offered on a krb5p share", resp.AuthFlavors)
	}
}

// TestMount_AuthNoneDeniedWhenShareForbidsAuthSys covers the flavor the
// AllowAuthSys gate used to miss.
//
// The gate named AUTH_UNIX specifically, so an AUTH_NONE caller — carrying no
// credential at all — passed it on a share configured to refuse AUTH_SYS and
// received a root handle, gated only by the share's default_permission. A share
// that refuses AUTH_SYS refuses a weaker flavor too.
func TestMount_AuthNoneDeniedWhenShareForbidsAuthSys(t *testing.T) {
	h, ctx := newTestMountHandler(t, "/export", true)
	if err := h.Registry.(*runtime.Runtime).SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	c := newMountCtx(ctx)
	c.AuthFlavor = 0 // AUTH_NONE
	c.UID, c.GID, c.GIDs = nil, nil, nil

	resp, err := h.Mount(c, &MountRequest{DirPath: "/export"})
	if err != nil {
		t.Fatalf("Mount returned unexpected error: %v", err)
	}
	if resp.Status != MountErrAccess {
		t.Fatalf("Status = %d, want MountErrAccess (%d): an AUTH_NONE caller must not mount a share that refuses AUTH_SYS",
			resp.Status, MountErrAccess)
	}
	if len(resp.FileHandle) != 0 {
		t.Fatalf("FileHandle = %x (%d bytes), want empty — a denied mount must not return a root handle",
			resp.FileHandle, len(resp.FileHandle))
	}
}
