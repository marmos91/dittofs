package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	// permErr fails the share-permission lookup while GetUser still succeeds,
	// the shape a store outage takes once the user record is already cached.
	permErr error
	// sidErr fails only the SID-grant lookup, which for an AD principal with no
	// local account can carry its entire grant.
	sidErr error
	// onResolve, when set, runs on entry to the share-permission lookup so a
	// test can observe how many sweeps are inside the resolve pass at once.
	onResolve func()
	// onGetUser, when set, runs on entry to the user lookup. It is the window
	// the sweep leaves open between reading the session's identity and acting
	// on what the store says about it, so a test can re-authenticate the
	// session at exactly that point.
	onGetUser func()
	// gotSIDs records the SIDs the last SID-grant lookup was asked about, so a
	// test can tell which identity the resolution actually ran against.
	gotSIDs []string
}

// ResolveSharePermissionForSIDs makes the fake satisfy sidSharePermissionResolver
// so the SID arm of the resolver is exercised.
func (s *revalidateUserStore) ResolveSharePermissionForSIDs(_ context.Context, sids []string, _ string) (models.SharePermission, error) {
	s.gotSIDs = append([]string(nil), sids...)
	if s.sidErr != nil {
		return models.PermissionNone, s.sidErr
	}
	return models.PermissionNone, nil
}

func (s *revalidateUserStore) GetUser(_ context.Context, _ string) (*models.User, error) {
	if s.onGetUser != nil {
		s.onGetUser()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.user, nil
}

func (s *revalidateUserStore) ResolveSharePermission(_ context.Context, _ *models.User, _ string) (models.SharePermission, error) {
	if s.onResolve != nil {
		s.onResolve()
	}
	if s.permErr != nil {
		return models.PermissionNone, s.permErr
	}
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
	// Assigned only when non-nil: putting a typed nil pointer in the interface
	// makes GetUserStore return something that is not nil, which is the whole
	// condition a no-user-store test is trying to produce.
	reg := &revalidateRuntime{smbRuntime: rt}
	if store != nil {
		reg.users = store
	}
	h.Registry = reg

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

// TestRevalidateAuthorization_ResolverErrorDoesNotRaisePermission pins the
// re-check against a failed lookup. The resolver answers a lookup error with the
// share's default permission, which TREE_CONNECT may hand out on a fresh
// connect; writing it back here would raise a tree the operator had restricted
// to whatever the share grants everyone. The tree below is pinned at read while
// the share defaults to read-write, so a fallback treated as a decision shows up
// as exactly that promotion.
func TestRevalidateAuthorization_ResolverErrorDoesNotRaisePermission(t *testing.T) {
	store := &revalidateUserStore{
		user:    enabledUser(),
		permErr: errors.New("connection refused"),
	}

	h, _, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionRead, true)
	h.RevalidateAuthorization(context.Background())

	tree, ok := h.GetTree(treeID)
	if !ok {
		t.Fatal("tree removed on a failed lookup; a store outage must not revoke access either")
	}
	if tree.Permission != models.PermissionRead {
		t.Errorf("Permission = %v, want read: a failed lookup fell back to the share "+
			"default and was written back as a new authorization decision", tree.Permission)
	}
}

// TestRevalidateAuthorization_RevokedTreeCancelsParkedLock pins the tree
// teardown against a blocking LOCK. A LOCK parked on the tree passed its
// authorization check before the grant was withdrawn, and closing the tree's
// opens does not wake it — TREE_DISCONNECT owes such a lock a cancel, and a
// revocation that removes the same tree owes it the same. Without the drain the
// dispatch goroutine waits for a lock no client will ever release, on a tree the
// sweep has already deleted.
func TestRevalidateAuthorization_RevokedTreeCancelsParkedLock(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionNone}

	h, sessionID, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)

	gotStatus := make(chan types.Status, 1)
	if err := h.PendingLockRegistry.Register(&pending.PendingLock{
		SessionID: sessionID,
		TreeID:    treeID,
		MessageID: 11,
		AsyncId:   42,
		Callback: func(_, _, _ uint64, status types.Status, _ []byte) error {
			gotStatus <- status
			return nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	h.RevalidateAuthorization(context.Background())

	if _, ok := h.GetTree(treeID); ok {
		t.Fatal("fixture is wrong: the tree survived, so no teardown ran")
	}
	select {
	case status := <-gotStatus:
		if status != types.StatusRangeNotLocked {
			t.Errorf("parked LOCK completed with %v, want STATUS_RANGE_NOT_LOCKED", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked LOCK was never completed: the revoked tree left its dispatch " +
			"goroutine waiting for a lock no client can release")
	}
}

// TestTreeConnect_RefusesAfterSessionRevoked pins the window between resolving
// access and publishing the tree. The dispatch gate runs before the handler, so
// a sweep that revokes the session and removes its trees while TREE_CONNECT is
// resolving would otherwise find a tree published behind it — and because
// re-authentication clears the revocation, that tree would stand on the
// permission resolved for the user that was just retired.
func TestTreeConnect_RefusesAfterSessionRevoked(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionReadWrite}

	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, false)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}
	// The state the sweep leaves behind, reached here directly because the race
	// it models is what the guard exists to lose.
	sess.RevokeAuth()

	ctx := newTreeConnectTestContext(sessionID)
	res, err := h.TreeConnect(ctx, buildTreeConnectRequestBody("\\\\server\\export"))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	if res.Status != types.StatusNetworkSessionExpired {
		t.Errorf("Status = %#x, want STATUS_NETWORK_SESSION_EXPIRED: a revoked "+
			"session published a tree that a later re-auth would leave standing", res.Status)
	}
	if ctx.TreeID != 0 {
		t.Errorf("TreeID = %d, want 0: the tree was published anyway", ctx.TreeID)
	}
}

