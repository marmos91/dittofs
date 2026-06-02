package lock

import (
	"sync"
	"testing"
)

// dlease2Key is the DLEASE2 lease-key constant smbtorture reuses across every
// dirlease subtest (source4/torture/smb2/lease.c).
var dlease2Key = [16]byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// twoSameKeyDirLeases injects two RH directory-lease records that share key
// under one handleKey: a stale ack-to-None-then-regranted record (dead session)
// coexisting with the live record — exactly the shape an unclean disconnect
// leaves. RequestLease dedups same-key grants, so the coexistence is built
// directly. Returns a per-key break counter the caller asserts against.
func twoSameKeyDirLeases(t *testing.T, lm *Manager, handleKey string, key [16]byte) (*sync.Mutex, map[[16]byte]int) {
	t.Helper()

	mu := &sync.Mutex{}
	breaksByKey := map[[16]byte]int{}
	lm.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(_ string, l *UnifiedLock, _ uint32) {
			mu.Lock()
			breaksByKey[l.Lease.LeaseKey]++
			mu.Unlock()
		},
	})

	lm.mu.Lock()
	lm.unifiedLocks[handleKey] = []*UnifiedLock{
		{
			Owner: LockOwner{OwnerID: "stale", ClientID: "smb:1"},
			Lease: &OpLock{LeaseKey: key, LeaseState: LeaseStateRead | LeaseStateHandle, Epoch: 1, IsDirectory: true},
		},
		{
			Owner: LockOwner{OwnerID: "live", ClientID: "smb:2"},
			Lease: &OpLock{LeaseKey: key, LeaseState: LeaseStateRead | LeaseStateHandle, Epoch: 1, IsDirectory: true},
		},
	}
	lm.mu.Unlock()

	return mu, breaksByKey
}

// TestBreakLeasesOnOpenConflict_DedupsByLeaseKey is a regression test for the
// intermittent smbtorture smb2.dirlease.unlink_*_and_close double-break flake
// (lease.c:7653 "wrong value for lease_break_info.count got 0x2 - should be
// 0x1").
//
// Root cause: a single directory-content-change break iterated every
// *UnifiedLock record under the parent handleKey and dispatched one wire
// LEASE_BREAK per record. smbtorture reuses the DLEASE2 lease-key constant
// across every dirlease subtest on one ClientGUID, so an orphaned ack-to-None
// record left under the same handleKey by an earlier subtest (or an unclean
// disconnect) coexists with the live record under the SAME lease key. With no
// per-key dedup, breaking that handle dispatched TWO notifications for ONE
// logical lease, and GetSessionForBreak routed both to the same live primary
// session (shared ClientGUID) — the client observed count==2.
//
// Samba dispatches exactly one LEASE_BREAK per distinct lease key per change
// (source3/smbd/smb2_oplock.c contend_dirleases -> do_dirlease_break_to_none
// breaks each holder once). This test injects two directory-lease records that
// share a lease key under one handleKey and asserts a single content-change
// break dispatches exactly one notification for that key.
func TestBreakLeasesOnOpenConflict_DedupsByLeaseKey(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	const handleKey = "/share:dir-uuid"
	mu, breaksByKey := twoSameKeyDirLeases(t, lm, handleKey, dlease2Key)

	// A directory-content change (unlink) breaks the parent dir lease to None.
	if err := lm.BreakLeasesOnOpenConflict(handleKey, nil, BreakReasonDestructive); err != nil {
		t.Fatalf("BreakLeasesOnOpenConflict: %v", err)
	}

	mu.Lock()
	got := breaksByKey[dlease2Key]
	mu.Unlock()
	if got != 1 {
		t.Fatalf("dir-lease break dispatched %d notifications for lease key %x; "+
			"want exactly 1 (smbtorture unlink_*_and_close asserts lease_break_info.count==1)",
			got, dlease2Key)
	}
}

// TestOnDirChange_DedupsByLeaseKey asserts the same single-notification-per-key
// invariant on the metadata-store delete path (RemoveFile -> notifyDirChange ->
// OnDirChange), the other entry point that breaks parent dir leases on a
// content change.
func TestOnDirChange_DedupsByLeaseKey(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	const handleKey = "/share:dir-uuid"
	mu, breaksByKey := twoSameKeyDirLeases(t, lm, handleKey, dlease2Key)

	// Origin client "smb:3" differs from both holders, so neither is suppressed
	// by the origin-client rule — both records are break candidates.
	lm.OnDirChange(FileHandle(handleKey), DirChangeRemoveEntry, "smb:3", [16]byte{}, false)

	mu.Lock()
	got := breaksByKey[dlease2Key]
	mu.Unlock()
	if got != 1 {
		t.Fatalf("OnDirChange dispatched %d notifications for lease key %x; want exactly 1", got, dlease2Key)
	}
}
