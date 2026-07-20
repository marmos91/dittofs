package lock

import (
	"context"
	"sync"
	"testing"
	"time"
)

// dlease2Key is the DLEASE2 lease-key constant smbtorture reuses across every
// dirlease subtest (source4/torture/smb2/lease.c).
var dlease2Key = [16]byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// twoSameKeyDirLeases injects two RH directory-lease records that share key
// under one handleKey: a stale ack-to-None-then-regranted record (dead session)
// coexisting with the live record — exactly the shape an unclean disconnect
// leaves. RequestLease dedups same-key grants, so the coexistence is built
// directly. Returns the two injected records (in injection order) and a per-key
// break counter the caller asserts against.
func twoSameKeyDirLeases(t *testing.T, lm *Manager, handleKey string, key [16]byte) (*sync.Mutex, map[[16]byte]int, []*UnifiedLock) {
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

	records := []*UnifiedLock{
		{
			Owner: LockOwner{OwnerID: "stale", ClientID: "smb:1"},
			Lease: &OpLock{LeaseKey: key, LeaseState: LeaseStateRead | LeaseStateHandle, Epoch: 1, IsDirectory: true},
		},
		{
			Owner: LockOwner{OwnerID: "live", ClientID: "smb:2"},
			Lease: &OpLock{LeaseKey: key, LeaseState: LeaseStateRead | LeaseStateHandle, Epoch: 1, IsDirectory: true},
		},
	}

	lm.mu.Lock()
	lm.unifiedLocks[handleKey] = records
	// Populate the lease-key index so findLeaseByKey (used by
	// AcknowledgeLeaseBreak) can resolve these directly-injected records, as it
	// would for records added through the normal grant path.
	lm.reindexHandleLocked(handleKey, nil)
	lm.mu.Unlock()

	return mu, breaksByKey, records
}

// assertBothSiblingsBreaking verifies that every same-key record ends in the
// SAME break stage after a dedup'd break: Breaking=true and identical
// BreakToState / BreakingToRequired / Epoch. Per MS-SMB2 §3.3.5.9 opens sharing
// a lease key share one logical lease, so the sibling skipped for the wire
// notification must still carry the canonical record's break state — otherwise a
// later scan treats the stale sibling as an active non-breaking lease and
// dispatches a fresh spurious break.
func assertBothSiblingsBreaking(t *testing.T, lm *Manager, records []*UnifiedLock) {
	t.Helper()

	lm.mu.Lock()
	defer lm.mu.Unlock()

	first := records[0].Lease
	for i, rec := range records {
		l := rec.Lease
		if !l.Breaking {
			t.Fatalf("record %d (%s): Breaking=false after dedup'd break; "+
				"a skipped sibling must mirror the canonical break stage "+
				"(else a later scan dispatches a fresh spurious break)", i, rec.Owner.OwnerID)
		}
		if l.BreakToState != first.BreakToState ||
			l.BreakingToRequired != first.BreakingToRequired ||
			l.Epoch != first.Epoch {
			t.Fatalf("record %d (%s): break stage diverged from canonical: "+
				"BreakToState=%#x BreakingToRequired=%#x Epoch=%d vs canonical "+
				"BreakToState=%#x BreakingToRequired=%#x Epoch=%d",
				i, rec.Owner.OwnerID,
				l.BreakToState, l.BreakingToRequired, l.Epoch,
				first.BreakToState, first.BreakingToRequired, first.Epoch)
		}
	}
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
	mu, breaksByKey, records := twoSameKeyDirLeases(t, lm, handleKey, dlease2Key)

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

	// Both same-key records must carry the break stage so a later scan can't
	// treat the skipped sibling as an active non-breaking lease.
	assertBothSiblingsBreaking(t, lm, records)
}

// TestOnDirChange_DedupsByLeaseKey asserts the same single-notification-per-key
// invariant on the metadata-store delete path (RemoveFile -> notifyDirChange ->
// OnDirChange), the other entry point that breaks parent dir leases on a
// content change.
func TestOnDirChange_DedupsByLeaseKey(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	const handleKey = "/share:dir-uuid"
	mu, breaksByKey, records := twoSameKeyDirLeases(t, lm, handleKey, dlease2Key)

	// Origin client "smb:3" differs from both holders, so neither is suppressed
	// by the origin-client rule — both records are break candidates.
	lm.OnDirChange(FileHandle(handleKey), DirChangeRemoveEntry, "smb:3", [16]byte{}, false)

	mu.Lock()
	got := breaksByKey[dlease2Key]
	mu.Unlock()
	if got != 1 {
		t.Fatalf("OnDirChange dispatched %d notifications for lease key %x; want exactly 1", got, dlease2Key)
	}

	// Both same-key records must carry the break stage (Breaking + matching
	// BreakToState/BreakingToRequired/Epoch) so a later OnDirChange scan can't
	// treat the skipped sibling as an active non-breaking lease.
	assertBothSiblingsBreaking(t, lm, records)
}

