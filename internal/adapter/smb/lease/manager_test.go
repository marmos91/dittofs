package lease

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// fakeLockManager is a minimal recording fake for lock.LockManager. It embeds
// the interface so any method we don't need is implicitly nil and will panic
// if a test path unexpectedly calls it.
type fakeLockManager struct {
	lock.LockManager // embedded interface; unimplemented methods will panic if called

	mu sync.Mutex

	breakHandleCalls           []breakCall
	breakReadCalls             []breakCall
	waitForBreakCompletionKeys []string
	// callOrder records the relative order of all observed calls so tests can
	// assert that BreakLeasesOnOpenConflict / BreakReadLeasesForParentDir
	// happen BEFORE WaitForBreakCompletion returns.
	callOrder []string

	// waitBlock, if non-nil, blocks WaitForBreakCompletion until closed
	// (used to assert no-deadlock behavior in the exclude-triggering-client test).
	waitBlock chan struct{}
}

type breakCall struct {
	HandleKey    string
	ExcludeOwner *lock.LockOwner
	Reason       lock.BreakReason
}

func (f *fakeLockManager) BreakLeasesOnOpenConflict(handleKey string, excludeOwner *lock.LockOwner, reason lock.BreakReason) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakHandleCalls = append(f.breakHandleCalls, breakCall{
		HandleKey:    handleKey,
		ExcludeOwner: excludeOwner,
		Reason:       reason,
	})
	f.callOrder = append(f.callOrder, "BreakLeasesOnOpenConflict")
	return nil
}

func (f *fakeLockManager) BreakReadLeasesForParentDir(handleKey string, excludeOwner *lock.LockOwner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakReadCalls = append(f.breakReadCalls, breakCall{HandleKey: handleKey, ExcludeOwner: excludeOwner})
	f.callOrder = append(f.callOrder, "BreakReadLeasesForParentDir")
	return nil
}