// TestRevalidateAuthorization_SIDResolverErrorDoesNotRaisePermission covers the
// second lookup. A Kerberos principal with no local account holds its whole
// grant in the SID table: its synthesized record carries no per-user rows, so
// the local lookup answers with the share default and the SID grant is the only
// thing that can restrict or raise it. A failure there therefore leaves the
// share default standing, and writing that back is the same promotion a failed
// local lookup would cause.
func TestRevalidateAuthorization_SIDResolverErrorDoesNotRaisePermission(t *testing.T) {
	uid := uint32(4242)
	// No ID: the sweep keeps a synthesized directory principal as-is rather
	// than looking it up, so this is the record the tree re-resolve sees.
	synth := &models.User{Username: "ad-user", UID: &uid, Enabled: true, SID: "S-1-5-21-1-2-3-1200"}
	store := &revalidateUserStore{
		user: synth,
		// No per-user row, so the local lookup answers with the share default.
		perm:   models.PermissionReadWrite,
		sidErr: errors.New("connection refused"),
	}

	h, sessionID, treeID := newRevalidateHandler(t, synth, store, models.PermissionRead, true)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}
	// A PAC identity is what puts the SID arm on the path at all.
	sess.SetPACIdentity([]string{"S-1-5-21-1-2-3-1104"}, "S-1-5-21-1-2-3-1200")

	h.RevalidateAuthorization(context.Background())

	tree, ok := h.GetTree(treeID)
	if !ok {
		t.Fatal("tree removed on a failed SID lookup; an outage must not revoke access either")
	}
	if tree.Permission != models.PermissionRead {
		t.Errorf("Permission = %v, want read: the SID lookup failed, so the share default "+
			"was left standing and written back as an authorization decision", tree.Permission)
	}
}

// TestRevalidateAuthorization_SweepsDoNotOverlap pins the serialization. Each
// control-plane mutation fires its own invalidation from its own goroutine, so
// without a lock two sweeps interleave: the earlier one resolves a grant, the
// later one resolves and stores the newer value, and the earlier one then
// stores its stale copy over it, restoring access that was just withdrawn.
//
// Asserting on the final permission would be timing-dependent. The invariant
// that is not is that no two sweeps are ever inside the resolve pass at once.
func TestRevalidateAuthorization_SweepsDoNotOverlap(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32

	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionRead}
	store.onResolve = func() {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		// Wide enough that an unserialized pair would overlap here.
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
	}

	h, _, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.RevalidateAuthorization(context.Background())
		}()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("%d sweeps were resolving at once; concurrent sweeps can publish out of "+
			"order and restore a withdrawn grant", got)
	}
}

// TestRevalidateAuthorization_ConcurrentReauthIsRaceFree runs the sweep against
// a re-authentication, which is what the invalidation goroutine and SESSION_SETUP
// do to the same session in production. It asserts nothing directly: the race
// detector is the assertion, and it fails on any unsynchronized read of the
// identity fields the resolver consults.
func TestRevalidateAuthorization_ConcurrentReauthIsRaceFree(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionRead}
	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 50 {
			h.RevalidateAuthorization(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		user := enabledUser()
		for i := range 50 {
			sess.UpdateIdentity(fmt.Sprintf("user-%d", i), "", user, false, false)
		}
	}()
	wg.Wait()
}

