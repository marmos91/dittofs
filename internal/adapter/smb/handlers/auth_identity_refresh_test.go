package handlers

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// userWithGrant builds a record carrying one explicit grant on /export and one
// group, which is what a resolution and an identity respectively read off it.
func userWithGrant(perm models.SharePermission, uid uint32, groupGID uint32) *models.User {
	gid := groupGID
	return &models.User{
		ID:       "user-1",
		Username: "alice",
		UID:      &uid,
		Enabled:  true,
		Groups:   []models.Group{{ID: "g1", Name: "staff", GID: &gid}},
		SharePermissions: []models.UserSharePermission{
			{UserID: "user-1", ShareName: "/export", Permission: string(perm)},
		},
	}
}

// buildDispatchIdentity resolves the identity a file operation on this session
// would be authorized with: the same prime-then-build the dispatch path runs
// before every metadata call.
func buildDispatchIdentity(t *testing.T, h *Handler, sessionID uint64) *metadata.Identity {
	t.Helper()
	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:12345", sessionID, 0, 1)
	h.primeAuthContext(ctx, 0, sessionID)
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	if authCtx.Identity == nil || authCtx.Identity.UID == nil {
		t.Fatal("no identity on the AuthContext a file operation would carry")
	}
	return authCtx.Identity
}

// TestRevalidateAuthorization_PublishesRefreshedIdentity pins the per-operation
// half. The sweep re-resolves tree permissions against a freshly read record;
// unless it also publishes that record, the identity every file operation is
// authorized with — the UID an ownership check compares against, the GIDs a
// group ACL matches on — stays at the SESSION_SETUP snapshot. The assertion is
// on that identity, not on the session field it comes from.
func TestRevalidateAuthorization_PublishesRefreshedIdentity(t *testing.T) {
	// The record the session authenticated with: UID 1000, in group 1000.
	sessionRecord := userWithGrant(models.PermissionReadWrite, 1000, 1000)
	// The record the operator has since edited: moved to UID 1500 and regrouped.
	store := &revalidateUserStore{resolveFromRecord: true, user: userWithGrant(models.PermissionReadWrite, 1500, 2000)}

	h, sessionID, _ := newRevalidateHandler(t, sessionRecord, store, models.PermissionReadWrite, false)

	before := buildDispatchIdentity(t, h, sessionID)
	if *before.UID != 1000 {
		t.Fatalf("fixture is wrong: operations start out authorized as UID %d, want 1000", *before.UID)
	}

	h.RevalidateAuthorization(context.Background())

	after := buildDispatchIdentity(t, h, sessionID)
	if *after.UID != 1500 {
		t.Errorf("a file operation is still authorized as UID %d after the sweep, want 1500: "+
			"the ownership check runs against the identity captured at SESSION_SETUP", *after.UID)
	}
	if !slices.Contains(after.GIDs, uint32(2000)) {
		t.Errorf("identity GIDs = %v, want the current group 2000: a group ACL still matches "+
			"on the membership the session authenticated with", after.GIDs)
	}
	if slices.Contains(after.GIDs, uint32(1000)) {
		t.Errorf("identity GIDs = %v, still carries the withdrawn group 1000", after.GIDs)
	}
}

// TestRevalidateAuthorization_ReauthDuringLookupLeavesRecord pins the other
// side of that publish: a re-authentication landing while the lookup is in
// flight has already re-decided authorization, so the record read for the
// previous identity must not be written over it.
func TestRevalidateAuthorization_ReauthDuringLookupLeavesRecord(t *testing.T) {
	sessionRecord := userWithGrant(models.PermissionReadWrite, 1000, 1000)
	store := &revalidateUserStore{resolveFromRecord: true, user: userWithGrant(models.PermissionReadWrite, 1500, 2000)}

	h, sessionID, _ := newRevalidateHandler(t, sessionRecord, store, models.PermissionReadWrite, false)
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}

	bobUID := uint32(4242)
	bob := &models.User{ID: "user-2", Username: "bob", UID: &bobUID, Enabled: true}
	store.onGetUser = func() {
		sess.UpdateIdentity(bob.Username, "", bob, false, false, nil, "")
	}

	h.RevalidateAuthorization(context.Background())

	got := buildDispatchIdentity(t, h, sessionID)
	if *got.UID != bobUID {
		t.Errorf("operations authorized as UID %d, want %d: the sweep published alice's record "+
			"onto a session that had already re-authenticated as bob", *got.UID, bobUID)
	}
}

// TestTreeConnect_ResolvesAgainstPersistedRecord pins the reconnect half. The
// sweep removes a tree whose grant was withdrawn, but a client reconnects
// routinely, and a TREE_CONNECT resolved from the session's snapshot hands back
// exactly the grant that was just revoked. The assertion is on the access the
// reconnect is granted, and the sweep is deliberately not run: TREE_CONNECT has
// to re-decide on its own.
func TestTreeConnect_ResolvesAgainstPersistedRecord(t *testing.T) {
	sessionRecord := userWithGrant(models.PermissionReadWrite, 1000, 1000)
	// The grant has been withdrawn since the session authenticated.
	store := &revalidateUserStore{resolveFromRecord: true, user: userWithGrant(models.PermissionNone, 1000, 1000)}

	h, sessionID, _ := newRevalidateHandler(t, sessionRecord, store, models.PermissionReadWrite, false)

	ctx := newTreeConnectTestContext(sessionID)
	res, err := h.TreeConnect(ctx, buildTreeConnectRequestBody("\\\\server\\export"))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	if store.gets == 0 {
		t.Error("TREE_CONNECT never re-read the user record")
	}
	if res.Status != types.StatusAccessDenied {
		t.Errorf("Status = %#x, want STATUS_ACCESS_DENIED: the reconnect was authorized from "+
			"the grants the session captured before they were withdrawn", res.Status)
	}
}

