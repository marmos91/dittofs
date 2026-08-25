package lease

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// shareResolver routes each share name to its own lock.Manager, mirroring the
// per-share LockManager layout the runtime builds.
type shareResolver struct {
	mgrs map[string]lock.LockManager
}

func (r *shareResolver) GetLockManagerForShare(shareName string) lock.LockManager {
	return r.mgrs[shareName]
}

const testRH = lock.LeaseStateRead | lock.LeaseStateHandle

// TestReleaseSessionLeases_OtherClientSameKeyKeepsItsLease asserts that a
// logoff releases only the leases the departing session actually holds. Two
// clients may present the same 16-byte lease key value on different files, so
// a per-key session mapping records only whichever of them registered last:
// the survivor's lease is torn down and the loser's is never released at all.
func TestReleaseSessionLeases_OtherClientSameKeyKeepsItsLease(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&shareResolver{mgrs: map[string]lock.LockManager{"share1": mgr}}, nil)
	ctx := context.Background()
	leaseKey := [16]byte{0x11, 0x22, 0x33}

	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-A"), leaseKey, [16]byte{},
		1, [16]byte{0xA1}, "smb:lease:A", "smb:1", "share1", testRH, false); err != nil {
		t.Fatalf("client A RequestLease: %v", err)
	}
	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-B"), leaseKey, [16]byte{},
		2, [16]byte{0xB2}, "smb:lease:B", "smb:2", "share1", testRH, false); err != nil {
		t.Fatalf("client B RequestLease: %v", err)
	}

	// Client B logs off. Only its lease on file-B may go.
	if err := lm.ReleaseSessionLeases(ctx, 2); err != nil {
		t.Fatalf("ReleaseSessionLeases(2): %v", err)
	}
	if !mgr.HasLeaseOnHandle("file-A", leaseKey) {
		t.Errorf("client A's lease on file-A was released by client B's logoff")
	}
	if mgr.HasLeaseOnHandle("file-B", leaseKey) {
		t.Errorf("client B's lease on file-B survived its own logoff")
	}

	// Client A logs off. Its lease must go now.
	if err := lm.ReleaseSessionLeases(ctx, 1); err != nil {
		t.Fatalf("ReleaseSessionLeases(1): %v", err)
	}
	if mgr.HasLeaseOnHandle("file-A", leaseKey) {
		t.Errorf("client A's lease on file-A survived its own logoff")
	}
}

// TestGetLeaseState_OtherShareSameKeyAnswersItsOwn asserts that a lease-state
// read answers about the share it was asked about. The same 16-byte key value
// can be live in two shares at once — each share has its own lock manager, so
// the cross-file uniqueness rule does not span them. The state and epoch read
// here go back out on the wire: they are what a durable disconnect persists
// and what a replayed CREATE rewrites its RqLs response context with.
func TestGetLeaseState_OtherShareSameKeyAnswersItsOwn(t *testing.T) {
	t.Parallel()

	mgr1, mgr2 := lock.NewManager(), lock.NewManager()
	lm := NewLeaseManager(&shareResolver{mgrs: map[string]lock.LockManager{
		"share1": mgr1,
		"share2": mgr2,
	}}, nil)
	ctx := context.Background()
	leaseKey := [16]byte{0x44, 0x55}

	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-A"), leaseKey, [16]byte{},
		1, [16]byte{0xA1}, "smb:lease:A", "smb:1", "share1", testRH, false); err != nil {
		t.Fatalf("client A RequestLease: %v", err)
	}
	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-B"), leaseKey, [16]byte{},
		2, [16]byte{0xB2}, "smb:lease:B", "smb:2", "share2", lock.LeaseStateRead, false); err != nil {
		t.Fatalf("client B RequestLease: %v", err)
	}

	if state, _, found := lm.GetLeaseState(ctx, "share1", leaseKey); !found || state != testRH {
		t.Errorf("GetLeaseState(share1) = 0x%x found=%v, want RH (0x%x) — answered from the other share's lease",
			state, found, uint32(testRH))
	}
	if state, _, found := lm.GetLeaseState(ctx, "share2", leaseKey); !found || state != lock.LeaseStateRead {
		t.Errorf("GetLeaseState(share2) = 0x%x found=%v, want R (0x%x)",
			state, found, uint32(lock.LeaseStateRead))
	}
}

// TestAcknowledgeLeaseBreak_ResolvesTheAckingClientsLease asserts that a
// LEASE_BREAK_ACK is applied to the acking client's own lease. The wire gives
// only a lease key, so the acking connection supplies the rest of the identity;
// resolving the share from the key alone sends the ack to whichever client
// registered the key value last, which both fails the acking client's request
// and leaves its lease stuck in Breaking until the server force-downgrades it.
func TestAcknowledgeLeaseBreak_ResolvesTheAckingClientsLease(t *testing.T) {
	t.Parallel()

	mgr1, mgr2 := lock.NewManager(), lock.NewManager()
	lm := NewLeaseManager(&shareResolver{mgrs: map[string]lock.LockManager{
		"share1": mgr1,
		"share2": mgr2,
	}}, nil)
	ctx := context.Background()
	leaseKey := [16]byte{0x66, 0x77}
	guidA := [16]byte{0xA1}

	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-A"), leaseKey, [16]byte{},
		1, guidA, "smb:lease:A", "smb:1", "share1", testRH, false); err != nil {
		t.Fatalf("client A RequestLease: %v", err)
	}
	// Client B takes the same key value in the other share, after A. On a
	// per-key share map this is the entry A's ack would resolve through.
	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-B"), leaseKey, [16]byte{},
		2, [16]byte{0xB2}, "smb:lease:B", "smb:2", "share2", lock.LeaseStateRead, false); err != nil {
		t.Fatalf("client B RequestLease: %v", err)
	}

	// A conflicting open breaks client A's lease.
	if err := mgr1.BreakLeasesOnOpenConflict("file-A", &lock.LockOwner{ClientID: "smb:9"}, lock.BreakReasonDestructive); err != nil {
		t.Fatalf("break A's lease: %v", err)
	}
	if !mgr1.HasOtherBreakingLeases("file-A", [16]byte{}) {
		t.Fatal("client A's lease is not breaking after the conflicting open")
	}

	if err := lm.AcknowledgeLeaseBreak(ctx, leaseKey, 1, guidA, lock.LeaseStateNone, 0); err != nil {
		t.Fatalf("client A's ack rejected: %v", err)
	}
	if mgr1.HasOtherBreakingLeases("file-A", [16]byte{}) {
		t.Errorf("client A's lease is still breaking after its own ack — the ack was routed to another client's lease")
	}
	if state, _, _ := mgr2.GetLeaseState(ctx, leaseKey); state != lock.LeaseStateRead {
		t.Errorf("client B's lease state = 0x%x, want R (0x%x) — A's ack downgraded B's lease",
			state, uint32(lock.LeaseStateRead))
	}
}