func (f *fakeLockManager) WaitForBreakCompletion(ctx context.Context, handleKey string) error {
	f.mu.Lock()
	f.waitForBreakCompletionKeys = append(f.waitForBreakCompletionKeys, handleKey)
	f.callOrder = append(f.callOrder, "WaitForBreakCompletion")
	wb := f.waitBlock
	f.mu.Unlock()
	if wb != nil {
		select {
		case <-wb:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// fakeResolver returns the same fakeLockManager for any share name.
type fakeResolver struct {
	mgr lock.LockManager
}

func (r *fakeResolver) GetLockManagerForShare(_ string) lock.LockManager {
	return r.mgr
}

// TestBreakParentHandleLeasesOnCreate_WaitsForAck asserts that
// BreakParentHandleLeasesOnCreate calls WaitForBreakCompletion AFTER
// BreakLeasesOnOpenConflict and BEFORE returning. Per MS-SMB2 3.3.4.7, the
// server must wait for LEASE_BREAK_ACK before completing the triggering CREATE.
// The parent-dir break uses BreakReasonSharingViolation to select the
// Handle-strip mask (MS-FSA 2.1.5.14: child-set change invalidates directory
// Handle cache).
// TestMarkLeaseVersionIfUnset_StickyV2: a lease first marked V2 stays V2
// even when MarkLeaseVersionIfUnset is later called with isV2=false. Per
// smbtorture v2_epoch2 the V2 lease keeps producing V2 responses for V1
// reopens on the same key.
func TestMarkLeaseVersionIfUnset_StickyV2(t *testing.T) {
	t.Parallel()

	lm := NewLeaseManager(nil, nil)
	key := [16]byte{0xAA}

	lm.MarkLeaseVersionIfUnset(key, true) // first grant: V2
	if !lm.IsV2(key) {
		t.Fatalf("expected IsV2=true after V2 first grant")
	}
	if !lm.IsLeaseVersionKnown(key) {
		t.Fatalf("expected version known after first grant")
	}

	lm.MarkLeaseVersionIfUnset(key, false) // V1 reopen — must NOT downgrade
	if !lm.IsV2(key) {
		t.Errorf("V1 reopen wrongly cleared V2 mark")
	}
}

// TestMarkLeaseVersionIfUnset_StickyV1: a lease first marked V1 stays V1
// even when a later request is V2 (smbtorture v2_epoch3).
func TestMarkLeaseVersionIfUnset_StickyV1(t *testing.T) {
	t.Parallel()

	lm := NewLeaseManager(nil, nil)
	key := [16]byte{0xBB}

	lm.MarkLeaseVersionIfUnset(key, false) // first grant: V1
	if lm.IsV2(key) {
		t.Fatalf("V1 first grant wrongly set V2 mark")
	}
	if !lm.IsLeaseVersionKnown(key) {
		t.Fatalf("expected version known after first grant")
	}

	lm.MarkLeaseVersionIfUnset(key, true) // V2 upgrade — must NOT promote
	if lm.IsV2(key) {
		t.Errorf("V2 upgrade wrongly upgraded V1 lease to V2")
	}
}

// TestGetSessionForBreak_RoutesByClientGUIDPrimary covers the
// smbtorture smb2.lease.v2_complex1 routing requirement: when two sessions
// of the same ClientGUID open leases on the same file (LEASE1 from session
// 1, LEASE2 from session 2), every lease break MUST be delivered on the
// FIRST session of that client (Samba `smbXsrv_pending_break_submit` head
// of `client->connections`). Per MS-SMB2 §3.3.4.7 the break is a
// client-level notification, not a session-level one.
//
// Pre-fix code keyed break dispatch off the per-lease sessionMap (whichever
// session was last to call RequestLease wins), which sent LEASE2's break
// on session 2's transport and tripped the test's
// CHECK_BREAK_INFO_V2(tree1a->session->transport, ...) assertion.
func TestGetSessionForBreak_RoutesByClientGUIDPrimary(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&fakeResolver{mgr: mgr}, nil)

	clientGUID := [16]byte{0xCA, 0xFE, 0xBA, 0xBE}
	const (
		session1 uint64 = 100 // tree1a — first connection of the client
		session2 uint64 = 200 // tree1b — same ClientGUID, established later
	)
	lease1 := [16]byte{0x01}
	lease2 := [16]byte{0x02}
	fh := lock.FileHandle("file-1")

	// Session 1 grants LEASE1 R first (registers (clientGUID, session1)
	// as primary).
	if _, _, err := lm.RequestLease(context.Background(), fh,
		lease1, [16]byte{}, session1, clientGUID,
		"owner-1", "client-1", "share1",
		lock.LeaseStateRead, false); err != nil {
		t.Fatalf("session1 RequestLease(LEASE1, R): %v", err)
	}

	// Session 2 (same ClientGUID) requests LEASE2 R on the same file —
	// must NOT bump the primary session for the ClientGUID.
	if _, _, err := lm.RequestLease(context.Background(), fh,
		lease2, [16]byte{}, session2, clientGUID,
		"owner-2", "client-2", "share1",
		lock.LeaseStateRead, false); err != nil {
		t.Fatalf("session2 RequestLease(LEASE2, R): %v", err)
	}

	// Both leases must route their breaks to session1 — the FIRST session
	// of the shared ClientGUID — not to whichever session opened the lease.
	// LEASE2's per-lease sessionMap entry initially points at session2, so
	// this assertion specifically pins the ClientGUID-aware override.
	if sid, ok := lm.GetSessionForBreak(lease1, "client-1"); !ok || sid != session1 {
		t.Errorf("LEASE1 break session = %d (ok=%v), want %d", sid, ok, session1)
	}
	if sid, ok := lm.GetSessionForBreak(lease2, "client-2"); !ok || sid != session1 {
		t.Errorf("LEASE2 break session = %d (ok=%v), want %d (ClientGUID primary, NOT per-lease sessionMap session2=%d)",
			sid, ok, session1, session2)
	}

	// The legacy GetSessionForLease still returns the per-lease sessionMap
	// (used by other paths that genuinely want the registering session;
	// none currently fault on this distinction but the API contract is
	// preserved for callers that do).
	if sid, _ := lm.GetSessionForLease(lease2); sid != session2 {
		t.Errorf("GetSessionForLease(LEASE2) = %d, want %d (per-lease sessionMap unchanged)",
			sid, session2)
	}
}

// TestGetSessionForBreak_FallsBackToSessionMap covers the legacy /
// test-context path where ClientGUID is unknown (zero) — typical for unit
// tests that don't wire a CryptoState. Break dispatch must continue to
// route via the per-lease sessionMap so we don't regress any existing
// single-session lease test.
func TestGetSessionForBreak_FallsBackToSessionMap(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&fakeResolver{mgr: mgr}, nil)

	const session1 uint64 = 42
	leaseKey := [16]byte{0xAB}
	fh := lock.FileHandle("file-1")

	if _, _, err := lm.RequestLease(context.Background(), fh,
		leaseKey, [16]byte{}, session1, [16]byte{}, // zero ClientGUID
		"owner-1", "client-1", "share1",
		lock.LeaseStateRead, false); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	if sid, ok := lm.GetSessionForBreak(leaseKey, "client-1"); !ok || sid != session1 {
		t.Errorf("GetSessionForBreak(zero GUID) = %d (ok=%v), want %d (sessionMap fallback)",
			sid, ok, session1)
	}
}

// TestReleaseSessionLeases_ReapsClientPrimary verifies that when a session
// is torn down (LOGOFF / disconnect), any clientPrimarySession entry that
// pointed at it is removed. Without this the next break for a lease bound
// to that ClientGUID would route to a dead sessionID and the notifier
// would silently drop the notification.
func TestReleaseSessionLeases_ReapsClientPrimary(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&fakeResolver{mgr: mgr}, nil)

	clientGUID := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}
	const session1 uint64 = 100
	leaseKey := [16]byte{0x07}
	fh := lock.FileHandle("file-1")

	if _, _, err := lm.RequestLease(context.Background(), fh,
		leaseKey, [16]byte{}, session1, clientGUID,
		"owner-1", "client-1", "share1",
		lock.LeaseStateRead, false); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	// Sanity: primary is registered.
	lm.mu.RLock()
	if got := lm.clientPrimarySession[clientGUID]; got != session1 {
		lm.mu.RUnlock()
		t.Fatalf("primary registered = %d, want %d", got, session1)
	}
	lm.mu.RUnlock()

	if err := lm.ReleaseSessionLeases(context.Background(), session1); err != nil {
		t.Fatalf("ReleaseSessionLeases: %v", err)
	}

	lm.mu.RLock()
	defer lm.mu.RUnlock()
	if _, present := lm.clientPrimarySession[clientGUID]; present {
		t.Errorf("clientPrimarySession[%x] still present after session %d torn down",
			clientGUID, session1)
	}
}

