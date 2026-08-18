package state

import (
	"bytes"
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

// ============================================================================
// Errors exempt under RFC 7530 Section 9.1.7 must leave the seqid alone
// ============================================================================

// TestCloseFile_BadSeqidLeavesOwnerSeqidUntouched pins the seqid across a
// rejected CLOSE. NFS4ERR_BAD_SEQID is on the exempt list of RFC 7530 Section
// 9.1.7: the client does not advance its sequence after one, so neither may the
// server.
func TestCloseFile_BadSeqidLeavesOwnerSeqidUntouched(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lock.NewManager())

	_, _, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// A seqid the owner never reached: neither the expected next one nor a
	// replay of the last.
	if _, err := sm.CloseFile(openStateid, openSeqid+7); !errors.Is(err, ErrBadSeqid) {
		t.Fatalf("CLOSE at an out-of-sequence seqid: got %v, want NFS4ERR_BAD_SEQID", err)
	}

	// The client is still at the seqid it was, and so must the server be.
	if _, err := sm.CloseFile(openStateid, openSeqid+1); err != nil {
		t.Fatalf("CLOSE at the expected seqid after a rejected one failed: %v "+
			"(the rejected request advanced the open-owner seqid; it must not)", err)
	}
}

// TestUnlockFile_BadStateidLeavesLockOwnerSeqidUntouched pins the seqid across a
// LOCKU whose stateid seqid runs ahead of the server's. NFS4ERR_BAD_STATEID is
// on the exempt list of RFC 7530 Section 9.1.7: the client leaves its sequence
// where it was, so the server must too.
func TestUnlockFile_BadStateidLeavesLockOwnerSeqidUntouched(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lock.NewManager())

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	lockSeqid := uint32(1)
	lockRes, err := sm.LockNew(context.Background(),
		clientID, []byte("lock-owner"), lockSeqid,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	ahead := lockRes.Stateid
	ahead.Seqid = nextSeqID(ahead.Seqid)
	if _, err := sm.UnlockFile(&ahead, lockSeqid+1, types.WRITE_LT, 0, 100); !errors.Is(err, ErrBadStateid) {
		t.Fatalf("LOCKU with a stateid ahead of the server's: got %v, want NFS4ERR_BAD_STATEID", err)
	}

	// The retry carries the right stateid at the same lock-owner seqid, which
	// the server must still be expecting.
	if _, err := sm.UnlockFile(&lockRes.Stateid, lockSeqid+1, types.WRITE_LT, 0, 100); err != nil {
		t.Fatalf("LOCKU retried after NFS4ERR_BAD_STATEID failed: %v "+
			"(the rejected request advanced the lock-owner seqid; it must not)", err)
	}
}

// TestDowngradeOpen_ReplayLeavesOwnerSeqidUntouched pins the seqid and the
// reply cache across a retransmit. RFC 7530 Section 9.1.7 returns the stored
// response for a request at the last seqid without re-executing it, so it moves
// neither.
func TestDowngradeOpen_ReplayLeavesOwnerSeqidUntouched(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	clientID, _, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	seqid := openSeqid + 1
	if _, err := sm.DowngradeOpen(openStateid, seqid,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE); err != nil {
		t.Fatalf("OPEN_DOWNGRADE failed: %v", err)
	}

	// As the handler does on success: cache the encoded reply for replay.
	cached := []byte("encoded OPEN_DOWNGRADE reply")
	sm.CacheOpenOwnerResult(clientID, []byte("open-owner"), types.NFS4_OK, cached)

	// Every retransmit gets that reply back, not just the first.
	for i := range 2 {
		_, err := sm.DowngradeOpen(openStateid, seqid,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)
		var replayErr *ReplayError
		if !errors.As(err, &replayErr) {
			t.Fatalf("retransmit %d of OPEN_DOWNGRADE: got %v, want a replay", i+1, err)
		}
		if replayErr.Status != types.NFS4_OK || !bytes.Equal(replayErr.Data, cached) {
			t.Fatalf("retransmit %d replayed status=%d data=%q, want status=%d data=%q "+
				"(the replay consumed the seqid and overwrote the cached reply)",
				i+1, replayErr.Status, replayErr.Data, types.NFS4_OK, cached)
		}
	}

	// The sequence is still where the successful OPEN_DOWNGRADE left it.
	if _, err := sm.DowngradeOpen(openStateid, seqid+1,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE); err != nil {
		t.Fatalf("OPEN_DOWNGRADE after replays failed: %v "+
			"(a replay advanced the open-owner seqid; it must not)", err)
	}
}
