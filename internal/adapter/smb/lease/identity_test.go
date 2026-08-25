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