// TestGetSessionForBreak_CrossClientSameLeaseKey_IsolatesByClientID covers
// the per-client uniqueness of lease keys: lock.Manager scopes lease-key
// reuse by Owner.ClientID (round-3 lease_match), so two distinct SMB
// clients may each hold a record under the same numeric LeaseKey on
// different files. The composite-keyed leaseClientGUID map must keep
// their break-routing bindings isolated — client B's break must NOT route
// to client A's primary session even when their leaseKeys collide.
func TestGetSessionForBreak_CrossClientSameLeaseKey_IsolatesByClientID(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&fakeResolver{mgr: mgr}, nil)

	guidA := [16]byte{0xAA, 0xAA}
	guidB := [16]byte{0xBB, 0xBB}
	const (
		sessionA uint64 = 11
		sessionB uint64 = 22
	)
	leaseKey := [16]byte{0xEE} // same numeric key, different clients

	// Client A grants on file-A.
	if _, _, err := lm.RequestLease(context.Background(), lock.FileHandle("file-A"),
		leaseKey, [16]byte{}, sessionA, guidA,
		"owner-A", "client-A", "share1",
		lock.LeaseStateRead, false); err != nil {
		t.Fatalf("client A RequestLease: %v", err)
	}

	// Client B grants the SAME numeric leaseKey on file-B (per-client
	// namespace allows this — round-3 lease_match scopes by ClientID).
	if _, _, err := lm.RequestLease(context.Background(), lock.FileHandle("file-B"),
		leaseKey, [16]byte{}, sessionB, guidB,
		"owner-B", "client-B", "share1",
		lock.LeaseStateRead, false); err != nil {
		t.Fatalf("client B RequestLease: %v", err)
	}

	// Each client's break must route to its own primary session.
	if sid, ok := lm.GetSessionForBreak(leaseKey, "client-A"); !ok || sid != sessionA {
		t.Errorf("client A break = %d (ok=%v), want %d", sid, ok, sessionA)
	}
	if sid, ok := lm.GetSessionForBreak(leaseKey, "client-B"); !ok || sid != sessionB {
		t.Errorf("client B break = %d (ok=%v), want %d", sid, ok, sessionB)
	}
}

// TestReleaseSessionLeases_ReElectsPrimaryFromSurvivors covers
// `client->connections` rehoming on Samba-style session teardown: when the
// primary session of a ClientGUID is released, the next-oldest surviving
// session of the same ClientGUID must be elected as the new primary so
// future breaks continue to route at the client level rather than falling
// through to the per-lease sessionMap (last-write-wins).
func TestReleaseSessionLeases_ReElectsPrimaryFromSurvivors(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&fakeResolver{mgr: mgr}, nil)

	clientGUID := [16]byte{0xC0, 0xFF, 0xEE}
	const (
		sessionOldest uint64 = 10 // initial primary
		sessionMiddle uint64 = 20
		sessionNewest uint64 = 30
	)
	lease1 := [16]byte{0x01}
	lease2 := [16]byte{0x02}
	lease3 := [16]byte{0x03}
	fh := lock.FileHandle("file-elect")

	// All three sessions are the same client (same clientID) holding
	// distinct leases on the same file. First grant elects sessionOldest
	// as primary; subsequent grants must NOT bump.
	for _, tc := range []struct {
		key [16]byte
		sid uint64
	}{{lease1, sessionOldest}, {lease2, sessionMiddle}, {lease3, sessionNewest}} {
		if _, _, err := lm.RequestLease(context.Background(), fh,
			tc.key, [16]byte{}, tc.sid, clientGUID,
			"owner-X", "client-X", "share1",
			lock.LeaseStateRead, false); err != nil {
			t.Fatalf("RequestLease(sid=%d): %v", tc.sid, err)
		}
	}

	// Release the oldest (the current primary). Re-election must pick
	// sessionMiddle (smallest surviving sessionID).
	if err := lm.ReleaseSessionLeases(context.Background(), sessionOldest); err != nil {
		t.Fatalf("ReleaseSessionLeases(oldest): %v", err)
	}

	lm.mu.RLock()
	got, present := lm.clientPrimarySession[clientGUID]
	lm.mu.RUnlock()
	if !present {
		t.Fatalf("primary missing after release; want re-elected to %d", sessionMiddle)
	}
	if got != sessionMiddle {
		t.Errorf("re-elected primary = %d, want %d (smallest surviving sessionID)", got, sessionMiddle)
	}
}

