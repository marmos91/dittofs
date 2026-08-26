package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestLeaseAck_DoesNotDowngradeAnotherClientsLease drives two V2 lease clients
// through the real grant, break and acknowledge path.
//
// Two clients may present the same 16-byte lease key value on different files
// of one share: the cross-file uniqueness rule is per (client, file), so the
// value alone is not an identity. A LEASE_BREAK_ACK is one client's statement
// about its own lease. It must not resolve, complete, or rewrite the state of a
// lease another client holds under the same key value on another file.
func TestLeaseAck_DoesNotDowngradeAnotherClientsLease(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	notifier := &capturingNotifier{}
	leaseMgr := lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, notifier)
	mgr.RegisterBreakCallbacks(lease.NewSMBBreakHandler(leaseMgr, notifier))

	ctx := context.Background()
	const shareName = "share1"
	leaseKey := [16]byte{0xC0, 0xFF, 0xEE}
	const rwh = lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
	guidA := [16]byte{0xA1}
	guidB := [16]byte{0xB2}

	respA, err := ProcessLeaseCreateContext(ctx, leaseMgr, encodeV2LeaseContext(leaseKey, rwh, 0x0100),
		lock.FileHandle("file-A"), 1, guidA, "smb:1", shareName, false, false, false)
	if err != nil || respA.LeaseState != rwh {
		t.Fatalf("client A CREATE: state=0x%x err=%v", respA.LeaseState, err)
	}
	respB, err := ProcessLeaseCreateContext(ctx, leaseMgr, encodeV2LeaseContext(leaseKey, rwh, 0x0200),
		lock.FileHandle("file-B"), 2, guidB, "smb:2", shareName, false, false, false)
	if err != nil || respB.LeaseState != rwh {
		t.Fatalf("client B CREATE: state=0x%x err=%v", respB.LeaseState, err)
	}

	// Break B's lease and leave it unacknowledged.
	if err := mgr.BreakLeasesOnOpenConflict("file-B", &lock.LockOwner{ClientID: "smb:9"}, lock.BreakReasonDestructive); err != nil {
		t.Fatalf("break client B's lease: %v", err)
	}
	stateBAfterBreak, _, _ := leaseMgr.GetLeaseState(ctx, lock.FileHandle("file-B"), shareName, leaseKey)

	// Break A's lease, then let A acknowledge it.
	if err := mgr.BreakLeasesOnOpenConflict("file-A", &lock.LockOwner{ClientID: "smb:9"}, lock.BreakReasonDestructive); err != nil {
		t.Fatalf("break client A's lease: %v", err)
	}
	ackState := uint32(lock.LeaseStateNone)
	if err := leaseMgr.AcknowledgeLeaseBreak(ctx, leaseKey, 1, guidA, ackState, 0); err != nil {
		t.Fatalf("client A ack: %v", err)
	}

	stateB, _, foundB := leaseMgr.GetLeaseState(ctx, lock.FileHandle("file-B"), shareName, leaseKey)
	if !foundB {
		t.Fatalf("client B's lease record disappeared after client A acknowledged its own break")
	}
	if stateB != stateBAfterBreak {
		t.Errorf("client B lease state = 0x%x, want 0x%x (unchanged) — client A's LEASE_BREAK_ACK rewrote a lease B holds on another file under the same key value",
			stateB, stateBAfterBreak)
	}
}
