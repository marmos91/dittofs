package lock

import (
	"context"
	"sync"
	"testing"
)

// dlease1Key is the DLEASE1 lease-key constant smbtorture reuses across the
// dirlease subtests (source4/torture/smb2/lease.c).
var dlease1Key = [16]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// TestPrepareBreakLeasesOnOpenConflictIgnoresLaterGrant pins the rule that a
// content change breaks the leases that existed when it happened, not the ones
// present when its notification is finally sent.
//
// The SMB CLOSE of a written file records a break-to-None on its parent
// directory's leases and defers the notification until after the CLOSE
// response, so a client that skips the ACK still sees the break. Each SMB2
// request runs on its own goroutine, so the client's next CREATE can be granted
// a fresh directory lease before that deferred notification runs. Re-reading
// the lease table at send time revoked that new lease, and the client counted
// two LEASE_BREAKs for one change.
func TestPrepareBreakLeasesOnOpenConflictIgnoresLaterGrant(t *testing.T) {
	lm := NewManager()
	const handleKey = "/share:test_unlink"

	var mu sync.Mutex
	var broken [][16]byte
	lm.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(_ string, l *UnifiedLock, _ uint32) {
			mu.Lock()
			broken = append(broken, l.Lease.LeaseKey)
			mu.Unlock()
		},
	})

	// The directory carries no lease when the change happens.
	send := lm.PrepareBreakLeasesOnOpenConflict(handleKey, &LockOwner{}, BreakReasonDestructive)

	// A later CREATE takes an RH directory lease on the same directory.
	if _, _, err := lm.RequestLease(context.Background(), FileHandle(handleKey), dlease1Key,
		[16]byte{}, "owner-1", "smb:1", "share",
		LeaseStateRead|LeaseStateHandle, true); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	send()

	mu.Lock()
	defer mu.Unlock()
	if len(broken) != 0 {
		t.Fatalf("deferred notification broke %d lease(s) (%x); a lease granted after the "+
			"change must not be broken by it", len(broken), broken)
	}
	if _, rec, _ := lm.findLeaseByKey(dlease1Key); rec != nil && rec.Lease.Breaking {
		t.Fatal("lease granted after the change was marked Breaking by it")
	}
}

// TestPrepareBreakLeasesOnOpenConflictBreaksExistingLease is the positive half:
// a lease held when the change happens is still broken once the notification is
// sent, and it is marked Breaking as soon as the change is recorded.
func TestPrepareBreakLeasesOnOpenConflictBreaksExistingLease(t *testing.T) {
	lm := NewManager()
	const handleKey = "/share:test_unlink"

	var mu sync.Mutex
	var broken [][16]byte
	lm.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(_ string, l *UnifiedLock, _ uint32) {
			mu.Lock()
			broken = append(broken, l.Lease.LeaseKey)
			mu.Unlock()
		},
	})

	if _, _, err := lm.RequestLease(context.Background(), FileHandle(handleKey), dlease1Key,
		[16]byte{}, "owner-1", "smb:1", "share",
		LeaseStateRead|LeaseStateHandle, true); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	send := lm.PrepareBreakLeasesOnOpenConflict(handleKey, &LockOwner{}, BreakReasonDestructive)

	if _, rec, _ := lm.findLeaseByKey(dlease1Key); rec == nil || !rec.Lease.Breaking {
		t.Fatal("lease held at change time must be marked Breaking when the change is recorded")
	}
	mu.Lock()
	sentEarly := len(broken)
	mu.Unlock()
	if sentEarly != 0 {
		t.Fatalf("notification sent before send(): %d", sentEarly)
	}

	send()

	mu.Lock()
	defer mu.Unlock()
	if len(broken) != 1 || broken[0] != dlease1Key {
		t.Fatalf("want one break on DLEASE1, got %x", broken)
	}
}