// TestTreeConnect_SynthesizedDirectoryUserNotRefused pins the first carve-out.
// A principal resolved from the directory with no local account is backed by a
// record that was never persisted, so a lookup reports it missing. Refusing on
// that answer would lock every AD session out of every share.
func TestTreeConnect_SynthesizedDirectoryUserNotRefused(t *testing.T) {
	uid := uint32(4242)
	// No ID: synthUserFromResolved never persists the record. The grant is
	// admin so the assertion can tell the record's own answer from the share
	// default a dropped record would fall back to.
	synth := &models.User{
		Username: "ad-user", UID: &uid, Enabled: true, SID: "S-1-5-21-1-2-3-1104",
		SharePermissions: []models.UserSharePermission{
			{ShareName: "/export", Permission: string(models.PermissionAdmin)},
		},
	}
	store := &revalidateUserStore{resolveFromRecord: true, err: models.ErrUserNotFound}

	h, sessionID, _ := newRevalidateHandler(t, synth, store, models.PermissionReadWrite, false)

	ctx := newTreeConnectTestContext(sessionID)
	res, err := h.TreeConnect(ctx, buildTreeConnectRequestBody("\\\\server\\export"))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	if store.gets != 0 {
		t.Error("a record with no primary key was offered to the store; the lookup can only report it missing")
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("Status = %#x, want STATUS_SUCCESS: a directory principal with no local "+
			"account was refused because its lookup reports it missing", res.Status)
	}
	assertTreePermission(t, h, ctx.TreeID, models.PermissionAdmin,
		"the directory principal's own grant was replaced by the share default")
}

// assertTreePermission checks the access the published tree actually carries,
// which is what every later operation on it is capped by.
func assertTreePermission(t *testing.T, h *Handler, treeID uint32, want models.SharePermission, why string) {
	t.Helper()
	tree, ok := h.GetTree(treeID)
	if !ok {
		t.Fatalf("no tree published for ID %d", treeID)
	}
	if tree.Permission != want {
		t.Errorf("tree permission = %v, want %v: %s", tree.Permission, want, why)
	}
}

// TestTreeConnect_LookupFailureKeepsSessionRecord pins the second carve-out. A
// failed lookup is not a decision: turning a store outage into a refusal would
// drop share access wholesale, and a deleted or disabled account is already
// refused outright by the dispatch gate.
func TestTreeConnect_LookupFailureKeepsSessionRecord(t *testing.T) {
	// Admin rather than read-write, so the assertion can tell the session
	// record's own answer from the share default that resolving against a
	// dropped record would fall back to.
	sessionRecord := userWithGrant(models.PermissionAdmin, 1000, 1000)
	store := &revalidateUserStore{resolveFromRecord: true, err: errors.New("connection refused")}

	h, sessionID, _ := newRevalidateHandler(t, sessionRecord, store, models.PermissionReadWrite, false)

	ctx := newTreeConnectTestContext(sessionID)
	res, err := h.TreeConnect(ctx, buildTreeConnectRequestBody("\\\\server\\export"))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("Status = %#x, want STATUS_SUCCESS: a store outage refused a share the "+
			"session is still granted", res.Status)
	}
	assertTreePermission(t, h, ctx.TreeID, models.PermissionAdmin,
		"a failed lookup was written back as an authorization decision")
}

// TestTreeConnect_ReauthDuringResolveRefusesTree pins the generation gate on
// the publish. Resolving access takes two store round trips, and MS-SMB2 keeps
// tree connections across a re-authentication — so a tree published from an
// answer about the previous principal would carry that principal's access for
// the life of the connection. The re-authentication is injected from inside the
// record lookup, which is where the window actually is.
func TestTreeConnect_ReauthDuringResolveRefusesTree(t *testing.T) {
	sessionRecord := userWithGrant(models.PermissionAdmin, 1000, 1000)
	store := &revalidateUserStore{resolveFromRecord: true, user: sessionRecord}

	h, sessionID, _ := newRevalidateHandler(t, sessionRecord, store, models.PermissionReadWrite, false)
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}

	bobUID := uint32(4242)
	bob := &models.User{ID: "user-2", Username: "bob", UID: &bobUID, Enabled: true}
	store.onGetUser = func() {
		sess.UpdateIdentity(bob.Username, "", bob, false, false, nil, "")
	}

	ctx := newTreeConnectTestContext(sessionID)
	res, err := h.TreeConnect(ctx, buildTreeConnectRequestBody("\\\\server\\export"))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	if res.Status != types.StatusNetworkSessionExpired {
		t.Errorf("Status = %#x, want STATUS_NETWORK_SESSION_EXPIRED: alice's access was "+
			"published onto a session that re-authenticated as bob mid-resolution", res.Status)
	}
	if ctx.TreeID != 0 {
		t.Errorf("TreeID = %d, want 0: the tree was published anyway", ctx.TreeID)
	}
}
