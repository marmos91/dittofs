package state

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// H2 — share reservation conflict enforcement across open-owners
// ============================================================================

// openConfirmed opens fileHandle for (clientID, owner) and confirms the owner so
// subsequent opens by the same owner don't trip the seqid machinery. It returns
// the confirmed stateid.
func openConfirmed(t *testing.T, sm *StateManager, clientID uint64, owner, fileHandle []byte, access, deny uint32) types.Stateid4 {
	t.Helper()
	res, err := sm.OpenFile(clientID, owner, 1, fileHandle, access, deny, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", owner, err)
	}
	confirmed, err := sm.ConfirmOpen(&res.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen(%s): %v", owner, err)
	}
	return confirmed.Stateid
}

func expectShareDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("OpenFile should have returned NFS4ERR_SHARE_DENIED")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected *NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_SHARE_DENIED {
		t.Errorf("status = %d, want NFS4ERR_SHARE_DENIED (%d)", stateErr.Status, types.NFS4ERR_SHARE_DENIED)
	}
}

func TestOpenFile_ShareDeny_BlocksConflictingAccessAcrossOwners(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-deny-write")

	// Owner A opens WRITE and denies WRITE to everyone else.
	openConfirmed(t, sm, 0, []byte("ownerA"), fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_WRITE)

	// Owner B's OPEN requesting WRITE access must be refused.
	_, err := sm.OpenFile(0, []byte("ownerB"), 1, fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	expectShareDenied(t, err)
}

func TestOpenFile_ShareDeny_RequestedDenyExcludesExistingAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-existing-read")

	// Owner A holds a plain READ open with no deny.
	openConfirmed(t, sm, 0, []byte("ownerA"), fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)

	// Owner B tries to open and DENY_READ — its requested deny excludes A's
	// existing read access, so the OPEN must be refused.
	_, err := sm.OpenFile(0, []byte("ownerB"), 1, fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_READ, types.CLAIM_NULL)
	expectShareDenied(t, err)
}

func TestOpenFile_ShareDeny_NonConflictingCombosSucceed(t *testing.T) {
	t.Run("read_deny_none_then_read", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		fh := []byte("fh-ok-read-read")
		openConfirmed(t, sm, 0, []byte("ownerA"), fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)
		// Second reader, no deny — no conflict.
		if _, err := sm.OpenFile(0, []byte("ownerB"), 1, fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); err != nil {
			t.Fatalf("non-conflicting OPEN should succeed: %v", err)
		}
	})

	t.Run("deny_write_then_read", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		fh := []byte("fh-ok-denywrite-read")
		openConfirmed(t, sm, 0, []byte("ownerA"), fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_WRITE)
		// B requests READ only; A denies WRITE, not READ — no conflict.
		if _, err := sm.OpenFile(0, []byte("ownerB"), 1, fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); err != nil {
			t.Fatalf("non-conflicting OPEN should succeed: %v", err)
		}
	})
}

// TestOpenFile_ShareDeny_SameOwnerIsNotExempt pins RFC 7530 Section 9.9, which
// states the rule and then removes the obvious exception from it: "This checking
// of share reservations on OPEN is done with no exception for an existing OPEN
// for the same open-owner."
//
// The check used to skip the requesting owner's own opens, so an owner that had
// denied WRITE to everyone could still open the same file for writing again, and
// a deny mode was only ever enforced against other owners.
func TestOpenFile_ShareDeny_SameOwnerIsNotExempt(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-same-owner")

	// Owner A opens WRITE + DENY_WRITE.
	openConfirmed(t, sm, 0, []byte("ownerA"), fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_WRITE)

	// request.access (WRITE) & file_state.deny (WRITE) is non-zero, so the
	// same owner's second OPEN is refused just as another owner's would be.
	_, err := sm.OpenFile(0, []byte("ownerA"), 3, fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_WRITE, types.CLAIM_NULL)
	if !errors.Is(err, ErrShareDenied) {
		t.Fatalf("same-owner OPEN against its own DENY_WRITE: err = %v, want ErrShareDenied", err)
	}

	// A deny mode the request does not touch is still no conflict: DENY_WRITE
	// says nothing about reading.
	if _, err := sm.OpenFile(0, []byte("ownerA"), 4, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); err != nil {
		t.Fatalf("same-owner READ open against DENY_WRITE: %v", err)
	}
}

func TestOpenFile_ShareDeny_ReleasedAfterClose(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fh := []byte("fh-deny-released")

	// Owner A opens with DENY_WRITE then closes.
	aStateid := openConfirmed(t, sm, 0, []byte("ownerA"), fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_WRITE)
	if _, err := sm.CloseFile(&aStateid, 3); err != nil {
		t.Fatalf("CloseFile: %v", err)
	}

	// With A's reservation gone, owner B may now open WRITE.
	if _, err := sm.OpenFile(0, []byte("ownerB"), 1, fh,
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); err != nil {
		t.Fatalf("OPEN after conflicting open closed should succeed: %v", err)
	}
}

func TestOpenFile_ShareDeny_DifferentFilesDoNotConflict(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	openConfirmed(t, sm, 0, []byte("ownerA"), []byte("fh-file-1"),
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_WRITE)

	// A deny on file-1 must not affect an OPEN of file-2.
	if _, err := sm.OpenFile(0, []byte("ownerB"), 1, []byte("fh-file-2"),
		types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL); err != nil {
		t.Fatalf("OPEN of a different file should succeed: %v", err)
	}
}

// ============================================================================
// H3 — special-stateid op-family gating in ValidateStateid
// ============================================================================

func readBypassStateid() *types.Stateid4 {
	sid := &types.Stateid4{Seqid: 0xFFFFFFFF}
	for i := range sid.Other {
		sid.Other[i] = 0xFF
	}
	return sid
}

func TestValidateStateid_ReadBypass_AllowedOnReadRejectedOnWrite(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// READ: all-ones is permitted (returns nil openState, nil error).
	openState, err := sm.ValidateStateid(readBypassStateid(), nil, StateidOpRead)
	if err != nil {
		t.Fatalf("read-bypass on READ should be allowed: %v", err)
	}
	if openState != nil {
		t.Error("special stateid should return nil openState")
	}

	// WRITE: all-ones MUST be rejected with NFS4ERR_BAD_STATEID.
	_, err = sm.ValidateStateid(readBypassStateid(), nil, StateidOpWrite)
	if err == nil {
		t.Fatal("read-bypass on WRITE should be rejected")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected *NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)", stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestValidateStateid_Anonymous_AllowedOnReadAndWrite(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	anon := &types.Stateid4{Seqid: 0} // all-zeros other

	for _, op := range []StateidOp{StateidOpRead, StateidOpWrite} {
		openState, err := sm.ValidateStateid(anon, nil, op)
		if err != nil {
			t.Fatalf("anonymous stateid (op=%d) should be allowed: %v", op, err)
		}
		if openState != nil {
			t.Errorf("anonymous stateid (op=%d) should return nil openState", op)
		}
	}
}
