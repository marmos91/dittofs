package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// The I/O operations resolve a stateid through ValidateStateid, which compares
// the owning client. The state-changing operations resolve it themselves, and
// these tests pin that they compare too.
//
// Each test asserts two halves of the rule: another client is refused
// NFS4ERR_BAD_STATEID, and the owning client is still served. The third half —
// a caller with no trusted client identity, every NFSv4.0 request, which
// carries no clientid4 and has no session to derive one from, is served as
// before — is what the rest of this package's tests pass 0 for; it is asserted
// explicitly here only for the operations that have no other coverage of it.
//
// The refusals also double as a check that NFS4ERR_BAD_STATEID leaves the
// owner's sequence untouched (RFC 7530 Section 9.1.7): every "owning client"
// leg reuses the seqid the refused call was made with.

const (
	authzClientA uint64 = 0xA11CE
	authzClientB uint64 = 0xB0B
)

// newConfirmedOpen opens and confirms a file for clientID, returning the
// confirmed open stateid and the seqid the next operation on that owner must
// use.
func newConfirmedOpen(t *testing.T, sm *StateManager, clientID uint64, fh []byte) (types.Stateid4, uint32) {
	t.Helper()
	openResult, err := sm.OpenFile(clientID, []byte("owner-a"), 1, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	confirmed, err := sm.ConfirmOpen(&openResult.Stateid, 2, clientID)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}
	return confirmed.Stateid, 3
}

func TestOpenConfirm_CrossClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	openResult, err := sm.OpenFile(authzClientA, []byte("owner-a"), 1, []byte("fh-confirm"),
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	sid := openResult.Stateid

	if _, err := sm.ConfirmOpen(&sid, 2, authzClientB); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B confirming client A's open: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
	if _, err := sm.ConfirmOpen(&sid, 2, authzClientA); err != nil {
		t.Errorf("owning client rejected: %v", err)
	}
}

func TestOpenConfirmV41_CrossClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	openResult, err := sm.OpenFile(authzClientA, []byte("owner-a"), 1, []byte("fh-confirm-v41"),
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	sid := openResult.Stateid

	if err := sm.ConfirmOpenV41(&sid, authzClientB); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B auto-confirming client A's open: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
	if err := sm.ConfirmOpenV41(&sid, authzClientA); err != nil {
		t.Errorf("owning client rejected: %v", err)
	}
	if err := sm.ConfirmOpenV41(&sid, 0); err != nil {
		t.Errorf("NFSv4.0 caller (no client identity) rejected: %v", err)
	}
}

func TestOpenDowngrade_CrossClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	sid, seqid := newConfirmedOpen(t, sm, authzClientA, []byte("fh-downgrade"))

	// The open behind these stateids was opened for BOTH and nothing else, so
	// BOTH is the only share_access an OPEN_DOWNGRADE on it may name: RFC 7530
	// Section 16.19.4 admits only a mode some OPEN actually asked for.
	if _, err := sm.DowngradeOpen(&sid, seqid, types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE, authzClientB); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B downgrading client A's open: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
	if _, err := sm.DowngradeOpen(&sid, seqid, types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE, authzClientA); err != nil {
		t.Errorf("owning client rejected: %v", err)
	}
}

func TestClose_CrossClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	sid, seqid := newConfirmedOpen(t, sm, authzClientA, []byte("fh-close"))

	if _, err := sm.CloseFile(&sid, seqid, authzClientB); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B closing client A's open: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
	if _, err := sm.CloseFile(&sid, seqid, authzClientA); err != nil {
		t.Errorf("owning client rejected: %v", err)
	}

	// The open is gone now, so a repeat CLOSE takes the already-closed branch,
	// which replays the cached reply. That reply is the owner's, so another
	// client must not be handed it either.
	//
	// The reply itself is cached by the handler layer, which is not in play in a
	// StateManager test, so it is seeded here to reach the branch at all.
	sm.mu.Lock()
	closed := sm.closedOwnerByOther[sid.Other]
	sm.mu.Unlock()
	if closed == nil {
		t.Fatal("CLOSE left no closed-owner entry to replay from")
	}
	closed.LastResult = &CachedResult{
		Status: types.NFS4_OK,
		Data:   encodeStatusReply(types.NFS4_OK),
	}

	if _, err := sm.CloseFile(&sid, seqid, authzClientB); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B replaying client A's CLOSE reply: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
	if _, err := sm.CloseFile(&sid, seqid, authzClientA); !isReplay(err) {
		t.Errorf("owning client denied its own CLOSE replay: %v", err)
	}
}

