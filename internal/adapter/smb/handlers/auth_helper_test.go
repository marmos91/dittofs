package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestBuildAuthContextFromUser_PopulatesSID(t *testing.T) {
	uid := uint32(1001)
	user := &models.User{
		ID:        "user-1",
		Username:  "alice",
		UID:       &uid,
		SID:       "S-1-5-21-1-2-3-2001",
		GroupSIDs: []string{"S-1-5-21-1-2-3-513"},
	}
	ctx := &SMBHandlerContext{
		Context: context.Background(),
		User:    user,
	}

	authCtx := BuildAuthContextFromUser(ctx, user)
	if authCtx == nil || authCtx.Identity == nil {
		t.Fatal("nil Identity")
	}
	if authCtx.Identity.SID == nil {
		t.Fatalf("Identity.SID is nil, want %q", user.SID)
	}
	if got := *authCtx.Identity.SID; got != user.SID {
		t.Errorf("Identity.SID = %q, want %q", got, user.SID)
	}
	// GroupSIDs MUST be the implicit Everyone + Authenticated Users set
	// (S-1-1-0 + S-1-5-11) merged with the user's named group SIDs.
	wantGroupSIDs := []string{"S-1-1-0", "S-1-5-11", "S-1-5-21-1-2-3-513"}
	if !equalStrings(authCtx.Identity.GroupSIDs, wantGroupSIDs) {
		t.Errorf("Identity.GroupSIDs = %v, want %v", authCtx.Identity.GroupSIDs, wantGroupSIDs)
	}
}

func TestBuildAuthContextFromUser_NoSIDLeavesIdentityEmpty(t *testing.T) {
	uid := uint32(1001)
	user := &models.User{ID: "user-2", Username: "bob", UID: &uid}
	ctx := &SMBHandlerContext{Context: context.Background(), User: user}

	authCtx := BuildAuthContextFromUser(ctx, user)
	if authCtx.Identity.SID != nil {
		t.Errorf("Identity.SID = %v, want nil for user without SID", *authCtx.Identity.SID)
	}
	// Even when no SID is populated, the authenticated session still gets
	// the implicit Everyone + Authenticated Users group SIDs — a DACL ACE
	// keyed on S-1-5-11 must match the authenticated user without depending
	// on the local user model's named-group enumeration.
	wantGroupSIDs := []string{"S-1-1-0", "S-1-5-11"}
	if !equalStrings(authCtx.Identity.GroupSIDs, wantGroupSIDs) {
		t.Errorf("Identity.GroupSIDs = %v, want %v", authCtx.Identity.GroupSIDs, wantGroupSIDs)
	}
}