// oneDirLease injects a single RH directory-lease record under handleKey owned by
// ownerClient with the given key, wires a per-key break counter, and returns it.
func oneDirLease(t *testing.T, lm *Manager, handleKey, ownerClient string, key [16]byte) (*sync.Mutex, map[[16]byte]int) {
	t.Helper()
	mu := &sync.Mutex{}
	breaks := map[[16]byte]int{}
	lm.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(_ string, l *UnifiedLock, _ uint32) {
			mu.Lock()
			breaks[l.Lease.LeaseKey]++
			mu.Unlock()
		},
	})
	lm.mu.Lock()
	lm.unifiedLocks[handleKey] = []*UnifiedLock{{
		Owner: LockOwner{OwnerID: "h1", ClientID: ownerClient},
		Lease: &OpLock{LeaseKey: key, LeaseState: LeaseStateRead | LeaseStateHandle, Epoch: 1, IsDirectory: true},
	}}
	lm.reindexHandleLocked(handleKey, nil)
	lm.mu.Unlock()
	return mu, breaks
}

// TestOnDirChange_BreaksOriginatorsOtherDirLease reproduces the deterministic
// count==0 behind smb2.dirlease.unlink_*_and_close. Per MS-SMB2 §3.3.4.20 and the
// Samba object-store rule, a directory-content change breaks every parent dir
// lease whose lease key differs from the change's parent lease key — INCLUDING a
// dir lease the originating client holds on a different handle. Only the
// parent-key-matched lease is spared; the originating CLIENT must not be
// blanket-excluded.
func TestOnDirChange_BreaksOriginatorsOtherDirLease(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	const handleKey = "/share:dir"
	victimKey := [16]byte{0x02}
	closerParentKey := [16]byte{0x01} // differs from victimKey → victim must break

	// The victim dir lease is owned by the SAME client that originates the delete.
	mu, breaks := oneDirLease(t, lm, handleKey, "smb:c1", victimKey)

	lm.OnDirChange(FileHandle(handleKey), DirChangeRemoveEntry, "smb:c1", closerParentKey, true)

	mu.Lock()
	defer mu.Unlock()
	if breaks[victimKey] != 1 {
		t.Fatalf("originator's other-key dir lease got %d breaks, want 1; a blanket "+
			"client-exclusion wrongly suppressed a lease the parent-key rule says must break "+
			"(the deterministic smb2.dirlease.unlink_*_and_close count==0)", breaks[victimKey])
	}
}

// TestOnDirChange_SuppressesParentKeyMatchedDirLease pins the other half of the
// rule: the dir lease whose key MATCHES the change's parent lease key is spared
// (the originator's own cached view), even without any client-level exclusion.
func TestOnDirChange_SuppressesParentKeyMatchedDirLease(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	const handleKey = "/share:dir"
	matchedKey := [16]byte{0x07}

	mu, breaks := oneDirLease(t, lm, handleKey, "smb:c1", matchedKey)

	// Parent key == the lease key → parent-key suppression spares it.
	lm.OnDirChange(FileHandle(handleKey), DirChangeRemoveEntry, "smb:c1", matchedKey, true)

	mu.Lock()
	defer mu.Unlock()
	if breaks[matchedKey] != 0 {
		t.Fatalf("parent-key-matched dir lease got %d breaks, want 0 (must be suppressed)", breaks[matchedKey])
	}
}

