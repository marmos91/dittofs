package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// The lease these tests run under is short enough to lapse inside the test and
// long enough that nothing lapses while the setup is still running.
const courtesyLease = 250 * time.Millisecond

// courtesyClient registers a v4.1 client, confirms it with a session, and
// returns its client ID. A v4.1 client's state outlives its lease until a
// sweeper collects it, which is the courtesy window these tests exercise: the
// session reaper is never started here, so nothing but a conflicting request
// can release the state.
func courtesyClient(t *testing.T, sm *StateManager, ownerID string) uint64 {
	t.Helper()

	var verifier [8]byte
	copy(verifier[:], "verify01")

	res, err := sm.ExchangeID([]byte(ownerID), verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID(%s): %v", ownerID, err)
	}
	if _, _, err := sm.CreateSession(res.ClientID, res.SequenceID, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil); err != nil {
		t.Fatalf("CreateSession(%s): %v", ownerID, err)
	}
	return res.ClientID
}

// waitLeaseLapsed blocks until clientID's lease has run out, polling the same
// predicate the production paths branch on rather than sleeping for a guessed
// interval. A stalled runner makes it wait longer, never makes it proceed
// early.
func waitLeaseLapsed(t *testing.T, sm *StateManager, clientID uint64) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		sm.mu.Lock()
		lapsed := sm.clientLeaseLapsedLocked(clientID)
		sm.mu.Unlock()
		if lapsed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client %d lease still live after 30s (lease is %s)", clientID, courtesyLease)
		}
		time.Sleep(courtesyLease / 10)
	}
}

// renewLease puts a client's lease back to full. Tests that need one client to
// stay live while another lapses call this immediately before asserting: the
// live client's lease is the same short one, so any stall between establishing
// it and the assertion would otherwise lapse it too and the test would fail
// for a reason it is not about.
func renewLease(t *testing.T, sm *StateManager, clientID uint64) {
	t.Helper()

	sm.mu.Lock()
	defer sm.mu.Unlock()
	record := sm.v41ClientLocked(clientID)
	if record == nil || record.Lease == nil {
		t.Fatalf("no v4.1 lease for client %d", clientID)
	}
	record.Lease.Renew()
}

