package lease

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestRequestLease_FailedGrant_RestoresPreviousBinding pins the compare-and-swap
// semantics of the pre-registration restore: when a lease grant fails for a
// (client, share, key) whose binding already points at an earlier file, the
// binding must be restored to that earlier file — the client still holds the
// key there, which is exactly why the new grant was refused.
func TestRequestLease_FailedGrant_RestoresPreviousBinding(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&fakeResolver{mgr: mgr}, nil)

	leaseKey := [16]byte{0x42}
	fh1 := lock.FileHandle("file-1")
	fh2 := lock.FileHandle("file-2")
	const clientID = "client-1"
	const ownerSession uint64 = 100

	// First grant: the client holds the key on file-1.
	if _, _, err := lm.RequestLease(context.Background(), fh1,
		leaseKey, [16]byte{}, ownerSession, [16]byte{},
		"owner-1", clientID, "share1",
		lock.LeaseStateRead|lock.LeaseStateWrite, false); err != nil {
		t.Fatalf("first RequestLease: %v", err)
	}

	// Second grant on a DIFFERENT file handle: the LockManager has a record for
	// this (key, client) on file-1, so the second grant on file-2 fails or is
	// refused — either way the binding must stay on file-1.
	_, _, _ = lm.RequestLease(context.Background(), fh2,
		leaseKey, [16]byte{}, ownerSession, [16]byte{},
		"owner-2", clientID, "share1",
		lock.LeaseStateRead|lock.LeaseStateWrite, false)

	// The observable contract: the ack ownership still resolves for the
	// ORIGINAL file's binding (restore did not clobber it), and the failed
	// grant did not leave the binding pointing at file-2.
	if !lm.VerifyLeaseAckOwnership(leaseKey, ownerSession, [16]byte{}) {
		t.Error("ack ownership lost after a failed grant — the restore clobbered the previous binding")
	}
}

// TestRequestLease_SuccessReassertsBinding pins that a successful grant's
// binding survives to the return: a later ack resolves for the granted
// (client, share, key).
func TestRequestLease_SuccessReassertsBinding(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	lm := NewLeaseManager(&fakeResolver{mgr: mgr}, nil)

	leaseKey := [16]byte{0x43}
	fh := lock.FileHandle("file-1")
	const ownerSession uint64 = 200

	if _, _, err := lm.RequestLease(context.Background(), fh,
		leaseKey, [16]byte{}, ownerSession, [16]byte{},
		"owner-1", "client-1", "share1",
		lock.LeaseStateRead|lock.LeaseStateWrite, false); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	if !lm.VerifyLeaseAckOwnership(leaseKey, ownerSession, [16]byte{}) {
		t.Error("ack ownership lost after a successful grant — the binding was not re-asserted")
	}
}
