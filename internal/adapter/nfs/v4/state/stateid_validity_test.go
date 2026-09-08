package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// The operations that resolve an open state themselves -- CLOSE, OPEN_CONFIRM,
// OPEN_DOWNGRADE and the open_to_lock_owner4 path of LOCK -- hold sm.mu for
// writing, so they cannot reuse ValidateStateid. These pin the checks they
// perform instead: a stateid from another server incarnation is stale, a
// stateid whose seqid is behind the one the open now answers to is old, and a
// special stateid names nothing any of them can act on.

// validityFixture opens and confirms one file, returning the confirmed stateid
// and the open-owner seqid the next request must carry.
func validityFixture(t *testing.T, sm *StateManager, owner string) (fh []byte, opened, confirmed types.Stateid4, nextSeqid uint32) {
	t.Helper()

	fh = []byte("fh-" + owner)
	res, err := sm.OpenFile(0, []byte(owner), 1, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	conf, err := sm.ConfirmOpen(&res.Stateid, 2, 0)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}
	return fh, res.Stateid, conf.Stateid, 3
}

// foreignEpoch rewrites only the boot-epoch fragment of a stateid, leaving its
// type tag and random bytes alone, so a rejection can only be the epoch's
// doing. It is what the pynfs suite's makeStaleId simulates: a stateid a client
// held across a server restart.
func foreignEpoch(sid types.Stateid4) *types.Stateid4 {
	stale := sid
	stale.Other[1], stale.Other[2], stale.Other[3] = 0xFE, 0xED, 0xFA
	return &stale
}

func TestCloseFile_ForeignEpochStateidIsStale(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	_, _, confirmed, seqid := validityFixture(t, sm, "close-stale")

	if _, err := sm.CloseFile(foreignEpoch(confirmed), seqid, 0); !errors.Is(err, ErrStaleStateid) {
		t.Errorf("CLOSE with a stateid from another incarnation: err = %v, want NFS4ERR_STALE_STATEID", err)
	}
}

func TestCloseFile_OldStateidIsRejected(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	// OPEN_CONFIRM advanced the seqid, so the stateid OPEN returned is now one
	// behind the one the open answers to.
	_, opened, confirmed, seqid := validityFixture(t, sm, "close-old")
	if opened.Seqid == confirmed.Seqid {
		t.Fatal("OPEN_CONFIRM must advance the stateid seqid for this to test anything")
	}

	if _, err := sm.CloseFile(&opened, seqid, 0); !errors.Is(err, ErrOldStateid) {
		t.Errorf("CLOSE with the pre-OPEN_CONFIRM stateid: err = %v, want NFS4ERR_OLD_STATEID", err)
	}
}

