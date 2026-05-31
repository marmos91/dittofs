package state

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// These tests cover the NFSv4.0 owner-seqid replay caches (RFC 7530 §9.1.7):
//   - H4: OPEN/CLOSE/OPEN_DOWNGRADE/OPEN_CONFIRM replay on the open-owner.
//   - H5: LOCK/LOCKU replay on the lock-owner.
//
// They model what the handler does: run the op, cache its encoded reply via the
// matching Cache*OwnerResult, then retransmit at the same seqid and assert the
// state manager returns a *ReplayError carrying the exact cached bytes (rather
// than NFS4ERR_BAD_SEQID, which would corrupt the client's seqid bookkeeping or
// drop a lock-owner).

func mustReplay(t *testing.T, err error, wantStatus uint32, wantData []byte) {
	t.Helper()
	var re *ReplayError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ReplayError, got %T: %v", err, err)
	}
	if re.Status != wantStatus {
		t.Errorf("replay status = %d, want %d", re.Status, wantStatus)
	}
	if !bytes.Equal(re.Data, wantData) {
		t.Errorf("replay data = %x, want %x", re.Data, wantData)
	}
}

// setupConfirmedOpen opens and confirms a file, returning the confirmed open
// stateid and the open-owner identity. The open-owner's next seqid is 3.
func setupConfirmedOpen(t *testing.T, sm *StateManager, ownerData []byte, fh []byte, access uint32) (*types.Stateid4, uint64) {
	t.Helper()
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	res, err := sm.SetClientID("replay-client", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	open, err := sm.OpenFile(res.ClientID, ownerData, 1, fh, access, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	confirmed, err := sm.ConfirmOpen(&open.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}
	return &confirmed.Stateid, res.ClientID
}

// ---------------------------------------------------------------------------
// H4 — open-owner replay (CLOSE / OPEN_DOWNGRADE / OPEN_CONFIRM)
// ---------------------------------------------------------------------------

func TestReplay_Close_ReturnsCachedReply(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	owner := []byte("close-replay-owner")
	stateid, clientID := setupConfirmedOpen(t, sm, owner, []byte("fh-close"), types.OPEN4_SHARE_ACCESS_BOTH)

	// CLOSE at seqid 3 (next open-owner seqid).
	closed, err := sm.CloseFile(stateid, 3)
	if err != nil {
		t.Fatalf("CloseFile: %v", err)
	}
	cachedReply := []byte("close-encoded-reply")
	sm.CacheOpenOwnerResult(clientID, closed.OwnerData, types.NFS4_OK, cachedReply)

	// Retransmit CLOSE at the SAME seqid -> must replay the cached reply,
	// not NFS4ERR_BAD_SEQID (which would happen with the old single-OPEN cache).
	_, err = sm.CloseFile(stateid, 3)
	mustReplay(t, err, types.NFS4_OK, cachedReply)
}

func TestReplay_OpenDowngrade_ReturnsCachedReply(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	owner := []byte("downgrade-replay-owner")
	stateid, clientID := setupConfirmedOpen(t, sm, owner, []byte("fh-downgrade"), types.OPEN4_SHARE_ACCESS_BOTH)

	dg, err := sm.DowngradeOpen(stateid, 3, types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)
	if err != nil {
		t.Fatalf("DowngradeOpen: %v", err)
	}
	cachedReply := []byte("downgrade-encoded-reply")
	sm.CacheOpenOwnerResult(clientID, dg.OwnerData, types.NFS4_OK, cachedReply)

	// A second DOWNGRADE that would advance the stateid seqid must NOT run on a
	// replay; the original encoded reply is returned verbatim.
	_, err = sm.DowngradeOpen(stateid, 3, types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)
	mustReplay(t, err, types.NFS4_OK, cachedReply)
}

func TestReplay_OpenConfirm_ReturnsCachedReply(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	res, err := sm.SetClientID("confirm-replay-client", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	owner := []byte("confirm-replay-owner")
	open, err := sm.OpenFile(res.ClientID, owner, 1, []byte("fh-confirm"),
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	confirmed, err := sm.ConfirmOpen(&open.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}
	cachedReply := []byte("confirm-encoded-reply")
	sm.CacheOpenOwnerResult(res.ClientID, confirmed.OwnerData, types.NFS4_OK, cachedReply)

	// Retransmit OPEN_CONFIRM at seqid 2.
	_, err = sm.ConfirmOpen(&open.Stateid, 2)
	mustReplay(t, err, types.NFS4_OK, cachedReply)
}

// ---------------------------------------------------------------------------
// H5 — lock-owner replay (LOCK / LOCKU)
// ---------------------------------------------------------------------------

func TestReplay_Lock_ReturnsCachedReply(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	clientID, fh, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// LOCK (new lock-owner) at lock seqid 1.
	res, err := sm.LockNew(clientID, []byte("lock-replay-owner"), 1,
		openStateid, openSeqid+1, fh, types.WRITE_LT, 0, 100, false)
	if err != nil {
		t.Fatalf("LockNew: %v", err)
	}
	if res.Denied != nil {
		t.Fatalf("unexpected lock conflict")
	}
	cachedReply := []byte("lock-encoded-reply")
	sm.CacheLockOwnerResult(res.OwnerClientID, res.OwnerData, types.NFS4_OK, cachedReply)

	// Retransmit the LOCK at the SAME lock seqid via the existing-lock-owner
	// path. The old code returned NFS4ERR_BAD_SEQID here (fatal to the Linux
	// client -> dropped lock-owner / silent lock loss); it must replay instead.
	_, err = sm.LockExisting(&res.Stateid, 1, fh, types.WRITE_LT, 0, 100, false)
	mustReplay(t, err, types.NFS4_OK, cachedReply)
}

func TestReplay_LockU_ReturnsCachedReply(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	clientID, fh, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	res, err := sm.LockNew(clientID, []byte("locku-replay-owner"), 1,
		openStateid, openSeqid+1, fh, types.WRITE_LT, 0, 100, false)
	if err != nil {
		t.Fatalf("LockNew: %v", err)
	}

	// LOCKU at lock seqid 2.
	unlock, err := sm.UnlockFile(&res.Stateid, 2, types.WRITE_LT, 0, 100)
	if err != nil {
		t.Fatalf("UnlockFile: %v", err)
	}
	cachedReply := []byte("locku-encoded-reply")
	sm.CacheLockOwnerResult(unlock.OwnerClientID, unlock.OwnerData, types.NFS4_OK, cachedReply)

	// Retransmit LOCKU: the client resends the ORIGINAL (pre-LOCKU) lock
	// stateid whose seqid is now one behind. This must be detected as a replay
	// (returning the cached reply) and not rejected as NFS4ERR_OLD_STATEID.
	_, err = sm.UnlockFile(&res.Stateid, 2, types.WRITE_LT, 0, 100)
	mustReplay(t, err, types.NFS4_OK, cachedReply)
}

// TestReplay_Lock_Denied_CachesAndReplays verifies that a DENIED LOCK (which
// still advances the lock-owner seqid per RFC 7530 §8.1.5) caches its reply and
// replays it on retransmit instead of failing NFS4ERR_BAD_SEQID.
func TestReplay_Lock_Denied_CachesAndReplays(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	fh := []byte("/export:denied-replay")

	// Client 1 holds an exclusive lock on [0,100).
	c1, _, open1, seq1 := setupClientAndOpenStateNamed(t, sm, "denied-c1", "owner-1", fh)
	if _, err := sm.LockNew(c1, []byte("lock-owner-1"), 1, open1, seq1+1, fh, types.WRITE_LT, 0, 100, false); err != nil {
		t.Fatalf("LockNew c1: %v", err)
	}

	// Client 2's overlapping LOCK is DENIED.
	c2, _, open2, seq2 := setupClientAndOpenStateNamed(t, sm, "denied-c2", "owner-2", fh)
	res, err := sm.LockNew(c2, []byte("lock-owner-2"), 1, open2, seq2+1, fh, types.WRITE_LT, 50, 100, false)
	if err != nil {
		t.Fatalf("LockNew c2: %v", err)
	}
	if res.Denied == nil {
		t.Fatalf("expected DENIED for overlapping exclusive lock")
	}
	cachedReply := []byte("denied-encoded-reply")
	sm.CacheLockOwnerResult(res.OwnerClientID, res.OwnerData, types.NFS4ERR_DENIED, cachedReply)

	// Retransmit client 2's LOCK at the SAME open+lock seqids -> replay the
	// cached DENIED reply (both seqids were advanced by the DENIED original).
	_, err = sm.LockNew(c2, []byte("lock-owner-2"), 1, open2, seq2+1, fh, types.WRITE_LT, 50, 100, false)
	mustReplay(t, err, types.NFS4ERR_DENIED, cachedReply)
}

// setupClientAndOpenStateNamed is like setupClientAndOpenState but lets the
// caller pick distinct client/owner names and a shared file handle so multiple
// clients can contend on the same file.
func setupClientAndOpenStateNamed(t *testing.T, sm *StateManager, clientName, ownerName string, fh []byte) (uint64, []byte, *types.Stateid4, uint32) {
	t.Helper()
	verifier := [8]byte{byte(len(clientName)), 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	res, err := sm.SetClientID(clientName, verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID %s: %v", clientName, err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID %s: %v", clientName, err)
	}
	open, err := sm.OpenFile(res.ClientID, []byte(ownerName), 1, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", clientName, err)
	}
	confirmed, err := sm.ConfirmOpen(&open.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen %s: %v", clientName, err)
	}
	return res.ClientID, fh, &confirmed.Stateid, 2
}
