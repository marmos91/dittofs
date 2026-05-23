package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
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
	if len(authCtx.Identity.GroupSIDs) != 1 || authCtx.Identity.GroupSIDs[0] != user.GroupSIDs[0] {
		t.Errorf("Identity.GroupSIDs = %v, want %v", authCtx.Identity.GroupSIDs, user.GroupSIDs)
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
	if len(authCtx.Identity.GroupSIDs) != 0 {
		t.Errorf("Identity.GroupSIDs = %v, want empty", authCtx.Identity.GroupSIDs)
	}
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