// TestMarkLeaseVersionIfUnset_UnknownByDefault: until first grant marks the
// lease, IsLeaseVersionKnown returns false (so the response-encoding path
// falls back to the request's format).
func TestMarkLeaseVersionIfUnset_UnknownByDefault(t *testing.T) {
	t.Parallel()

	lm := NewLeaseManager(nil, nil)
	key := [16]byte{0xCC}
	if lm.IsLeaseVersionKnown(key) {
		t.Errorf("fresh key should not be marked known")
	}
	if lm.IsV2(key) {
		t.Errorf("fresh key should default to !IsV2")
	}
}

func TestBreakParentHandleLeasesOnCreate_WaitsForAck(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentHandle := lock.FileHandle("parent-dir-handle")
	if err := lm.BreakParentHandleLeasesOnCreate(context.Background(), parentHandle, "share1", "smb:A", [16]byte{}, false); err != nil {
		t.Fatalf("BreakParentHandleLeasesOnCreate returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1", got)
	}
	if fake.breakHandleCalls[0].HandleKey != string(parentHandle) {
		t.Errorf("BreakLeasesOnOpenConflict handleKey = %q, want %q",
			fake.breakHandleCalls[0].HandleKey, string(parentHandle))
	}
	if got := fake.breakHandleCalls[0].Reason; got != lock.BreakReasonSharingViolation {
		t.Errorf("parent-dir Handle break must pass BreakReasonSharingViolation (strip Handle mask); got %d", got)
	}

	if got := len(fake.waitForBreakCompletionKeys); got != 1 {
		t.Fatalf("WaitForBreakCompletion call count = %d, want 1 (parent break must wait for ack per MS-SMB2 3.3.4.7)", got)
	}
	if fake.waitForBreakCompletionKeys[0] != string(parentHandle) {
		t.Errorf("WaitForBreakCompletion handleKey = %q, want %q",
			fake.waitForBreakCompletionKeys[0], string(parentHandle))
	}

	// Order: break must come before wait.
	if len(fake.callOrder) < 2 {
		t.Fatalf("expected at least 2 calls in order, got %v", fake.callOrder)
	}
	if fake.callOrder[0] != "BreakLeasesOnOpenConflict" {
		t.Errorf("first call = %q, want BreakLeasesOnOpenConflict", fake.callOrder[0])
	}
	if fake.callOrder[1] != "WaitForBreakCompletion" {
		t.Errorf("second call = %q, want WaitForBreakCompletion", fake.callOrder[1])
	}
}