// TestOnDirChange_SerializesMultipleDirLeaseBreaks pins the serialized delivery
// of multiple directory RH-lease breaks (smb2.dirlease.unlink_different_*): when
// one directory change breaks two dir leases with different keys, only the first
// break is delivered immediately; acknowledging it delivers the second. Sending
// both at once produces the client-side lease_break_info.count==2 failure.
func TestOnDirChange_SerializesMultipleDirLeaseBreaks(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	const handleKey = "/share:dir"
	key1 := [16]byte{0x01}
	key2 := [16]byte{0x02}

	mu := &sync.Mutex{}
	var order [][16]byte
	lm.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(_ string, l *UnifiedLock, _ uint32) {
			mu.Lock()
			order = append(order, l.Lease.LeaseKey)
			mu.Unlock()
		},
	})
	lm.mu.Lock()
	lm.unifiedLocks[handleKey] = []*UnifiedLock{
		{Owner: LockOwner{OwnerID: "h1", ClientID: "smb:c1"},
			Lease: &OpLock{LeaseKey: key1, LeaseState: LeaseStateRead | LeaseStateHandle, Epoch: 1, IsDirectory: true}},
		{Owner: LockOwner{OwnerID: "h2", ClientID: "smb:c2"},
			Lease: &OpLock{LeaseKey: key2, LeaseState: LeaseStateRead | LeaseStateHandle, Epoch: 1, IsDirectory: true}},
	}
	lm.reindexHandleLocked(handleKey, nil)
	lm.mu.Unlock()

	// A change with no parent key breaks both dir leases.
	lm.OnDirChange(FileHandle(handleKey), DirChangeRemoveEntry, "smb:other", [16]byte{}, false)

	// Only the first break is delivered now; the second is deferred.
	mu.Lock()
	got := len(order)
	var first [16]byte
	if got == 1 {
		first = order[0]
	}
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly 1 dir-lease break delivered immediately (RH breaks serialize), got %d", got)
	}

	// Acknowledging the first break delivers the deferred second break.
	if err := lm.AcknowledgeLeaseBreak(context.Background(), first, LeaseStateNone, 0); err != nil {
		t.Fatalf("AcknowledgeLeaseBreak: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 {
		t.Fatalf("acknowledging the first break must deliver the deferred second; got %d total breaks", len(order))
	}
	if order[1] == first {
		t.Fatalf("second break repeated the first lease key %x; expected the other dir lease", first)
	}
}

// TestAcknowledgeLeaseBreak_ClearsMirroredSiblings is a regression test for the
// create/delete-heavy SMB3 stall: a mirrored sibling left Breaking=true after
// the client's single acknowledge.
//
// A directory-content-change break dispatches one wire LEASE_BREAK per lease key
// and mirrors the break stage onto every sibling sharing that key without a
// second notification (opens sharing a key are one logical lease, MS-SMB2
// §3.3.5.9 — see assertBothSiblingsBreaking). The client then sends exactly ONE
// acknowledge for the key, which findLeaseByKey resolves to a single record.
// Unless the acknowledge clears every record sharing the key, the mirrored
// sibling stays Breaking=true forever, so WaitForBreakCompletion on that
// handleKey blocks until the force-complete timeout on every following operation.
func TestAcknowledgeLeaseBreak_ClearsMirroredSiblings(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	const handleKey = "/share:dir-uuid"
	_, _, records := twoSameKeyDirLeases(t, lm, handleKey, dlease2Key)

	// Break both same-key dir leases to None: one notification, sibling mirrored.
	lm.OnDirChange(FileHandle(handleKey), DirChangeRemoveEntry, "smb:3", [16]byte{}, false)

	// The client sends exactly ONE acknowledge for the shared lease key.
	if err := lm.AcknowledgeLeaseBreak(context.Background(), dlease2Key, LeaseStateNone, 0); err != nil {
		t.Fatalf("AcknowledgeLeaseBreak: %v", err)
	}

	// Every record sharing the key — the one findLeaseByKey resolved AND the
	// mirrored sibling — must land at None with Breaking cleared.
	lm.mu.Lock()
	for i, rec := range records {
		if rec.Lease.Breaking {
			lm.mu.Unlock()
			t.Fatalf("record %d (%s): Breaking=true after a single acknowledge; the "+
				"mirrored sibling was never cleared (WaitForBreakCompletion stalls until "+
				"the force-complete timeout)", i, rec.Owner.OwnerID)
		}
		if rec.Lease.LeaseState != LeaseStateNone {
			lm.mu.Unlock()
			t.Fatalf("record %d (%s): LeaseState=%#x after ack-to-None; want None",
				i, rec.Owner.OwnerID, rec.Lease.LeaseState)
		}
	}
	lm.mu.Unlock()

	// With no record left Breaking, WaitForBreakCompletion returns at once rather
	// than blocking until the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lm.WaitForBreakCompletion(ctx, handleKey); err != nil {
		t.Fatalf("WaitForBreakCompletion blocked/errored with a stuck mirrored sibling: %v", err)
	}
}