// TestRevalidateAuthorization_ReauthDuringLookupSurvives pins the decision that
// a re-authentication landing while the store lookup is in flight wins over the
// record the sweep read. The store reports the old identity as deleted, which
// without the generation check retires the session the client has just
// successfully re-authenticated.
func TestRevalidateAuthorization_ReauthDuringLookupSurvives(t *testing.T) {
	store := &revalidateUserStore{err: models.ErrUserNotFound, perm: models.PermissionReadWrite}
	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionNone, false)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("session %d missing", sessionID)
	}
	uid := uint32(1001)
	replacement := &models.User{ID: "user-2", Username: "bob", UID: &uid, Enabled: true}
	store.onGetUser = func() {
		sess.UpdateIdentity(replacement.Username, "", replacement, false, false)
	}

	h.RevalidateAuthorization(context.Background())

	if sess.AuthRevoked() {
		t.Fatal("session revoked on a record replaced by re-authentication mid-lookup; the re-authenticated identity must survive")
	}
	if got := sess.CurrentUser(); got == nil || got.Username != replacement.Username {
		t.Fatalf("session identity = %v, want the re-authenticated user %q", got, replacement.Username)
	}
}

// TestRevalidateAuthorization_ReauthDuringResolveLeavesTree is the same rule one
// pass over: a permission resolved against the old record must not be written
// onto a tree whose session has since re-authenticated, because MS-SMB2 keeps
// tree connections across a re-authentication and the grant belongs to an
// identity the session no longer holds.
func TestRevalidateAuthorization_ReauthDuringResolveLeavesTree(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionAdmin}
	h, sessionID, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionRead, true)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("session %d missing", sessionID)
	}
	uid := uint32(1001)
	replacement := &models.User{ID: "user-2", Username: "bob", UID: &uid, Enabled: true}
	store.onResolve = func() {
		sess.UpdateIdentity(replacement.Username, "", replacement, false, false)
	}

	h.RevalidateAuthorization(context.Background())

	tree, ok := h.GetTree(treeID)
	if !ok {
		t.Fatal("tree removed on a decision resolved for an identity the session no longer holds")
	}
	if tree.Permission != models.PermissionRead {
		t.Fatalf("tree Permission = %v, want %v: the old identity's grant was applied to the re-authenticated one",
			tree.Permission, models.PermissionRead)
	}
}

// TestIPCShare_RevokedSessionRefused covers the publication path TREE_CONNECT's
// own re-check does not reach. IPC$ returns before it, so a sweep that revokes
// between the dispatch gate and here would otherwise leave a tree that outlives
// the revocation, since re-authentication clears the flag and keeps trees.
func TestIPCShare_RevokedSessionRefused(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionReadWrite}
	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionNone, false)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("session %d missing", sessionID)
	}
	sess.RevokeAuth()

	before := countTrees(h)
	res, err := h.handleIPCShare(&SMBHandlerContext{Context: context.Background(), SessionID: sessionID})
	if err != nil {
		t.Fatalf("handleIPCShare: %v", err)
	}
	if res.Status != types.StatusNetworkSessionExpired {
		t.Fatalf("IPC$ status = %#x, want %#x for a revoked session", res.Status, types.StatusNetworkSessionExpired)
	}
	if after := countTrees(h); after != before {
		t.Fatalf("tree count %d -> %d: a revoked session published an IPC$ tree", before, after)
	}
}

func countTrees(h *Handler) int {
	n := 0
	h.trees.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestRevalidateAuthorization_UserlessSessionSweptWithoutUserStore covers the
// sweep's entry guard. A guest or anonymous session holds no user record, so
// there is nothing to re-read for it; its access rests entirely on the share
// default, which the tree pass re-resolves. Returning early when no user store
// is configured skipped that pass too, leaving such a tree pinned at whatever
// permission it was granted with for as long as the connection lived.
func TestRevalidateAuthorization_UserlessSessionSweptWithoutUserStore(t *testing.T) {
	h, sessionID, treeID := newRevalidateHandler(t, enabledUser(), nil, models.PermissionAdmin, true)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}
	// A guest session: no record, so the tree pass is the only thing that can
	// re-decide its access.
	sess.User = nil
	sess.IsGuest = true

	h.RevalidateAuthorization(context.Background())

	tree, ok := h.GetTree(treeID)
	if !ok {
		t.Fatal("tree removed: the share default is read-write, not none")
	}
	if tree.Permission != models.PermissionReadWrite {
		t.Errorf("Permission = %v, want read-write: the sweep returned before the tree "+
			"pass, so a guest tree kept the admin grant it was connected with", tree.Permission)
	}
}

