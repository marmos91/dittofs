// Tests for the OpenFile opener-identity snapshot path (smbtorture
// smb2.session.reauth4 / reauth5, #772). The shape they pin:
//
//  1. CaptureOpenerIdentity copies the SMB session's current User (and the
//     guest/null flags) onto the OpenFile at CREATE time.
//  2. buildOpenerAuthContext returns an AuthContext derived from THAT
//     snapshot rather than the now-mutated ctx.User after re-auth — so the
//     metadata layer ownership gate inside SetFileAttributes sees U1 even
//     while the session has been re-authed to anonymous.
//  3. When no snapshot was captured (legacy / restored-durable codepaths),
//     buildOpenerAuthContext falls back to the session-current
//     BuildAuthContext result so pre-existing behaviour is preserved.
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestCaptureOpenerIdentity_SnapshotsUserAndFlags verifies that the snapshot
// captures ctx.User + ctx.IsGuest verbatim. Caller is responsible for
// SessionManager wiring — when absent we silently skip the IsNull lookup
// (test-fixture path).
func TestCaptureOpenerIdentity_SnapshotsUserAndFlags(t *testing.T) {
	uid := uint32(1001)
	alice := &models.User{ID: "u1", Username: "alice", UID: &uid}
	ctx := &SMBHandlerContext{
		Context:   context.Background(),
		User:      alice,
		IsGuest:   false,
		SessionID: 42,
	}
	openFile := &OpenFile{}

	// nil-receiver guard — must not panic when called with bare Handler.
	(*Handler)(nil).CaptureOpenerIdentity(ctx, openFile)
	if openFile.OpenerUser != nil {
		t.Errorf("nil-Handler call should leave OpenerUser nil, got %+v", openFile.OpenerUser)
	}

	h := &Handler{}
	h.CaptureOpenerIdentity(ctx, openFile)
	if openFile.OpenerUser != alice {
		t.Errorf("OpenerUser = %+v, want pointer-equal to alice", openFile.OpenerUser)
	}
	if openFile.OpenerIsGuest {
		t.Error("OpenerIsGuest = true, want false (alice is not guest)")
	}
}

// TestBuildOpenerAuthContext_UsesSnapshotAfterReauth pins the core re-auth
// behaviour: after the SMB session's User has been swapped out under us
// (simulating SESSION_SETUP re-auth via tryReauthUpdate), the opener-context
// builder MUST still return Alice's identity for handle-bound ops on a
// handle opened by Alice. Mirror of MS-SMB2 §3.3.5.5.3.
func TestBuildOpenerAuthContext_UsesSnapshotAfterReauth(t *testing.T) {
	aliceUID := uint32(1001)
	alice := &models.User{ID: "u1", Username: "alice", UID: &aliceUID}

	// Phase 1: Alice opens a handle. Snapshot her identity onto OpenFile.
	openFile := &OpenFile{}
	openFile.OpenerUser = alice
	openFile.OpenerIsGuest = false

	// Phase 2: re-auth happens. ctx now represents the new (guest) session.
	ctx := &SMBHandlerContext{
		Context: context.Background(),
		User:    nil, // anon
		IsGuest: true,
		// No User on ctx — the legacy path would mint a 65534 guest identity.
	}
	h := &Handler{}

	authCtx, err := h.buildOpenerAuthContext(ctx, openFile)
	if err != nil {
		t.Fatalf("buildOpenerAuthContext: %v", err)
	}
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		t.Fatal("nil identity from opener-context builder")
	}
	if got := *authCtx.Identity.UID; got != aliceUID {
		t.Errorf("opener UID = %d, want %d (alice). Re-auth-to-guest must not leak into handle-bound authCtx (#772).", got, aliceUID)
	}
	if authCtx.Identity.Username != alice.Username {
		t.Errorf("opener Username = %q, want %q", authCtx.Identity.Username, alice.Username)
	}
}

// TestBuildOpenerAuthContext_FallsBackToSessionWithoutSnapshot pins the
// legacy / restored-durable path: when no opener snapshot exists on the
// OpenFile, the builder MUST fall back to BuildAuthContext against the
// current ctx. Anything else would silently regress every code path that
// pre-dates the snapshot.
func TestBuildOpenerAuthContext_FallsBackToSessionWithoutSnapshot(t *testing.T) {
	uid := uint32(7777)
	bob := &models.User{ID: "u2", Username: "bob", UID: &uid}
	ctx := &SMBHandlerContext{
		Context: context.Background(),
		User:    bob,
	}
	h := &Handler{}

	openFile := &OpenFile{} // no snapshot

	authCtx, err := h.buildOpenerAuthContext(ctx, openFile)
	if err != nil {
		t.Fatalf("buildOpenerAuthContext: %v", err)
	}
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		t.Fatal("nil identity from fallback path")
	}
	if got := *authCtx.Identity.UID; got != uid {
		t.Errorf("fallback UID = %d, want %d (ctx-current bob)", got, uid)
	}
}

// TestBuildOpenerAuthContext_GuestSnapshotPinsNobody pins guest-opener
// snapshot fidelity. The handle was opened by a guest session (User=nil,
// IsGuest=true). After re-auth to a real authenticated user, handle-bound
// ops MUST still resolve as nobody/65534, not the new principal's UID.
// Tests the symmetric case to the U1→anon flow.
func TestBuildOpenerAuthContext_GuestSnapshotPinsNobody(t *testing.T) {
	// Phase 1: guest opens a handle.
	openFile := &OpenFile{
		OpenerUser:    nil,
		OpenerIsGuest: true,
	}

	// Phase 2: re-auth swaps the session to a real user. Anything that
	// rebuilt authCtx from ctx.User would resolve as that real user.
	uid := uint32(2002)
	realUser := &models.User{ID: "u3", Username: "carol", UID: &uid}
	ctx := &SMBHandlerContext{
		Context: context.Background(),
		User:    realUser,
		IsGuest: false,
	}
	h := &Handler{}

	authCtx, err := h.buildOpenerAuthContext(ctx, openFile)
	if err != nil {
		t.Fatalf("buildOpenerAuthContext: %v", err)
	}
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		t.Fatal("nil identity")
	}
	const guestNobody = uint32(65534)
	if got := *authCtx.Identity.UID; got != guestNobody {
		t.Errorf("opener UID = %d, want %d (guest nobody). The opener-snapshot guest arm must not be shadowed by the session's new authenticated User.", got, guestNobody)
	}
}
