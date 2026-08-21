package state

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// These tests guard the secondary indexes (openStateByFile and
// revokedDelegCount) against drift from the authoritative state. Each mutation
// path that touches openStateByOther / delegByOther must keep the index in
// sync; the assertions below check the index directly (white-box) after every
// open/close/free/evict/revoke/return so a missed maintenance call is caught.

// fileIndexLen returns the number of OpenStates indexed under fileHandle.
func fileIndexLen(sm *StateManager, fileHandle []byte) int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.openStateByFile[string(fileHandle)])
}

// fileIndexMatchesAuthoritative verifies that openStateByFile is a faithful
// reindex of openStateByOther: same membership, no orphan/empty file slots.
func fileIndexMatchesAuthoritative(t *testing.T, sm *StateManager) {
	t.Helper()
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Every authoritative open must appear exactly once under its file handle.
	for _, os := range sm.openStateByOther {
		found := 0
		for _, indexed := range sm.openStateByFile[string(os.FileHandle)] {
			if indexed == os {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("open state for fh %q indexed %d times, want 1", os.FileHandle, found)
		}
	}

	// The index must contain no entries that are absent from the authoritative
	// map, and no empty file slots.
	total := 0
	for fh, states := range sm.openStateByFile {
		if len(states) == 0 {
			t.Fatalf("openStateByFile has empty slot for fh %q", fh)
		}
		for _, os := range states {
			if _, ok := sm.openStateByOther[os.Stateid.Other]; !ok {
				t.Fatalf("openStateByFile contains stale open state for fh %q", fh)
			}
			total++
		}
	}
	if total != len(sm.openStateByOther) {
		t.Fatalf("openStateByFile has %d entries, openStateByOther has %d", total, len(sm.openStateByOther))
	}
}

func TestOpenStateByFile_OpenThenCloseRemovesFromIndex(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-open-close")

	stateid := openConfirmed(t, sm, 0, []byte("owner-oc"), fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_NONE)

	if n := fileIndexLen(sm, fh); n != 1 {
		t.Fatalf("after open: index len = %d, want 1", n)
	}
	fileIndexMatchesAuthoritative(t, sm)

	// CLOSE advances the owner seqid; open used 1, confirm used 2, close uses 3.
	if _, err := sm.CloseFile(&stateid, 3); err != nil {
		t.Fatalf("CloseFile: %v", err)
	}

	if n := fileIndexLen(sm, fh); n != 0 {
		t.Fatalf("after close: index len = %d, want 0 (slot must be dropped)", n)
	}
	fileIndexMatchesAuthoritative(t, sm)
}

func TestOpenStateByFile_MultipleOwnersSameFile(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-multi-owner")

	// Two distinct owners with non-conflicting share modes (both READ, no deny).
	sidA := openConfirmed(t, sm, 0, []byte("ownerA"), fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)
	openConfirmed(t, sm, 0, []byte("ownerB"), fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)

	if n := fileIndexLen(sm, fh); n != 2 {
		t.Fatalf("two owners: index len = %d, want 2", n)
	}
	fileIndexMatchesAuthoritative(t, sm)

	// Closing one owner leaves the other indexed.
	if _, err := sm.CloseFile(&sidA, 3); err != nil {
		t.Fatalf("CloseFile(A): %v", err)
	}
	if n := fileIndexLen(sm, fh); n != 1 {
		t.Fatalf("after closing A: index len = %d, want 1", n)
	}
	fileIndexMatchesAuthoritative(t, sm)
}

func TestOpenStateByFile_EvictClearsIndex(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-evict")

	verifier := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.2.8.1"}
	res, err := sm.SetClientID("evict-client", verifier, callback, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	openConfirmed(t, sm, res.ClientID, []byte("owner-evict"), fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_NONE)
	if n := fileIndexLen(sm, fh); n != 1 {
		t.Fatalf("after open: index len = %d, want 1", n)
	}

	if err := sm.EvictV40Client(res.ClientID); err != nil {
		t.Fatalf("EvictV40Client: %v", err)
	}
	if n := fileIndexLen(sm, fh); n != 0 {
		t.Fatalf("after evict: index len = %d, want 0", n)
	}
	fileIndexMatchesAuthoritative(t, sm)
}

func TestOpenStateByFile_FreeStateidRemovesFromIndex(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-free-stateid")

	stateid := openConfirmed(t, sm, 0, []byte("owner-free"), fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_NONE)
	if n := fileIndexLen(sm, fh); n != 1 {
		t.Fatalf("after open: index len = %d, want 1", n)
	}

	sm.mu.Lock()
	err := sm.freeOpenStateidLocked(0, &stateid)
	sm.mu.Unlock()
	if err != nil {
		t.Fatalf("freeOpenStateidLocked: %v", err)
	}

	if n := fileIndexLen(sm, fh); n != 0 {
		t.Fatalf("after free: index len = %d, want 0", n)
	}
	fileIndexMatchesAuthoritative(t, sm)
}

// revokedCount reads the per-client revoked-delegation index directly.
func revokedCount(sm *StateManager, clientID uint64) int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.revokedDelegCount[clientID]
}

func TestRevokedDelegIndex_SetOnRevokeClearedOnReturn(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	const clientID = uint64(7777)
	fh := []byte("fh-revoke-return")

	deleg := sm.GrantDelegation(clientID, fh, types.OPEN_DELEGATE_READ)
	if deleg == nil {
		t.Fatal("GrantDelegation returned nil")
	}
	if c := revokedCount(sm, clientID); c != 0 {
		t.Fatalf("fresh delegation: revoked count = %d, want 0", c)
	}

	session := &Session{ClientID: clientID}
	if sm.GetStatusFlags(session)&types.SEQ4_STATUS_RECALLABLE_STATE_REVOKED != 0 {
		t.Fatal("RECALLABLE_STATE_REVOKED set before any revoke")
	}

	sm.RevokeDelegation(deleg.Stateid.Other)
	if c := revokedCount(sm, clientID); c != 1 {
		t.Fatalf("after revoke: revoked count = %d, want 1", c)
	}
	if sm.GetStatusFlags(session)&types.SEQ4_STATUS_RECALLABLE_STATE_REVOKED == 0 {
		t.Fatal("RECALLABLE_STATE_REVOKED not set after revoke")
	}

	// Revoking again must not double-count (idempotent).
	sm.RevokeDelegation(deleg.Stateid.Other)
	if c := revokedCount(sm, clientID); c != 1 {
		t.Fatalf("after double revoke: revoked count = %d, want 1 (idempotent)", c)
	}

	// Returning the (revoked) delegation frees it from delegByOther and must
	// clear the revoked index entry.
	if err := sm.ReturnDelegation(&deleg.Stateid); err != nil {
		t.Fatalf("ReturnDelegation: %v", err)
	}
	if c := revokedCount(sm, clientID); c != 0 {
		t.Fatalf("after return: revoked count = %d, want 0", c)
	}
	if sm.GetStatusFlags(session)&types.SEQ4_STATUS_RECALLABLE_STATE_REVOKED != 0 {
		t.Fatal("RECALLABLE_STATE_REVOKED still set after returning the revoked delegation")
	}
}

func TestRevokedDelegIndex_ClearedOnEvict(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{2, 2, 2, 2, 2, 2, 2, 2}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.3.8.1"}
	res, err := sm.SetClientID("revoke-evict-client", verifier, callback, "10.0.0.3:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	fh := []byte("fh-revoke-evict")
	deleg := sm.GrantDelegation(res.ClientID, fh, types.OPEN_DELEGATE_READ)
	if deleg == nil {
		t.Fatal("GrantDelegation returned nil")
	}
	sm.RevokeDelegation(deleg.Stateid.Other)
	if c := revokedCount(sm, res.ClientID); c != 1 {
		t.Fatalf("after revoke: revoked count = %d, want 1", c)
	}

	if err := sm.EvictV40Client(res.ClientID); err != nil {
		t.Fatalf("EvictV40Client: %v", err)
	}
	if c := revokedCount(sm, res.ClientID); c != 0 {
		t.Fatalf("after evict: revoked count = %d, want 0 (index entry must be dropped)", c)
	}
}

// TestOwnerCachedKey_MatchesRecomputed verifies the cached owner keys agree
// with the canonical make*Key functions, and that the LockManager owner ID is
// exactly the prefix + lock-owner key (so cached and free-function derivations
// stay in lock-step).
func TestOwnerCachedKey_MatchesRecomputed(t *testing.T) {
	const clientID = uint64(0xABCDEF)
	ownerData := []byte("owner-cached-key")

	oo := &OpenOwner{ClientID: clientID, OwnerData: ownerData, key: makeOwnerKey(clientID, ownerData)}
	if got, want := oo.Key(), makeOwnerKey(clientID, ownerData); got != want {
		t.Fatalf("OpenOwner.Key() = %q, want %q", got, want)
	}
	// Fallback (no cached key) must still match.
	if got, want := (&OpenOwner{ClientID: clientID, OwnerData: ownerData}).Key(), makeOwnerKey(clientID, ownerData); got != want {
		t.Fatalf("OpenOwner.Key() fallback = %q, want %q", got, want)
	}

	lo := &LockOwner{ClientID: clientID, OwnerData: ownerData, key: makeLockOwnerKey(clientID, ownerData)}
	if got, want := lo.Key(), makeLockOwnerKey(clientID, ownerData); got != want {
		t.Fatalf("LockOwner.Key() = %q, want %q", got, want)
	}
	if got, want := lo.LockManagerOwnerID(), lockManagerOwnerID(clientID, ownerData); got != want {
		t.Fatalf("LockOwner.LockManagerOwnerID() = %q, want %q", got, want)
	}
	// Fallback path (literal lock-owner without cached key) must also agree.
	loNoKey := &LockOwner{ClientID: clientID, OwnerData: ownerData}
	if got, want := loNoKey.LockManagerOwnerID(), lockManagerOwnerID(clientID, ownerData); got != want {
		t.Fatalf("LockOwner.LockManagerOwnerID() fallback = %q, want %q", got, want)
	}
}

// TestRevokedDelegIndex_NonRevokedFreeDoesNotUnderflow ensures freeing a
// delegation that was never revoked leaves the index untouched (no spurious
// decrement / negative count).
func TestRevokedDelegIndex_NonRevokedFreeDoesNotUnderflow(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	const clientID = uint64(4242)
	fh := []byte("fh-nonrevoked-free")

	deleg := sm.GrantDelegation(clientID, fh, types.OPEN_DELEGATE_READ)
	if deleg == nil {
		t.Fatal("GrantDelegation returned nil")
	}
	if err := sm.ReturnDelegation(&deleg.Stateid); err != nil {
		t.Fatalf("ReturnDelegation: %v", err)
	}
	if c := revokedCount(sm, clientID); c != 0 {
		t.Fatalf("returning a never-revoked delegation: revoked count = %d, want 0", c)
	}
}
