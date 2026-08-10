package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Open-owner seqid must advance on a failed CLOSE
// ============================================================================

// TestCloseFile_LocksHeldStillAdvancesSeqid pins the sequencing rule for a
// CLOSE that fails because byte-range locks are still held.
//
// RFC 7530 Section 9.1.7 advances the owner's seqid on every operation that
// reaches seqid checking, whether or not it then succeeds. Only a short list of
// errors leaves the seqid untouched — NFS4ERR_STALE_CLIENTID,
// NFS4ERR_STALE_STATEID, NFS4ERR_BAD_STATEID, NFS4ERR_BAD_SEQID,
// NFS4ERR_BADXDR, NFS4ERR_RESOURCE, NFS4ERR_NOFILEHANDLE and NFS4ERR_MOVED —
// because those mean the request was never attributable to the owner in the
// first place. NFS4ERR_LOCKS_HELD is not among them: the owner and its seqid
// were both valid, and the server simply declined the close.
//
// A client that gets NFS4ERR_LOCKS_HELD therefore moves on to the next seqid.
// If the server does not, the two disagree permanently, and every later
// operation for that owner is answered NFS4ERR_BAD_SEQID — an error the client
// cannot recover from without tearing down the owner, so it surfaces as an I/O
// error to whatever was using the file.
//
// The sequence below is the one a WAL database produces: lock, close while
// still holding the lock, unlock, close again.
func TestCloseFile_LocksHeldStillAdvancesSeqid(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// Take a byte-range lock so the CLOSE below has something to trip over.
	lockSeqid := uint32(1)
	openSeqid++
	lockRes, err := sm.LockNew(context.Background(),
		clientID, []byte("lock-owner"), lockSeqid,
		openStateid, openSeqid,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	// CLOSE with locks outstanding: expected to fail, and expected to consume
	// its seqid on the way out.
	openSeqid++
	_, err = sm.CloseFile(openStateid, openSeqid)
	var stateErr *NFS4StateError
	if !errors.As(err, &stateErr) || stateErr.Status != types.NFS4ERR_LOCKS_HELD {
		t.Fatalf("CLOSE with locks held: got %v, want NFS4ERR_LOCKS_HELD", err)
	}

	// Release the lock, as a client does on being told LOCKS_HELD.
	if _, err := sm.UnlockFile(&lockRes.Stateid, lockSeqid+1, types.WRITE_LT, 0, 100); err != nil {
		t.Fatalf("UnlockFile failed: %v", err)
	}

	// The retry carries the next seqid. It must be accepted: the server owes
	// the client agreement about where the sequence got to.
	openSeqid++
	if _, err := sm.CloseFile(openStateid, openSeqid); err != nil {
		if errors.Is(err, ErrBadSeqid) {
			t.Fatalf("CLOSE after LOCKS_HELD rejected with NFS4ERR_BAD_SEQID: "+
				"the failed CLOSE did not advance the open-owner seqid, so the "+
				"client and server disagree about it from here on (seqid=%d)", openSeqid)
		}
		t.Fatalf("CLOSE after releasing locks failed: %v", err)
	}
}
