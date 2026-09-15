package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/pending"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	memorymeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// revalidateUserStore serves a single user record and a canned share-permission
// answer. models.UserStore is embedded rather than implemented: the sweep calls
// only GetUser and ResolveSharePermission, so anything else reaching the store
// is a change in what the sweep does and should fail loudly instead of
// silently returning a zero value.
type revalidateUserStore struct {
	models.UserStore
	user *models.User
	err  error
	perm models.SharePermission
}

func (s *revalidateUserStore) GetUser(_ context.Context, _ string) (*models.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.user, nil
}

func (s *revalidateUserStore) ResolveSharePermission(_ context.Context, _ *models.User, _ string) (models.SharePermission, error) {
	return s.perm, nil
}

// revalidateRuntime overrides only the user store on a real runtime, so share
// lookups keep going through the genuine registry.
type revalidateRuntime struct {
	smbRuntime
	users models.UserStore
}

func (r *revalidateRuntime) GetUserStore() models.UserStore { return r.users }

// newRevalidateHandler builds a handler holding one share, one session
// authenticated as user, and (when withTree) one tree pinned at pinned.
func newRevalidateHandler(t *testing.T, user *models.User, store *revalidateUserStore, pinned models.SharePermission, withTree bool) (*Handler, uint64, uint32) {
	t.Helper()

	rt, blockStoreID := newTestShareRuntime(t)
	metaStore := memorymeta.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	cfg := &runtime.ShareConfig{
		Name:              "/export",
		MetadataStore:     "test-meta",
		BlockStoreID:      blockStoreID,
		Enabled:           true,
		DefaultPermission: "read-write",
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}
	if err := rt.AddShare(context.Background(), cfg); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	h := NewHandler()
	h.Registry = &revalidateRuntime{smbRuntime: rt, users: store}

	sess := h.CreateSession("127.0.0.1:12345", false, user.Username, "")
	sess.User = user

	var treeID uint32
	if withTree {
		treeID = h.GenerateTreeID()
		h.StoreTree(&TreeConnection{
			TreeID:     treeID,
			SessionID:  sess.SessionID,
			ShareName:  "/export",
			Permission: pinned,
		})
	}
	return h, sess.SessionID, treeID
}

func enabledUser() *models.User {
	uid := uint32(1000)
	return &models.User{ID: "user-1", Username: "alice", UID: &uid, Enabled: true}
}

// TestRevalidateAuthorization_DisabledUserRevokesSession is the core guard for
// the gap: a session authenticated while the user was enabled must stop being
// authorized once the user is disabled, without waiting for the connection to
// drop.
func TestRevalidateAuthorization_DisabledUserRevokesSession(t *testing.T) {
	disabled := enabledUser()
	disabled.Enabled = false
	store := &revalidateUserStore{user: disabled, perm: models.PermissionReadWrite}

	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, false)

	sess, _ := h.GetSession(sessionID)
	if sess.AuthRevoked() {
		t.Fatal("session revoked before revalidation ran")
	}

	h.RevalidateAuthorization(context.Background())

	if !sess.AuthRevoked() {
		t.Error("session authorization not revoked after the user was disabled")
	}
}

// TestRevalidateAuthorization_DeletedUserRevokesSession covers the other
// retirement: the record is gone entirely, which the store reports as an error.
func TestRevalidateAuthorization_DeletedUserRevokesSession(t *testing.T) {
	store := &revalidateUserStore{err: models.ErrUserNotFound, perm: models.PermissionReadWrite}

	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, false)
	h.RevalidateAuthorization(context.Background())

	sess, _ := h.GetSession(sessionID)
	if !sess.AuthRevoked() {
		t.Error("session authorization not revoked after the user record disappeared")
	}
}

// TestRevalidateAuthorization_EnabledUserSurvives is the regression guard: the
// sweep runs on every control-plane mutation, so revoking a still-valid session
// would disconnect every SMB client on an unrelated edit.
func TestRevalidateAuthorization_EnabledUserSurvives(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionReadWrite}

	h, sessionID, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)
	h.RevalidateAuthorization(context.Background())

	sess, _ := h.GetSession(sessionID)
	if sess.AuthRevoked() {
		t.Error("a session whose user is still enabled must not be revoked")
	}
	tree, ok := h.GetTree(treeID)
	if !ok {
		t.Fatal("tree removed for a user who still has access")
	}
	if tree.Permission != models.PermissionReadWrite {
		t.Errorf("Permission = %v, want read-write (unchanged)", tree.Permission)
	}
}