func isReplay(err error) bool {
	var replay *ReplayError
	return errors.As(err, &replay)
}

// TestLock_CrossClient covers LOCK on both paths and LOCKU. The open_to_lock
// path is the sharpest case of all of these: without the comparison a client
// can take a byte-range lock through another client's open, because LockNew
// resolves the open stateid but keys the lock-owner by the client ID it was
// handed.
func TestLock_CrossClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lock.NewManager())
	defer sm.Shutdown()

	fh := []byte("fh-locknew-cross")
	sid, seqid := newConfirmedOpen(t, sm, authzClientA, fh)

	_, err := sm.LockNew(context.Background(),
		authzClientB, []byte("owner-b"), 1, &sid, seqid,
		fh, types.WRITE_LT, 0, 100, false, authzClientB)
	if !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B locking through client A's open: err = %v, want NFS4ERR_BAD_STATEID", err)
	}

	res, err := sm.LockNew(context.Background(),
		authzClientA, []byte("owner-a"), 1, &sid, seqid,
		fh, types.WRITE_LT, 0, 100, false, authzClientA)
	if err != nil || res.Denied != nil {
		t.Fatalf("owning client rejected: err = %v denied = %v", err, res)
	}

	if _, err := sm.LockExisting(context.Background(), &res.Stateid, 2,
		fh, types.WRITE_LT, 200, 100, false, authzClientB); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B extending client A's lock: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
	if _, err := sm.UnlockFile(&res.Stateid, 2, types.WRITE_LT, 0, 100,
		authzClientB); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Errorf("client B unlocking client A's lock: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
	if _, err := sm.UnlockFile(&res.Stateid, 2, types.WRITE_LT, 0, 100, authzClientA); err != nil {
		t.Errorf("owning client rejected on LOCKU: %v", err)
	}
}

// TestTestStateids_CrossClient pins that TEST_STATEID is not an existence
// oracle for another client's state, in all three stateid families.
func TestTestStateids_CrossClient(t *testing.T) {
	probe := func(t *testing.T, sm *StateManager, sid types.Stateid4) {
		t.Helper()
		if got := sm.TestStateids([]types.Stateid4{sid}, authzClientB); got[0] != types.NFS4ERR_BAD_STATEID {
			t.Errorf("client B probing client A's stateid: got %d, want NFS4ERR_BAD_STATEID", got[0])
		}
		if got := sm.TestStateids([]types.Stateid4{sid}, authzClientA); got[0] != types.NFS4_OK {
			t.Errorf("owning client rejected: got %d", got[0])
		}
		if got := sm.TestStateids([]types.Stateid4{sid}, 0); got[0] != types.NFS4_OK {
			t.Errorf("NFSv4.0 caller (no client identity) rejected: got %d", got[0])
		}
	}

	t.Run("open stateid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()
		sid, _ := newConfirmedOpen(t, sm, authzClientA, []byte("fh-test-open"))
		probe(t, sm, sid)
	})

	t.Run("lock stateid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		sm.SetLockManager(lock.NewManager())
		defer sm.Shutdown()
		probe(t, sm, newLockedFile(t, sm, authzClientA, []byte("fh-test-lock")))
	})

	t.Run("delegation stateid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()
		deleg := sm.GrantDelegation(authzClientA, []byte("fh-test-deleg"), types.OPEN_DELEGATE_READ)
		if deleg == nil {
			t.Fatal("GrantDelegation returned nil")
		}
		probe(t, sm, deleg.Stateid)
	})
}