// TestMergeImplicitAuthSIDs_Dedupes pins the dedup contract so a user model
// that already enumerates Everyone or Authenticated Users in user.GroupSIDs
// does not produce duplicate entries.
func TestMergeImplicitAuthSIDs_Dedupes(t *testing.T) {
	got := mergeImplicitAuthSIDs([]string{"S-1-5-11", "S-1-5-21-1-2-3-513", "S-1-1-0"})
	want := []string{"S-1-1-0", "S-1-5-11", "S-1-5-21-1-2-3-513"}
	if !equalStrings(got, want) {
		t.Errorf("mergeImplicitAuthSIDs = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPrimeAuthContext_GuestSessionPropagatesIsGuest pins the guest-session
// regression flagged on PR #618: guest sessions are created with User=nil and
// IsGuest=true (see session.NewSession). The earlier primeAuthContext gated
// BOTH the ctx.User assignment AND the IsGuest assignment on sess.User!=nil,
// so guest sessions silently lost IsGuest on every operation past the
// handshake — BuildAuthContext then fell into the User==nil + IsGuest==false
// arm and mapped the request to UID 0 (root) instead of UID 65534 (nobody).
//
// The fix loosens the guard: IsGuest, ctx.TreeID and ctx.SessionID propagate
// independently of sess.User, only ctx.User=sess.User stays nil-guarded.
func TestPrimeAuthContext_GuestSessionPropagatesIsGuest(t *testing.T) {
	h := NewHandler()

	guestSess := h.CreateSession("127.0.0.1:0", true /*isGuest*/, "" /*username*/, "" /*domain*/)
	if guestSess.User != nil {
		t.Fatalf("test precondition: guest session expected User=nil, got %+v", guestSess.User)
	}
	if !guestSess.IsGuest {
		t.Fatal("test precondition: guest session expected IsGuest=true")
	}

	const treeID uint32 = 42
	h.StoreTree(&TreeConnection{
		TreeID:    treeID,
		SessionID: guestSess.SessionID,
		ShareName: "/guestshare",
		// Permission left zero-value — we only care about IsGuest propagation.
	})

	ctx := &SMBHandlerContext{Context: context.Background()}
	h.primeAuthContext(ctx, treeID, guestSess.SessionID)

	if !ctx.IsGuest {
		t.Errorf("primeAuthContext did not propagate IsGuest from guest session " +
			"(would re-introduce the UID-0 root-bypass for guest QUERY_DIRECTORY)")
	}
	if ctx.User != nil {
		t.Errorf("primeAuthContext clobbered ctx.User with nil sess.User; want nil-guard preserved, got %+v", ctx.User)
	}
	if ctx.TreeID != treeID {
		t.Errorf("ctx.TreeID = %d, want %d (downstream gates such as treeHasAccessBasedEnumeration read ctx.TreeID directly)", ctx.TreeID, treeID)
	}
	if ctx.SessionID != guestSess.SessionID {
		t.Errorf("ctx.SessionID = %d, want %d", ctx.SessionID, guestSess.SessionID)
	}
	if ctx.ShareName != "/guestshare" {
		t.Errorf("ctx.ShareName = %q, want %q", ctx.ShareName, "/guestshare")
	}

	// And the downstream BuildAuthContext arm: with IsGuest now propagated the
	// nobody/nogroup UID 65534 must win, not the anonymous UID-0 root.
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	if authCtx.Identity.UID == nil || *authCtx.Identity.UID != 65534 {
		gotUID := uint32(0)
		if authCtx.Identity.UID != nil {
			gotUID = *authCtx.Identity.UID
		}
		t.Errorf("guest session mapped to UID %d, want 65534 (nobody); the IsGuest gate in BuildAuthContext is being bypassed", gotUID)
	}
}

// TestPrimeAuthContext_PreservesFixtureUserWhenSessionUserNil documents the
// other half of the nil-guard contract: when the session exists but has
// User=nil (e.g. GetSession(0) or test fixtures that don't register a parallel
// session), primeAuthContext must NOT clobber a pre-populated ctx.User. This
// is what keeps the unit-level fixtures working without forcing each test to
// also register a session.
func TestPrimeAuthContext_PreservesFixtureUserWhenSessionUserNil(t *testing.T) {
	h := NewHandler()
	// Build a non-guest session with User=nil — mirrors the manager's seeded
	// anonymous pre-auth session.
	anonSess := h.CreateSession("127.0.0.1:0", false, "", "")
	if anonSess.User != nil {
		t.Fatalf("test precondition: anonymous session expected User=nil, got %+v", anonSess.User)
	}

	uid := uint32(1001)
	fixtureUser := &models.User{ID: "fixture", Username: "alice", UID: &uid}
	ctx := &SMBHandlerContext{
		Context: context.Background(),
		User:    fixtureUser,
	}

	h.primeAuthContext(ctx, 0 /*no tree*/, anonSess.SessionID)

	if ctx.User != fixtureUser {
		t.Errorf("primeAuthContext clobbered pre-populated ctx.User (got %+v, want fixture user)", ctx.User)
	}
	if ctx.IsGuest {
		t.Errorf("ctx.IsGuest = true; anonymous (non-guest) session should leave IsGuest=false")
	}
}

// TestBuildAuthContext_NullSessionMapsToNobodyNotRoot is the negative control
// for the anonymous-NTLM-to-UID-0 root-bypass (audit #1132 HIGH). A null /
// anonymous SMB session has ctx.User==nil and ctx.IsGuest==false. Before the
// fix BuildAuthContext mapped it to UID=0/GID=0, which trips the UID==0 root
// short-circuit in pkg/metadata/auth_permissions.go and grants
// root-equivalent access to every file regardless of POSIX bits or ACLs.
//
// The mapping MUST resolve to the unprivileged nobody/nogroup (65534)
// identity so per-file permission checks still apply. Accepting the anonymous
// *connection* stays legal (smb2.anon-signing / anon-encryption); only the
// identity mapping changes.
func TestBuildAuthContext_NullSessionMapsToNobodyNotRoot(t *testing.T) {
	ctx := &SMBHandlerContext{
		Context: context.Background(),
		User:    nil,   // anonymous / null
		IsGuest: false, // NOT a guest — this is the null-session arm
	}

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	if authCtx.Identity == nil || authCtx.Identity.UID == nil || authCtx.Identity.GID == nil {
		t.Fatal("null session produced no UID/GID identity")
	}
	if *authCtx.Identity.UID == 0 || *authCtx.Identity.GID == 0 {
		t.Fatalf("null/anonymous session mapped to UID=%d/GID=%d — this hits the metadata UID==0 root bypass and grants root to unauthenticated clients",
			*authCtx.Identity.UID, *authCtx.Identity.GID)
	}
	if *authCtx.Identity.UID != 65534 || *authCtx.Identity.GID != 65534 {
		t.Errorf("null session UID/GID = %d/%d, want 65534/65534 (nobody/nogroup)",
			*authCtx.Identity.UID, *authCtx.Identity.GID)
	}
}

// TestBuildOpenerAuthContext_NullOpenerMapsToNobodyNotRoot is the negative
// control for the same root-bypass on the handle-bound opener path
// (buildOpenerAuthContext). A handle opened by a null session (OpenerUser==nil,
// OpenerIsGuest==false, OpenerIsNull==true) must rebuild to the unprivileged
// nobody identity, never UID=0.
func TestBuildOpenerAuthContext_NullOpenerMapsToNobodyNotRoot(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{Context: context.Background()}
	openFile := &OpenFile{
		OpenerUser:    nil,
		OpenerIsGuest: false,
		OpenerIsNull:  true,
	}

	authCtx := h.buildOpenerAuthContext(ctx, openFile)
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		t.Fatal("null opener produced no identity")
	}
	if *authCtx.Identity.UID == 0 {
		t.Fatalf("null opener mapped to UID=0 — root bypass on the handle-bound path")
	}
	if *authCtx.Identity.UID != 65534 {
		t.Errorf("null opener UID = %d, want 65534 (nobody)", *authCtx.Identity.UID)
	}
}

// TestPropagateOpenFileParentLeaseKey_Set asserts that the helper copies
// OpenFile.ParentLeaseKey / HasParentLeaseKey onto the AuthContext so the
// metadata layer can route the value into notifyDirChange (#470 C6/C7).
func TestPropagateOpenFileParentLeaseKey_Set(t *testing.T) {
	openFile := &OpenFile{
		ParentLeaseKey:    [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04},
		HasParentLeaseKey: true,
	}
	authCtx := &metadata.AuthContext{Context: context.Background()}

	PropagateOpenFileParentLeaseKey(authCtx, openFile)

	if !authCtx.HasParentLeaseKey {
		t.Fatal("HasParentLeaseKey must be true after propagation from a flagged OpenFile")
	}
	if authCtx.ParentLeaseKey != openFile.ParentLeaseKey {
		t.Errorf("ParentLeaseKey = %x, want %x", authCtx.ParentLeaseKey, openFile.ParentLeaseKey)
	}
}

// TestPropagateOpenFileParentLeaseKey_Unset asserts no-op when the OpenFile
// had no RqLs+parent-key linkage. This preserves the "no parent-key on the
// closing handle" path (different_set_and_close, different_initial_and_close)
// where all dir leases MUST break.
func TestPropagateOpenFileParentLeaseKey_Unset(t *testing.T) {
	openFile := &OpenFile{HasParentLeaseKey: false}
	authCtx := &metadata.AuthContext{
		Context:           context.Background(),
		ParentLeaseKey:    [16]byte{}, // start clean
		HasParentLeaseKey: false,
	}
	// Should leave the context unchanged.
	PropagateOpenFileParentLeaseKey(authCtx, openFile)
	if authCtx.HasParentLeaseKey {
		t.Fatal("HasParentLeaseKey must stay false when OpenFile has no linkage")
	}
	if authCtx.ParentLeaseKey != ([16]byte{}) {
		t.Errorf("ParentLeaseKey must stay zero when OpenFile has no linkage, got %x", authCtx.ParentLeaseKey)
	}
}

// TestPropagateOpenFileParentLeaseKey_NilSafe pins the documented nil-safety
// contract: callers in close.go / cleanup paths may receive nil OpenFile or
// authCtx if state is partially torn down; the helper must not panic.
func TestPropagateOpenFileParentLeaseKey_NilSafe(t *testing.T) {
	PropagateOpenFileParentLeaseKey(nil, nil)
	PropagateOpenFileParentLeaseKey(&metadata.AuthContext{Context: context.Background()}, nil)
	PropagateOpenFileParentLeaseKey(nil, &OpenFile{HasParentLeaseKey: true})
}