// TestVerifyLeaseAckOwnership_RejectsForeignClientSameKey asserts the ack
// ownership gate is decided by the binding the ack will actually act on. A
// session holding the same key value on another file must not be able to
// acknowledge — and thereby downgrade — a lease it does not own.
func TestVerifyLeaseAckOwnership_RejectsForeignClientSameKey(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&shareResolver{mgrs: map[string]lock.LockManager{"share1": mgr}}, nil)
	ctx := context.Background()
	leaseKey := [16]byte{0x88}
	guidA, guidB := [16]byte{0xA1}, [16]byte{0xB2}

	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-A"), leaseKey, [16]byte{},
		1, guidA, "smb:lease:A", "smb:1", "share1", testRH, false); err != nil {
		t.Fatalf("client A RequestLease: %v", err)
	}
	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-B"), leaseKey, [16]byte{},
		2, guidB, "smb:lease:B", "smb:2", "share1", testRH, false); err != nil {
		t.Fatalf("client B RequestLease: %v", err)
	}

	if !lm.VerifyLeaseAckOwnership(leaseKey, 1, guidA) {
		t.Error("client A's ack for its own lease was rejected")
	}
	if !lm.VerifyLeaseAckOwnership(leaseKey, 2, guidB) {
		t.Error("client B's ack for its own lease was rejected")
	}
	if lm.VerifyLeaseAckOwnership(leaseKey, 3, [16]byte{0xCC}) {
		t.Error("a third client's ack for a key it never bound was accepted")
	}
}

// TestGetSessionForBreak_ZeroGUIDSameKeyRoutesToOwner covers the break-routing
// fallback taken when no ClientGUID was recorded (callers without a
// CryptoState: older durable-reconnect paths and tests). The ClientGUID path
// above it is already client-scoped, so this fallback is where a per-key
// session mapping mis-routes: the break for one client's lease is delivered on
// whichever session registered the key value last.
func TestGetSessionForBreak_ZeroGUIDSameKeyRoutesToOwner(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&shareResolver{mgrs: map[string]lock.LockManager{"share1": mgr}}, nil)
	ctx := context.Background()
	leaseKey := [16]byte{0x99}

	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-A"), leaseKey, [16]byte{},
		1, [16]byte{}, "smb:lease:A", "smb:1", "share1", testRH, false); err != nil {
		t.Fatalf("client A RequestLease: %v", err)
	}
	if _, _, err := lm.RequestLease(ctx, lock.FileHandle("file-B"), leaseKey, [16]byte{},
		2, [16]byte{}, "smb:lease:B", "smb:2", "share1", testRH, false); err != nil {
		t.Fatalf("client B RequestLease: %v", err)
	}

	if sid, ok := lm.GetSessionForBreak("smb:1", "share1", leaseKey); !ok || sid != 1 {
		t.Errorf("break for client A's lease routes to session %d (ok=%v), want 1", sid, ok)
	}
	if sid, ok := lm.GetSessionForBreak("smb:2", "share1", leaseKey); !ok || sid != 2 {
		t.Errorf("break for client B's lease routes to session %d (ok=%v), want 2", sid, ok)
	}
}

// TestResolveAckBinding_SameSessionTwoSharesIsDeterministic pins the tie-break
// for the case MS-SMB2 does not contemplate: one session holding tree connects
// to two shares and presenting the same lease key value in both. Map iteration
// order is randomized, so resolving to whichever candidate came out of the map
// first would send an ack to an arbitrary one of the two — leaving the other
// lease Breaking until it times out, and downgrading a lease nobody acked.
func TestResolveAckBinding_SameSessionTwoSharesIsDeterministic(t *testing.T) {
	t.Parallel()

	lm := NewLeaseManager(nil, nil)
	leaseKey := [16]byte{0xAB, 0xCD}
	lm.bindings[leaseClientKey{ClientID: "smb:1", Share: "share-b", Key: leaseKey}] =
		leaseBinding{SessionID: 1, HandleKey: "file-b"}
	lm.bindings[leaseClientKey{ClientID: "smb:1", Share: "share-a", Key: leaseKey}] =
		leaseBinding{SessionID: 1, HandleKey: "file-a"}

	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for i := 0; i < 200; i++ {
		ck, found := lm.resolveAckBindingLocked(leaseKey, 1, [16]byte{})
		if !found {
			t.Fatalf("iteration %d: no binding resolved", i)
		}
		if ck.Share != "share-a" {
			t.Fatalf("iteration %d: ack resolved to share %q, want the deterministic pick %q",
				i, ck.Share, "share-a")
		}
	}
}