// TestOpen_ConflictExpiresLapsedShareReservation covers a courtesy client's
// share reservation meeting a conflicting OPEN. Holding the reservation past
// the lease is what makes the server courteous; enforcing it against another
// client once the lease has run out is not courtesy but a refusal on behalf of
// a client that is gone.
//
// Nothing here waits for a sweeper, which is the point: with the release
// driven by the sweep instead of by the conflict, the same OPEN succeeds or
// fails depending on where in the sweep interval it lands.
func TestOpen_ConflictExpiresLapsedShareReservation(t *testing.T) {
	sm := NewStateManager(courtesyLease)
	defer sm.Shutdown()

	fh := []byte("courtesy-share-fh")
	holder := courtesyClient(t, sm, "courtesy-holder")

	if _, err := sm.OpenFile(holder, []byte("holder-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_READ, types.CLAIM_NULL); err != nil {
		t.Fatalf("holder OpenFile: %v", err)
	}

	// While the lease is live the reservation is enforced.
	newcomer := courtesyClient(t, sm, "courtesy-newcomer")
	if _, err := sm.OpenFile(newcomer, []byte("newcomer-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); !errors.Is(err, ErrShareDenied) {
		t.Fatalf("OPEN against a live share reservation: got %v, want ErrShareDenied", err)
	}

	waitLeaseLapsed(t, sm, holder)

	if _, err := sm.OpenFile(newcomer, []byte("newcomer-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); err != nil {
		t.Errorf("OPEN against a lapsed client's share reservation: got %v, want success", err)
	}
}

// TestLock_ConflictExpiresLapsedByteRangeLock is the same rule for byte-range
// locks, which are held in the cross-protocol lock manager rather than in the
// share-reservation index, and so are released by a different path.
func TestLock_ConflictExpiresLapsedByteRangeLock(t *testing.T) {
	sm := NewStateManager(courtesyLease)
	defer sm.Shutdown()
	sm.SetLockManager(lock.NewManager())

	fh := []byte("courtesy-lock-fh")

	holder := courtesyClient(t, sm, "lock-holder")
	holderOpen, err := sm.OpenFile(holder, []byte("holder-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("holder OpenFile: %v", err)
	}
	if _, err := sm.LockNew(context.Background(), holder, []byte("holder-lock-owner"), 0, &holderOpen.Stateid, 0, fh, types.WRITE_LT, 0, ^uint64(0), false, holder); err != nil {
		t.Fatalf("holder LockNew: %v", err)
	}

	newcomer := courtesyClient(t, sm, "lock-newcomer")
	newcomerOpen, err := sm.OpenFile(newcomer, []byte("newcomer-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("newcomer OpenFile: %v", err)
	}

	// While the lease is live the lock is enforced.
	res, err := sm.LockNew(context.Background(), newcomer, []byte("newcomer-lock-owner"), 0, &newcomerOpen.Stateid, 0, fh, types.WRITE_LT, 0, ^uint64(0), false, newcomer)
	if err != nil {
		t.Fatalf("newcomer LockNew against a live lock: %v", err)
	}
	if res.Denied == nil {
		t.Fatal("LOCK against a live conflicting lock: got success, want LOCK4denied")
	}

	waitLeaseLapsed(t, sm, holder)

	res, err = sm.LockNew(context.Background(), newcomer, []byte("newcomer-lock-owner-2"), 0, &newcomerOpen.Stateid, 0, fh, types.WRITE_LT, 0, ^uint64(0), false, newcomer)
	if err != nil {
		t.Fatalf("newcomer LockNew after the holder's lease lapsed: %v", err)
	}
	if res.Denied != nil {
		t.Errorf("LOCK against a lapsed client's lock: got LOCK4denied, want success")
	}
}

// A conflict must not reach past the lapsed clients: a live client's
// reservation stays enforced even when a dead one is released alongside it.
func TestOpen_ConflictKeepsLiveShareReservation(t *testing.T) {
	sm := NewStateManager(courtesyLease)
	defer sm.Shutdown()

	fh := []byte("courtesy-mixed-fh")
	lapsed := courtesyClient(t, sm, "mixed-lapsed")

	if _, err := sm.OpenFile(lapsed, []byte("lapsed-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_READ, types.CLAIM_NULL); err != nil {
		t.Fatalf("lapsed OpenFile: %v", err)
	}
	waitLeaseLapsed(t, sm, lapsed)

	// Established after the first client's lease ran out, so this one is live.
	live := courtesyClient(t, sm, "mixed-live")
	if _, err := sm.OpenFile(live, []byte("live-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_READ, types.CLAIM_NULL); err != nil {
		t.Fatalf("live OpenFile: %v", err)
	}

	newcomer := courtesyClient(t, sm, "mixed-newcomer")
	renewLease(t, sm, live)
	if _, err := sm.OpenFile(newcomer, []byte("newcomer-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); !errors.Is(err, ErrShareDenied) {
		t.Errorf("OPEN against a live reservation held alongside a lapsed one: got %v, want ErrShareDenied", err)
	}
}

// TestExpireClient_ReleasesLocksHeldOnItsOwnOpen covers the release sweep
// reaching the cross-protocol lock manager: a lock left there after its
// owner's client is released has nothing left that could unlock it.
func TestExpireClient_ReleasesLocksHeldOnItsOwnOpen(t *testing.T) {
	sm := NewStateManager(courtesyLease)
	defer sm.Shutdown()
	lm := lock.NewManager()
	sm.SetLockManager(lm)

	fh := []byte("cross-client-lock-fh")
	holder := courtesyClient(t, sm, "cross-holder")
	holderOpen, err := sm.OpenFile(holder, []byte("holder-owner"), 0, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("holder OpenFile: %v", err)
	}
	if _, err := sm.LockNew(context.Background(), holder, []byte("holder-lock-owner"), 0, &holderOpen.Stateid, 0, fh, types.WRITE_LT, 0, ^uint64(0), false, holder); err != nil {
		t.Fatalf("holder LockNew: %v", err)
	}

	if got := len(lm.ListUnifiedLocks(string(fh))); got != 1 {
		t.Fatalf("locks held before expiry = %d, want 1", got)
	}

	sm.mu.Lock()
	sm.releaseClientStateLocked(holder)
	sm.mu.Unlock()

	if got := len(lm.ListUnifiedLocks(string(fh))); got != 0 {
		t.Errorf("locks still held after the lock-owner's client was released = %d, want 0", got)
	}
}