// TestRevalidateAuthorization_TreePermissionDowngraded covers the share-grant
// half: the permission resolved at TREE_CONNECT is pinned on the tree, so a
// downgrade reaches the connection only if the sweep re-resolves it.
func TestRevalidateAuthorization_TreePermissionDowngraded(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionRead}

	h, _, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)
	h.RevalidateAuthorization(context.Background())

	tree, ok := h.GetTree(treeID)
	if !ok {
		t.Fatal("tree removed on a downgrade; it should have been re-resolved")
	}
	if tree.Permission != models.PermissionRead {
		t.Errorf("Permission = %v, want read after the grant was downgraded", tree.Permission)
	}
}

// TestRevalidateAuthorization_TreeRemovedWhenAccessRevoked pins the full
// revocation case. The tree is removed rather than left pinned at none: none
// reads as "not read-only" downstream, so storing it would lift the read-only
// ceiling instead of closing access.
func TestRevalidateAuthorization_TreeRemovedWhenAccessRevoked(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionNone}

	h, _, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)
	h.RevalidateAuthorization(context.Background())

	if _, ok := h.GetTree(treeID); ok {
		t.Error("tree survived a grant revoked to none")
	}
}

// TestResolveSharePermission_DisabledUserDenied guards the resolver directly.
// SESSION_SETUP refuses a disabled user at authentication, but an established
// session carries a user snapshot taken back then and can keep TREE_CONNECTing
// with it.
func TestResolveSharePermission_DisabledUserDenied(t *testing.T) {
	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:12345", 1, 0, 1)

	t.Run("DisabledUserGetsNone", func(t *testing.T) {
		uid := uint32(1000)
		user := &models.User{Username: "alice", UID: &uid, Enabled: false}
		sess := session.NewSessionWithUser(1, "127.0.0.1", user, "")
		share := &runtime.Share{Name: "/export", DefaultPermission: "read-write"}

		perm, _ := resolveSharePermission(ctx, sess, share, models.PermissionReadWrite, nil)

		if perm != models.PermissionNone {
			t.Errorf("Permission = %v, want none for a disabled user", perm)
		}
	})

	// A disabled root is still disabled: the squash bypass must not outrank the
	// account being retired.
	t.Run("DisabledRootDoesNotGetAdminBypass", func(t *testing.T) {
		uid := uint32(0)
		user := &models.User{Username: "root", UID: &uid, Enabled: false}
		sess := session.NewSessionWithUser(1, "127.0.0.1", user, "")
		share := &runtime.Share{
			Name:              "/export",
			Squash:            models.SquashRootToAdmin,
			DefaultPermission: "read-write",
		}

		perm, _ := resolveSharePermission(ctx, sess, share, models.PermissionReadWrite, nil)

		if perm != models.PermissionNone {
			t.Errorf("Permission = %v, want none for a disabled root user", perm)
		}
	})
}

// TestRevalidateAuthorization_SynthesizedADUserSurvives guards a regression the
// sweep would otherwise cause. A Kerberos/LDAP principal with no local account
// is backed by a synthesized, never-persisted record, so a lookup by username
// reports it missing. Revoking on that would retire every AD session on the
// next unrelated user edit — a worse outage than the staleness being fixed.
func TestRevalidateAuthorization_SynthesizedADUserSurvives(t *testing.T) {
	uid := uint32(4242)
	// No ID: synthUserFromResolved never persists the record.
	synth := &models.User{Username: "ad-user", UID: &uid, Enabled: true, SID: "S-1-5-21-1-2-3-1104"}
	store := &revalidateUserStore{err: models.ErrUserNotFound, perm: models.PermissionReadWrite}

	h, sessionID, treeID := newRevalidateHandler(t, synth, store, models.PermissionReadWrite, true)
	h.RevalidateAuthorization(context.Background())

	sess, _ := h.GetSession(sessionID)
	if sess.AuthRevoked() {
		t.Error("a directory-resolved session with no local account must not be revoked")
	}
	if _, ok := h.GetTree(treeID); !ok {
		t.Error("tree removed for a directory-resolved session that still has access")
	}
}

// TestRevalidateAuthorization_TransientStoreErrorKeepsSession pins the other
// half of that rule: a store failure is not evidence the account went away, and
// revoking on one would drop every SMB session on a database blip.
func TestRevalidateAuthorization_TransientStoreErrorKeepsSession(t *testing.T) {
	store := &revalidateUserStore{err: errors.New("connection refused"), perm: models.PermissionReadWrite}

	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, false)
	h.RevalidateAuthorization(context.Background())

	sess, _ := h.GetSession(sessionID)
	if sess.AuthRevoked() {
		t.Error("a transient store error must not revoke an established session")
	}
}