// TestBreakParentReadLeasesOnModify_WaitsForAck asserts the same ack-wait
// guarantee for BreakParentReadLeasesOnModify.
func TestBreakParentReadLeasesOnModify_WaitsForAck(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentHandle := lock.FileHandle("parent-dir-handle-2")
	if err := lm.BreakParentReadLeasesOnModify(context.Background(), parentHandle, "share1", "smb:A", [16]byte{}, false); err != nil {
		t.Fatalf("BreakParentReadLeasesOnModify returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakReadCalls); got != 1 {
		t.Fatalf("BreakReadLeasesForParentDir call count = %d, want 1", got)
	}
	if fake.breakReadCalls[0].HandleKey != string(parentHandle) {
		t.Errorf("BreakReadLeasesForParentDir handleKey = %q, want %q",
			fake.breakReadCalls[0].HandleKey, string(parentHandle))
	}

	if got := len(fake.waitForBreakCompletionKeys); got != 1 {
		t.Fatalf("WaitForBreakCompletion call count = %d, want 1 (parent break must wait for ack per MS-SMB2 3.3.4.7)", got)
	}
	if fake.waitForBreakCompletionKeys[0] != string(parentHandle) {
		t.Errorf("WaitForBreakCompletion handleKey = %q, want %q",
			fake.waitForBreakCompletionKeys[0], string(parentHandle))
	}

	if len(fake.callOrder) < 2 {
		t.Fatalf("expected at least 2 calls in order, got %v", fake.callOrder)
	}
	if fake.callOrder[0] != "BreakReadLeasesForParentDir" {
		t.Errorf("first call = %q, want BreakReadLeasesForParentDir", fake.callOrder[0])
	}
	if fake.callOrder[1] != "WaitForBreakCompletion" {
		t.Errorf("second call = %q, want WaitForBreakCompletion", fake.callOrder[1])
	}
}

// TestBreakParentHandle_ExcludesTriggeringClient asserts that the triggering
// CREATE's clientID is forwarded as the excludeOwner so that the triggering
// session's own parent-dir lease (if any) is NOT in the breakable set, and
// that the function honours its caller's context cancellation rather than
// blocking forever — proving the wait cannot deadlock the triggering CREATE.
func TestBreakParentHandle_ExcludesTriggeringClient(t *testing.T) {
	t.Parallel()

	// waitBlock simulates an outstanding break that never gets acked. With a
	// bounded context the call must return when the context expires, NOT
	// deadlock indefinitely.
	fake := &fakeLockManager{
		waitBlock: make(chan struct{}),
	}
	defer close(fake.waitBlock)

	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	// Use a short caller-side timeout to bound the test.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- lm.BreakParentHandleLeasesOnCreate(ctx, lock.FileHandle("parent"), "share1", "smb:A", [16]byte{}, false)
	}()

	select {
	case <-done:
		// Returned (either nil or ctx.DeadlineExceeded). Both are acceptable —
		// what we are asserting is that the call does NOT block forever.
	case <-time.After(2 * time.Second):
		t.Fatal("BreakParentHandleLeasesOnCreate deadlocked: did not return within 2s of caller context expiry")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.breakHandleCalls) != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1", len(fake.breakHandleCalls))
	}
	excludeOwner := fake.breakHandleCalls[0].ExcludeOwner
	if excludeOwner == nil {
		t.Fatal("excludeOwner is nil; the triggering client's session must be excluded from the breakable set")
	}
	if excludeOwner.ClientID != "smb:A" {
		t.Errorf("excludeOwner.ClientID = %q, want %q", excludeOwner.ClientID, "smb:A")
	}

	// And we must have actually attempted to wait — proving the new contract
	// is wired (rather than the test passing trivially against unmodified code).
	if len(fake.waitForBreakCompletionKeys) != 1 {
		t.Fatalf("WaitForBreakCompletion call count = %d, want 1", len(fake.waitForBreakCompletionKeys))
	}
}

// TestBreakParentHandleLeasesOnCreate_ParentKeyPlumbed asserts the new
// (#470 C2) parent-key suppression args are forwarded into the LockOwner
// passed to BreakLeasesOnOpenConflict. The dir-lease parent-key suppression
// rule (MS-SMB2 §3.3.4.20) is enforced INSIDE the lock manager — this test
// pins that the LeaseManager does the plumbing, not the actual suppression.
func TestBreakParentHandleLeasesOnCreate_ParentKeyPlumbed(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentKey := [16]byte{0xCA, 0xFE, 0xBA, 0xBE}
	if err := lm.BreakParentHandleLeasesOnCreate(
		context.Background(), lock.FileHandle("parent"), "share1", "smb:A", parentKey, true,
	); err != nil {
		t.Fatalf("BreakParentHandleLeasesOnCreate returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.breakHandleCalls) != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1", len(fake.breakHandleCalls))
	}
	excludeOwner := fake.breakHandleCalls[0].ExcludeOwner
	if excludeOwner == nil {
		t.Fatal("excludeOwner must be non-nil when a parent_key is provided")
	}
	if !excludeOwner.HasExcludeParentDirLeaseKey {
		t.Errorf("HasExcludeParentDirLeaseKey = false, want true")
	}
	if excludeOwner.ExcludeParentDirLeaseKey != parentKey {
		t.Errorf("ExcludeParentDirLeaseKey = %x, want %x", excludeOwner.ExcludeParentDirLeaseKey, parentKey)
	}
}

// TestBreakParentReadLeasesOnModify_ParentKeyPlumbed: same plumbing assertion
// for the Read-lease break helper (the SET_INFO path on a child file).
func TestBreakParentReadLeasesOnModify_ParentKeyPlumbed(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentKey := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}
	if err := lm.BreakParentReadLeasesOnModify(
		context.Background(), lock.FileHandle("parent"), "share1", "smb:A", parentKey, true,
	); err != nil {
		t.Fatalf("BreakParentReadLeasesOnModify returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.breakReadCalls) != 1 {
		t.Fatalf("BreakReadLeasesForParentDir call count = %d, want 1", len(fake.breakReadCalls))
	}
	excludeOwner := fake.breakReadCalls[0].ExcludeOwner
	if excludeOwner == nil {
		t.Fatal("excludeOwner must be non-nil when a parent_key is provided")
	}
	if !excludeOwner.HasExcludeParentDirLeaseKey {
		t.Errorf("HasExcludeParentDirLeaseKey = false, want true")
	}
	if excludeOwner.ExcludeParentDirLeaseKey != parentKey {
		t.Errorf("ExcludeParentDirLeaseKey = %x, want %x", excludeOwner.ExcludeParentDirLeaseKey, parentKey)
	}
}