// TestReplaceTree_DoesNotResurrectARemovedTree covers the republish primitive.
// The sweep reads a tree, copies it, and writes the copy back under the same
// ID. A plain store makes that write unconditional, so a TREE_DISCONNECT or a
// session teardown landing in between is undone: the tree the client gave up
// reappears, carrying a permission the sweep resolved for it.
func TestReplaceTree_DoesNotResurrectARemovedTree(t *testing.T) {
	h := NewHandler()

	treeID := h.GenerateTreeID()
	original := &TreeConnection{TreeID: treeID, SessionID: 7, ShareName: "/export", Permission: models.PermissionRead}
	h.StoreTree(original)

	updated := *original
	updated.Permission = models.PermissionReadWrite
	if !h.ReplaceTree(original, &updated) {
		t.Fatal("ReplaceTree refused an unchanged tree: a re-resolve can never apply")
	}
	if got, _ := h.GetTree(treeID); got.Permission != models.PermissionReadWrite {
		t.Errorf("Permission = %v, want read-write", got.Permission)
	}

	// The teardown the sweep races.
	h.DeleteTree(treeID)

	stale := updated
	stale.Permission = models.PermissionAdmin
	if h.ReplaceTree(&updated, &stale) {
		t.Error("ReplaceTree swapped into a tree that was torn down")
	}
	if _, ok := h.GetTree(treeID); ok {
		t.Error("the removed tree is back: a disconnected tree was republished by the re-check")
	}
}

// TestResolveSharePermission_RunsOnOneIdentity covers the identity mixing the
// snapshot exists to prevent. Resolution reads two halves of the identity — the
// user record for the local grant, the PAC SIDs for the directory grant — and
// merges the results. Read separately, a re-authentication landing between them
// authorizes one principal's record beside another's group SIDs, a combination
// the session never held.
func TestResolveSharePermission_RunsOnOneIdentity(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionRead}
	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionRead, false)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}
	sess.SetPACIdentity([]string{"S-1-5-21-1-2-3-1104"}, "S-1-5-21-1-2-3-1200")

	// What a caller decided about.
	snap := sess.AuthzIdentity()

	// The re-authentication that lands while the decision is in flight.
	replacement := &models.User{ID: "user-2", Username: "bob", Enabled: true}
	sess.UpdateIdentity("bob", "", replacement, false, false)
	sess.SetPACIdentity([]string{"S-1-5-21-9-9-9-5104"}, "S-1-5-21-9-9-9-5200")

	share, err := h.Registry.GetShare("/export")
	if err != nil || share == nil {
		t.Fatalf("GetShare: %v", err)
	}
	_, identifier, _ := resolveSharePermissionForIdentity(
		&SMBHandlerContext{Context: context.Background()},
		sess, snap, share, models.PermissionRead, store,
	)

	if identifier != "alice" {
		t.Errorf("identifier = %q, want alice: the resolution followed the session past "+
			"the identity it was handed", identifier)
	}
	for _, sid := range store.gotSIDs {
		if strings.HasPrefix(sid, "S-1-5-21-9-9-9-") {
			t.Fatalf("SID lookup used %q: the replacement principal's group grants were "+
				"applied to the record the caller decided about", sid)
		}
	}
	if len(store.gotSIDs) == 0 {
		t.Fatal("no SID lookup ran, so this test cannot see which identity it used")
	}
}

// TestTreeConnect_RefusesAfterSessionLoggedOff covers the teardown direction the
// revocation check cannot see. LOGOFF marks the session and then removes its
// trees, so a TREE_CONNECT already past the mark publishes after that sweep has
// run: the client is handed a tree ID belonging to a session that is gone, and
// the entry outlives it.
func TestTreeConnect_RefusesAfterSessionLoggedOff(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionReadWrite}
	h, sessionID, _ := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, false)

	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}
	// LOGOFF's first act, reached directly: the handler is already past the
	// dispatch gate, which is exactly the window the guard covers.
	sess.LoggedOff.Store(true)

	ctx := newTreeConnectTestContext(sessionID)
	res, err := h.TreeConnect(ctx, buildTreeConnectRequestBody("\\\\server\\export"))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	if res.Status != types.StatusUserSessionDeleted {
		t.Errorf("Status = %#x, want STATUS_USER_SESSION_DELETED: a tree was published on "+
			"a session that had already been torn down", res.Status)
	}
	if ctx.TreeID != 0 {
		if _, alive := h.GetTree(ctx.TreeID); alive {
			t.Error("the tree outlived the session that owned it")
		}
	}
}