func TestConfirmOpen_ForeignEpochStateidIsStale(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	res, err := sm.OpenFile(0, []byte("confirm-stale"), 1, []byte("fh-confirm-stale"),
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	if _, err := sm.ConfirmOpen(foreignEpoch(res.Stateid), 2, 0); !errors.Is(err, ErrStaleStateid) {
		t.Errorf("OPEN_CONFIRM with a stateid from another incarnation: err = %v, want NFS4ERR_STALE_STATEID", err)
	}
}

// TestConfirmOpen_TwiceIsBadStateid covers RFC 7530 Section 16.18.5: the second
// OPEN_CONFIRM finds the open already confirmed, so its stateid no longer names
// anything OPEN_CONFIRM can act on.
func TestConfirmOpen_TwiceIsBadStateid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	_, _, confirmed, seqid := validityFixture(t, sm, "confirm-twice")

	if _, err := sm.ConfirmOpen(&confirmed, seqid, 0); !errors.Is(err, ErrBadStateid) {
		t.Errorf("second OPEN_CONFIRM: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
}

func TestDowngradeOpen_ForeignEpochStateidIsStale(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	_, _, confirmed, seqid := validityFixture(t, sm, "downgrade-stale")

	if _, err := sm.DowngradeOpen(foreignEpoch(confirmed), seqid,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, 0); !errors.Is(err, ErrStaleStateid) {
		t.Errorf("OPEN_DOWNGRADE with a stateid from another incarnation: err = %v, want NFS4ERR_STALE_STATEID", err)
	}
}

func TestDowngradeOpen_OldStateidIsRejected(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	_, opened, _, seqid := validityFixture(t, sm, "downgrade-old")

	if _, err := sm.DowngradeOpen(&opened, seqid,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, 0); !errors.Is(err, ErrOldStateid) {
		t.Errorf("OPEN_DOWNGRADE with the pre-OPEN_CONFIRM stateid: err = %v, want NFS4ERR_OLD_STATEID", err)
	}
}

// TestDowngradeOpen_NeverOpenedMode covers RFC 7530 Section 16.19.4. READ is a
// subset of the accumulated share_access of an OPEN for BOTH, but no OPEN asked
// for READ, so downgrading to it names a mode the client never held. Once an
// OPEN does ask for READ the same downgrade is legal.
func TestDowngradeOpen_NeverOpenedMode(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	fh, _, confirmed, seqid := validityFixture(t, sm, "downgrade-mode")

	if _, err := sm.DowngradeOpen(&confirmed, seqid,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, 0); !isStatus(err, types.NFS4ERR_INVAL) {
		t.Fatalf("OPEN_DOWNGRADE to a never-opened mode: err = %v, want NFS4ERR_INVAL", err)
	}

	reopened, err := sm.OpenFile(0, []byte("downgrade-mode"), seqid+1, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile (READ): %v", err)
	}
	downgraded, err := sm.DowngradeOpen(&reopened.Stateid, seqid+2,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, 0)
	if err != nil {
		t.Fatalf("OPEN_DOWNGRADE to a mode that was opened: %v", err)
	}

	// That downgrade kept READ and dropped the modes it did not, so READ is the
	// only mode a further OPEN_DOWNGRADE may name.
	again, err := sm.DowngradeOpen(&downgraded.Stateid, seqid+3,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, 0)
	if err != nil {
		t.Fatalf("second OPEN_DOWNGRADE to READ: %v", err)
	}
	if _, err := sm.DowngradeOpen(&again.Stateid, seqid+4,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, 0); !isStatus(err, types.NFS4ERR_INVAL) {
		t.Fatalf("OPEN_DOWNGRADE back up to BOTH: err = %v, want NFS4ERR_INVAL", err)
	}
}

func TestLockNew_ForeignEpochOpenStateidIsStale(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lock.NewManager())
	defer sm.Shutdown()

	fh, _, confirmed, seqid := validityFixture(t, sm, "lock-stale")

	_, err := sm.LockNew(context.Background(), 0, []byte("lock-owner"), 1,
		foreignEpoch(confirmed), seqid, fh, types.WRITE_LT, 0, 100, false, 0)
	if !errors.Is(err, ErrStaleStateid) {
		t.Errorf("LOCK with an open stateid from another incarnation: err = %v, want NFS4ERR_STALE_STATEID", err)
	}
}

func TestLockNew_OldOpenStateidIsRejected(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lock.NewManager())
	defer sm.Shutdown()

	fh, opened, _, seqid := validityFixture(t, sm, "lock-old")

	_, err := sm.LockNew(context.Background(), 0, []byte("lock-owner"), 1,
		&opened, seqid, fh, types.WRITE_LT, 0, 100, false, 0)
	if !errors.Is(err, ErrOldStateid) {
		t.Errorf("LOCK with the pre-OPEN_CONFIRM open stateid: err = %v, want NFS4ERR_OLD_STATEID", err)
	}
}

func TestLockNew_SpecialOpenStateidIsBad(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lock.NewManager())
	defer sm.Shutdown()

	fh, _, _, seqid := validityFixture(t, sm, "lock-special")

	anonymous := &types.Stateid4{Seqid: 0}
	_, err := sm.LockNew(context.Background(), 0, []byte("lock-owner"), 1,
		anonymous, seqid, fh, types.WRITE_LT, 0, 100, false, 0)
	if !errors.Is(err, ErrBadStateid) {
		t.Errorf("LOCK with the anonymous stateid: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
}

// TestStateidMissError_CurrentEpochIsBadNotStale is the negative control for the
// epoch checks above: an "other" this incarnation could have issued but never
// did stays NFS4ERR_BAD_STATEID, so the stale answer is the epoch's doing and
// not a blanket rename of the miss.
func TestStateidMissError_CurrentEpochIsBadNotStale(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	_, _, confirmed, seqid := validityFixture(t, sm, "miss-current")

	unissued := confirmed
	unissued.Other = sm.generateStateidOther(StateTypeOpen)

	if _, err := sm.CloseFile(&unissued, seqid, 0); !errors.Is(err, ErrBadStateid) {
		t.Errorf("CLOSE with an unissued stateid of this incarnation: err = %v, want NFS4ERR_BAD_STATEID", err)
	}
}