// TestBreakParentHandleLeasesOnCreate_NoParentKey_NoHasFlag: when hasExcludeKey
// is false the LockOwner must not surface HasExcludeParentDirLeaseKey=true
// (which would suppress every dir lease whose key happens to be all zeros).
func TestBreakParentHandleLeasesOnCreate_NoParentKey_NoHasFlag(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	if err := lm.BreakParentHandleLeasesOnCreate(
		context.Background(), lock.FileHandle("parent"), "share1", "smb:A", [16]byte{}, false,
	); err != nil {
		t.Fatalf("BreakParentHandleLeasesOnCreate returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.breakHandleCalls) != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1", len(fake.breakHandleCalls))
	}
	excludeOwner := fake.breakHandleCalls[0].ExcludeOwner
	if excludeOwner == nil {
		t.Fatal("excludeOwner is nil")
	}
	if excludeOwner.HasExcludeParentDirLeaseKey {
		t.Error("HasExcludeParentDirLeaseKey = true, want false (no parent_key was provided)")
	}
}

// TestBreakLeasesOnRename_SrcOnly_StripHandle: a non-overwrite rename
// dispatches exactly one break on the source handle with
// BreakReasonSharingViolation (strip H). Per smbtorture rename_wait the
// non-renamer's RH must be downgraded to R; per v2_rename the renamer's own
// lease must NOT be touched. Verified by ExcludeLeaseKey scoping.
func TestBreakLeasesOnRename_SrcOnly_StripHandle(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	srcHandle := lock.FileHandle("src-handle")
	renamerKey := [16]byte{0x11, 0x22, 0x33}

	if err := lm.BreakLeasesOnRename(srcHandle, "", "share1", renamerKey, false); err != nil {
		t.Fatalf("BreakLeasesOnRename returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1 (src only)", got)
	}
	got := fake.breakHandleCalls[0]
	if got.HandleKey != string(srcHandle) {
		t.Errorf("handleKey = %q, want %q", got.HandleKey, string(srcHandle))
	}
	if got.Reason != lock.BreakReasonSharingViolation {
		t.Errorf("reason = %d, want BreakReasonSharingViolation (strip H)", got.Reason)
	}
	if got.ExcludeOwner == nil {
		t.Fatal("ExcludeOwner is nil; renamer's own lease key must be excluded")
	}
	if got.ExcludeOwner.ExcludeLeaseKey != renamerKey {
		t.Errorf("ExcludeLeaseKey = %x, want %x", got.ExcludeOwner.ExcludeLeaseKey, renamerKey)
	}
	// MUST NOT scope by ClientID — a single client may hold two distinct
	// leases on the same file (smbtorture rename_wait LEASE1=h1 / LEASE2=h2);
	// ClientID-scoped exclusion would skip both.
	if got.ExcludeOwner.ClientID != "" {
		t.Errorf("ExcludeOwner.ClientID = %q, must be empty (rename exclusion is by lease key only)", got.ExcludeOwner.ClientID)
	}

	// Fire-and-forget: rename dispatch must NOT call WaitForBreakCompletion
	// inline. The set_info handler routes the wait through the round-4 async
	// park path so the request appears as STATUS_PENDING to the client.
	if len(fake.waitForBreakCompletionKeys) != 0 {
		t.Errorf("WaitForBreakCompletion called %d times; want 0 (rename break is fire-and-forget; caller routes the wait)",
			len(fake.waitForBreakCompletionKeys))
	}
}