// TestIsExpiredOrRevoked pins the predicate the dispatch gate and the LOCK
// handler share, so a revoked session cannot take a new lock through the
// exemption that lets it release held ones.
func TestIsExpiredOrRevoked(t *testing.T) {
	sess := session.NewSessionWithUser(1, "127.0.0.1", enabledUser(), "")
	if sess.IsExpiredOrRevoked() {
		t.Fatal("a fresh session reports as expired or revoked")
	}
	sess.RevokeAuth()
	if !sess.IsExpiredOrRevoked() {
		t.Error("a revoked session must report as unauthorized even with no ticket expiry")
	}
}

// TestRevokedSessionRecoversOnReauth pins the recovery path. Re-authentication
// reuses the same session object, so without clearing the flag a revoked session
// would keep failing every operation even after the account was re-enabled — the
// client would re-run SESSION_SETUP successfully and then loop on the refusal.
// The Kerberos expiry axis already recovers this way by refreshing ExpiresAt.
func TestRevokedSessionRecoversOnReauth(t *testing.T) {
	user := enabledUser()
	sess := session.NewSessionWithUser(1, "127.0.0.1", user, "")

	sess.RevokeAuth()
	if !sess.IsExpiredOrRevoked() {
		t.Fatal("session did not report as revoked")
	}

	sess.UpdateIdentity(user.Username, "", user, false, false)

	if sess.AuthRevoked() {
		t.Error("re-authentication must clear the revocation, or the session can never recover")
	}
	if sess.IsExpiredOrRevoked() {
		t.Error("session still reports as unauthorized after a successful re-authentication")
	}
}

// TestRevalidateAuthorization_RevokedSessionKeepsNoTrees pins the teardown.
// MS-SMB2 keeps tree connections across a re-authentication, and re-auth clears
// the revocation — so a revoked session that kept its trees would resume on the
// permission resolved for the old user, including grants revoked while it was
// out. Every TREE_CONNECT after a recovery must re-decide access.
func TestRevalidateAuthorization_RevokedSessionKeepsNoTrees(t *testing.T) {
	disabled := enabledUser()
	disabled.Enabled = false
	store := &revalidateUserStore{user: disabled, perm: models.PermissionReadWrite}

	h, sessionID, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)
	if _, ok := h.GetTree(treeID); !ok {
		t.Fatal("tree missing before revalidation")
	}

	h.RevalidateAuthorization(context.Background())

	sess, _ := h.GetSession(sessionID)
	if !sess.AuthRevoked() {
		t.Fatal("session not revoked")
	}
	if _, ok := h.GetTree(treeID); ok {
		t.Error("a revoked session kept its tree; a later re-auth would inherit its permission")
	}
}

// TestRevalidateAuthorization_RevokedSessionDrainsParkedCreate pins the outcome,
// not the mechanism: a CREATE parked on a lease break passed its authorization
// check before the revocation, and completeCreateAfterBreak does not re-check
// the session, so it must not be left to resume.
//
// It holds today by two independent paths — releasing the session's leases
// completes the break with StatusCancelled, and the explicit drain cancels
// whatever is still registered — so removing either one alone keeps it green.
// That is the point: the contract is that no parked operation survives a
// revocation, and it should stay pinned however the teardown is rearranged.
func TestRevalidateAuthorization_RevokedSessionDrainsParkedCreate(t *testing.T) {
	uid := uint32(1000)
	user := &models.User{ID: "u1", Username: "alice", UID: &uid, Enabled: true}
	store := &revalidateUserStore{user: &models.User{ID: "u1", Username: "alice", UID: &uid, Enabled: false}}

	h, sessionID, _ := newRevalidateHandler(t, user, store, models.PermissionReadWrite, true)

	gotStatus := make(chan types.Status, 1)
	err := h.PendingCreateRegistry.Register(&pending.PendingCreate{
		SessionID: sessionID,
		MessageID: 7,
		AsyncId:   99,
		Cancel:    func() {},
		Callback: func(_, _, _ uint64, status types.Status, _ []byte) error {
			gotStatus <- status
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Register parked CREATE: %v", err)
	}
	if h.PendingCreateRegistry.Len() != 1 {
		t.Fatalf("parked CREATE was not registered")
	}

	h.RevalidateAuthorization(context.Background())

	if n := h.PendingCreateRegistry.Len(); n != 0 {
		t.Fatalf("revocation left %d parked CREATE(s) registered; they resume with the revoked session's authorization", n)
	}
	select {
	case status := <-gotStatus:
		if status != types.StatusCancelled {
			t.Errorf("parked CREATE completed with %v; want StatusCancelled", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked CREATE was never completed, so its async slot and replay reservation leak")
	}
}
