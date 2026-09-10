package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestConfirmClientID_RebootReleasesPreviousIncarnationState covers the purge
// RFC 7530 Section 9.1.1 attaches to the confirm that replaces a confirmed
// record: the previous incarnation's opens, locks and delegations go with the
// record, and a stateid it held answers NFS4ERR_EXPIRED rather than still
// working.
//
// Forgetting only the record leaves every file it opened share-reserved and
// byte-range locked on behalf of an incarnation that will never close them.
func TestConfirmClientID_RebootReleasesPreviousIncarnationState(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	cb := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	fh := []byte("reboot-fh")

	first, err := sm.SetClientID("rebooter", [8]byte{1}, cb, "10.0.0.1:1", "uid:0")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(first.ClientID, first.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	open, err := sm.OpenFile(first.ClientID, []byte("owner"), 1, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_BOTH, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	stale := open.Stateid

	// Same id string, different verifier: a reboot. Confirming it replaces the
	// record established above.
	second, err := sm.SetClientID("rebooter", [8]byte{2}, cb, "10.0.0.1:1", "uid:0")
	if err != nil {
		t.Fatalf("SetClientID after reboot: %v", err)
	}
	if err := sm.ConfirmClientID(second.ClientID, second.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID after reboot: %v", err)
	}

	if _, err := sm.CloseFile(&stale, 2, 0); !errors.Is(err, ErrExpired) {
		t.Errorf("CLOSE with the pre-reboot stateid: got %v, want ErrExpired", err)
	}

	// The share reservation went with it, so the file is openable again.
	if _, err := sm.OpenFile(second.ClientID, []byte("owner2"), 1, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); err != nil {
		t.Errorf("OPEN after the reboot purge: got %v, want success", err)
	}
}

// TestRenewLease_ForgottenClientIDIsExpiredNotStale pins the distinction RFC
// 7530 Section 16.28.5 draws between the two ways a client ID goes unmatched.
// An id this boot minted and has since released is NFS4ERR_EXPIRED, which
// tells the client its lease lapsed; answering NFS4ERR_STALE_CLIENTID instead
// tells it the server rebooted, which is a different and much larger recovery.
func TestRenewLease_ForgottenClientIDIsExpiredNotStale(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	res, err := sm.SetClientID("renewer", [8]byte{1}, CallbackInfo{}, "10.0.0.1:1", "uid:0")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Drop the record the way a lapsed lease does.
	sm.RemoveClient(res.ClientID)

	if err := sm.RenewLease(res.ClientID, "uid:0"); !errors.Is(err, ErrExpired) {
		t.Errorf("RENEW for a released client ID from this boot: got %v, want ErrExpired", err)
	}
	if err := sm.ValidateAndRenewClient(res.ClientID); !errors.Is(err, ErrExpired) {
		t.Errorf("ValidateAndRenewClient for a released client ID from this boot: got %v, want ErrExpired", err)
	}

	// An id no boot of this server could have issued stays STALE_CLIENTID:
	// the client must re-run SETCLIENTID rather than reclaim.
	otherBoot := uint64(sm.BootEpoch()+1)<<32 | 1
	if err := sm.RenewLease(otherBoot, "uid:0"); !errors.Is(err, ErrStaleClientID) {
		t.Errorf("RENEW for a client ID from another boot: got %v, want ErrStaleClientID", err)
	}
	if err := sm.RenewLease(0, "uid:0"); !errors.Is(err, ErrStaleClientID) {
		t.Errorf("RENEW for client ID 0: got %v, want ErrStaleClientID", err)
	}
}

// TestSetClientID_LockOnlyStateStillBlocksAnotherPrincipal covers the gap
// between "holds an open" and "holds state". LOCK takes the lock-owner's
// client ID from the wire and does not require it to match the client that
// owns the open the lock hangs from, so a client can hold a live byte-range
// lock while owning no open state of its own. That is still leased state a
// SETCLIENTID from another principal would cancel.
func TestSetClientID_LockOnlyStateStillBlocksAnotherPrincipal(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()
	sm.SetLockManager(lock.NewManager())

	cb := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	fh := []byte("lock-only-fh")

	// The client that owns the open, under one principal.
	opener, err := sm.SetClientID("lock-only-opener", [8]byte{1}, cb, "10.0.0.1:1", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID(opener): %v", err)
	}
	if err := sm.ConfirmClientID(opener.ClientID, opener.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID(opener): %v", err)
	}
	open, err := sm.OpenFile(opener.ClientID, []byte("open-owner"), 1, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// A second client, holding only the lock.
	locker, err := sm.SetClientID("lock-only-locker", [8]byte{2}, cb, "10.0.0.2:2", "uid:2000")
	if err != nil {
		t.Fatalf("SetClientID(locker): %v", err)
	}
	if err := sm.ConfirmClientID(locker.ClientID, locker.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID(locker): %v", err)
	}
	if _, err := sm.LockNew(context.Background(), locker.ClientID, []byte("lock-owner"), 1, &open.Stateid, 2, fh, types.WRITE_LT, 0, 100, false, 0); err != nil {
		t.Fatalf("LockNew: %v", err)
	}

	if _, err := sm.SetClientID("lock-only-locker", [8]byte{2}, cb, "10.0.0.3:3", "uid:9999"); !errors.Is(err, ErrClientIDInUse) {
		t.Errorf("SETCLIENTID from another principal against a lock-holding client ID: got %v, want ErrClientIDInUse", err)
	}
}