// TestBreakLeasesOnRename_OverwriteBreaksBoth: an overwrite rename onto a
// non-empty destination dispatches breaks on BOTH source AND destination.
// Per smbtorture v2_rename_target_overwrite the dst's RWH must break to RW
// (strip H). The destination break is unscoped — the dst's lease holder is by
// definition someone other than the renamer.
func TestBreakLeasesOnRename_OverwriteBreaksBoth(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	srcHandle := lock.FileHandle("src-handle")
	dstHandle := lock.FileHandle("dst-handle")
	renamerKey := [16]byte{0xAA}

	if err := lm.BreakLeasesOnRename(srcHandle, dstHandle, "share1", renamerKey, true); err != nil {
		t.Fatalf("BreakLeasesOnRename returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 2 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 2 (src + dst)", got)
	}

	// Order is src first, then dst — matches the rename code path order.
	if fake.breakHandleCalls[0].HandleKey != string(srcHandle) {
		t.Errorf("first break handleKey = %q, want %q (src)", fake.breakHandleCalls[0].HandleKey, string(srcHandle))
	}
	if fake.breakHandleCalls[1].HandleKey != string(dstHandle) {
		t.Errorf("second break handleKey = %q, want %q (dst)", fake.breakHandleCalls[1].HandleKey, string(dstHandle))
	}
	if fake.breakHandleCalls[1].Reason != lock.BreakReasonSharingViolation {
		t.Errorf("dst break reason = %d, want BreakReasonSharingViolation", fake.breakHandleCalls[1].Reason)
	}
	// Destination break must NOT carry the renamer's lease key as exclusion —
	// the dst's lease is by definition held by someone other than the renamer.
	if fake.breakHandleCalls[1].ExcludeOwner != nil {
		t.Errorf("dst break excludeOwner = %+v, want nil (no exclusion on cross-file destination)", fake.breakHandleCalls[1].ExcludeOwner)
	}
}

// TestBreakLeasesOnRename_NonOverwrite_NoDstBreak: when isOverwrite=false the
// destination is NOT broken even if a dstHandle is supplied. (Defensive: caller
// usually passes an empty dstHandle when not overwriting; this test guards
// against a future refactor introducing a stray break on a name that may not
// exist.)
func TestBreakLeasesOnRename_NonOverwrite_NoDstBreak(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	if err := lm.BreakLeasesOnRename(lock.FileHandle("src"), lock.FileHandle("dst"), "share1", [16]byte{}, false); err != nil {
		t.Fatalf("BreakLeasesOnRename returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1 (src only when !isOverwrite)", got)
	}
	if fake.breakHandleCalls[0].HandleKey != "src" {
		t.Errorf("handleKey = %q, want \"src\"", fake.breakHandleCalls[0].HandleKey)
	}
}

// TestBreakLeasesOnRename_SameSrcDst_NoDstBreak: a self-rename (src==dst)
// dispatches only one break — never two on the same handle. Guards against an
// accidental double-break that would advance the epoch twice.
func TestBreakLeasesOnRename_SameSrcDst_NoDstBreak(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	h := lock.FileHandle("self")
	if err := lm.BreakLeasesOnRename(h, h, "share1", [16]byte{}, true); err != nil {
		t.Fatalf("BreakLeasesOnRename returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1 (self-rename src==dst)", got)
	}
}

// TestBreakFileHandleLeasesOnDelete_StripsHandleAndExcludesSetterKey verifies
// the contract used by SET_INFO FileDispositionInformation (set_info.go) and
// the session/tree teardown DOC path (handler.go::handleDeleteOnClose):
//
//   - the break is dispatched against the file's own handle key (not the parent),
//   - reason = BreakReasonSharingViolation so ComputeLeaseBreakTo strips Handle
//     (RH→R, RWH→RW) per MS-SMB2 3.3.5.9 / Samba delay_for_oplock_fn,
//   - the setter's own LeaseKey is excluded so MS-SMB2 nobreakself holds.
//
// Required by smbtorture smb2.lease.unlink (the SetDelete-on-close on h2 must
// dispatch a break to LEASE1 on h1, not to h2's own LEASE2).
func TestBreakFileHandleLeasesOnDelete_StripsHandleAndExcludesSetterKey(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	fileHandle := lock.FileHandle("file-unlink")
	setterKey := [16]byte{0x77, 0x88}

	if err := lm.BreakFileHandleLeasesOnDelete(
		fileHandle, "share1", &lock.LockOwner{ExcludeLeaseKey: setterKey},
	); err != nil {
		t.Fatalf("BreakFileHandleLeasesOnDelete returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1", got)
	}
	got := fake.breakHandleCalls[0]
	if got.HandleKey != string(fileHandle) {
		t.Errorf("handleKey = %q, want %q (file's own handle, not parent)", got.HandleKey, string(fileHandle))
	}
	if got.Reason != lock.BreakReasonSharingViolation {
		t.Errorf("reason = %d, want BreakReasonSharingViolation (strip H mask)", got.Reason)
	}
	if got.ExcludeOwner == nil || got.ExcludeOwner.ExcludeLeaseKey != setterKey {
		t.Errorf("ExcludeOwner = %+v, want ExcludeLeaseKey = %x", got.ExcludeOwner, setterKey)
	}

	// Fire-and-forget: matches the SET_INFO disposition contract — the holder
	// acks on its own transport. Inline waiting would deadlock on a
	// single-threaded test driver.
	if len(fake.waitForBreakCompletionKeys) != 0 {
		t.Errorf("WaitForBreakCompletion called %d times; want 0 (delete-disposition break is fire-and-forget)",
			len(fake.waitForBreakCompletionKeys))
	}
}

// TestBreakParentDirLeasesOnDestructiveCreate_SingleBreakWithDestructiveReason
// pins the #470 C4 contract: OVERWRITE/SUPERSEDE on a child must break the
// parent dir lease via ONE BreakLeasesOnOpenConflict call with
// BreakReasonDestructive (collapses strip-H + strip-R into break-to-None),
// then wait for the ack. This is the bug fix path — the legacy two-step
// (strip-H then strip-R) produced two notifications on a single RH dir lease
// and failed smb2.dirlease.overwrite which asserts exactly two break
// notifications across both the file and dir leases.
func TestBreakParentDirLeasesOnDestructiveCreate_SingleBreakWithDestructiveReason(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentHandle := lock.FileHandle("parent-dir-handle-destructive")
	if err := lm.BreakParentDirLeasesOnDestructiveCreate(
		context.Background(), parentHandle, "share1", "smb:A", [16]byte{}, false,
	); err != nil {
		t.Fatalf("BreakParentDirLeasesOnDestructiveCreate returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1 (a single destructive break collapses strip-H and strip-R)", got)
	}
	if got := fake.breakHandleCalls[0].Reason; got != lock.BreakReasonDestructive {
		t.Errorf("Reason = %d, want BreakReasonDestructive (Samba `will_overwrite` arm strips H|R atomically)", got)
	}
	if fake.breakHandleCalls[0].HandleKey != string(parentHandle) {
		t.Errorf("handleKey = %q, want %q", fake.breakHandleCalls[0].HandleKey, string(parentHandle))
	}

	// No second BreakReadLeasesForParentDir call — the test would otherwise
	// produce three break notifications when a single RH parent lease is in
	// play, breaking smb2.dirlease.overwrite (#470 C4).
	if got := len(fake.breakReadCalls); got != 0 {
		t.Errorf("BreakReadLeasesForParentDir call count = %d, want 0 (destructive path collapses both steps into one notification)", got)
	}

	if got := len(fake.waitForBreakCompletionKeys); got != 1 {
		t.Fatalf("WaitForBreakCompletion call count = %d, want 1 (CREATE must wait for ack per MS-SMB2 §3.3.4.7)", got)
	}
	if fake.waitForBreakCompletionKeys[0] != string(parentHandle) {
		t.Errorf("WaitForBreakCompletion handleKey = %q, want %q",
			fake.waitForBreakCompletionKeys[0], string(parentHandle))
	}

	// Order: break must precede wait.
	if len(fake.callOrder) < 2 {
		t.Fatalf("expected at least 2 calls in order, got %v", fake.callOrder)
	}
	if fake.callOrder[0] != "BreakLeasesOnOpenConflict" {
		t.Errorf("first call = %q, want BreakLeasesOnOpenConflict", fake.callOrder[0])
	}
	if fake.callOrder[1] != "WaitForBreakCompletion" {
		t.Errorf("second call = %q, want WaitForBreakCompletion", fake.callOrder[1])
	}
}

// TestBreakParentDirLeasesOnDestructiveCreate_PlumbsParentKeySuppression pins
// that the C2 parent-key-suppression args (excludeClientID + parent_key +
// hasExcludeKey) are forwarded into the LockOwner used for the break call,
// so the dir-lease parent-key rule (MS-SMB2 §3.3.4.20) keeps applying on the
// destructive path.
func TestBreakParentDirLeasesOnDestructiveCreate_PlumbsParentKeySuppression(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentKey := [16]byte{0xAB, 0xCD, 0xEF, 0x01}
	if err := lm.BreakParentDirLeasesOnDestructiveCreate(
		context.Background(), lock.FileHandle("parent"), "share1", "smb:client-B", parentKey, true,
	); err != nil {
		t.Fatalf("BreakParentDirLeasesOnDestructiveCreate returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.breakHandleCalls) != 1 {
		t.Fatalf("BreakLeasesOnOpenConflict call count = %d, want 1", len(fake.breakHandleCalls))
	}
	excludeOwner := fake.breakHandleCalls[0].ExcludeOwner
	if excludeOwner == nil {
		t.Fatal("excludeOwner must be non-nil when clientID + parent_key are provided")
	}
	if excludeOwner.ClientID != "smb:client-B" {
		t.Errorf("ClientID = %q, want %q", excludeOwner.ClientID, "smb:client-B")
	}
	if !excludeOwner.HasExcludeParentDirLeaseKey {
		t.Errorf("HasExcludeParentDirLeaseKey = false, want true")
	}
	if excludeOwner.ExcludeParentDirLeaseKey != parentKey {
		t.Errorf("ExcludeParentDirLeaseKey = %x, want %x", excludeOwner.ExcludeParentDirLeaseKey, parentKey)
	}
}
