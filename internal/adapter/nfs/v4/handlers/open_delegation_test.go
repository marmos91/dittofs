package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// encodeOpenArgsDelegateCur encodes OPEN args for CLAIM_DELEGATE_CUR.
// Wire: seqid shareAccess shareDeny clientID owner openType claimType delegStateid filename
// CLAIM_DELEGATE_CUR always opens an existing file, so openType is OPEN4_NOCREATE
// and there is no createhow4 block.
func encodeOpenArgsDelegateCur(
	seqid, shareAccess, shareDeny uint32,
	clientID uint64, owner []byte,
	openType uint32,
	delegStateid *types.Stateid4,
	filename string,
) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, seqid)
	_ = xdr.WriteUint32(&buf, shareAccess)
	_ = xdr.WriteUint32(&buf, shareDeny)
	_ = xdr.WriteUint64(&buf, clientID)
	_ = xdr.WriteXDROpaque(&buf, owner)
	_ = xdr.WriteUint32(&buf, openType)
	// open_claim4: claim_type
	_ = xdr.WriteUint32(&buf, types.CLAIM_DELEGATE_CUR)
	// delegate_stateid
	types.EncodeStateid4(&buf, delegStateid)
	// filename
	_ = xdr.WriteXDRString(&buf, filename)
	return buf.Bytes()
}

// rflagsFromOpenResult parses the rflags field out of an encoded OPEN4resok.
// Layout: status, stateid4 (seqid + other[12]), change_info4 (atomic + before + after), rflags...
func rflagsFromOpenResult(t *testing.T, data []byte) uint32 {
	t.Helper()
	reader := bytes.NewReader(data)
	_, _ = xdr.DecodeUint32(reader) // status
	if _, err := types.DecodeStateid4(reader); err != nil {
		t.Fatalf("decode stateid: %v", err)
	}
	_, _ = xdr.DecodeUint32(reader) // atomic
	_, _ = xdr.DecodeUint64(reader) // before
	_, _ = xdr.DecodeUint64(reader) // after
	rflags, _ := xdr.DecodeUint32(reader)
	return rflags
}

// ============================================================================
// Bug 1 — CLAIM_NULL delegation-conflict must not orphan a created file
// ============================================================================

// TestOpen_ClaimNull_CreateNew_DelegationConflict_NoOrphan verifies that an
// OPEN4_CREATE that hits a conflicting delegation returns NFS4ERR_DELAY WITHOUT
// creating the file. Before the fix, the file was created first and the conflict
// check ran against the new file's handle, leaving an orphan that turned every
// GUARDED4 retry into NFS4ERR_EXIST.
func TestOpen_ClaimNull_CreateNew_DelegationConflict_NoOrphan(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	// Client A holds a WRITE delegation on an existing file under the same parent.
	const clientA = uint64(0xAAAA)
	const clientB = uint64(0xBBBB)
	existing := fx.createRegularFile(t, fx.rootHandle, "existing.txt", 0o644, 0, 0)
	if d := fx.handler.StateManager.GrantDelegation(clientA, []byte(existing), types.OPEN_DELEGATE_WRITE); d == nil {
		t.Fatalf("GrantDelegation returned nil")
	}
	// Also hold a WRITE delegation registered against the parent directory handle,
	// so a CREATE in this directory observes a conflict for a different client.
	if d := fx.handler.StateManager.GrantDelegation(clientA, []byte(fx.rootHandle), types.OPEN_DELEGATE_WRITE); d == nil {
		t.Fatalf("GrantDelegation (parent) returned nil")
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientB,
		[]byte("owner-b"),
		types.OPEN4_CREATE,
		types.GUARDED4,
		types.CLAIM_NULL,
		"newfile.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4ERR_DELAY {
		t.Fatalf("first OPEN status = %d, want NFS4ERR_DELAY (%d)", result.Status, types.NFS4ERR_DELAY)
	}

	// The file must NOT exist after the conflict — no orphan.
	authCtx := newTestAuthCtx(0, 0)
	if _, lookupErr := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "newfile.txt"); lookupErr == nil {
		t.Fatalf("newfile.txt was orphaned: it exists after a conflicting OPEN4_CREATE")
	}

	// A retry must still return NFS4ERR_DELAY, not NFS4ERR_EXIST.
	ctx2 := newRealFSContext(0, 0)
	ctx2.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx2.CurrentFH, fx.rootHandle)
	retry := fx.handler.handleOpen(ctx2, bytes.NewReader(args))
	if retry.Status != types.NFS4ERR_DELAY {
		t.Fatalf("retry OPEN status = %d, want NFS4ERR_DELAY (%d)", retry.Status, types.NFS4ERR_DELAY)
	}
}

// TestOpen_ClaimNull_OpenExisting_DelegationConflict_ReturnsDelay guards the
// NOCREATE sub-path: opening an existing file under a conflicting delegation
// must still return NFS4ERR_DELAY after the refactor.
func TestOpen_ClaimNull_OpenExisting_DelegationConflict_ReturnsDelay(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	const clientA = uint64(0xAAAA)
	const clientB = uint64(0xBBBB)
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "existing.txt", 0o644, 0, 0)
	if d := fx.handler.StateManager.GrantDelegation(clientA, []byte(fileHandle), types.OPEN_DELEGATE_WRITE); d == nil {
		t.Fatalf("GrantDelegation returned nil")
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientB,
		[]byte("owner-b"),
		types.OPEN4_NOCREATE,
		0,
		types.CLAIM_NULL,
		"existing.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4ERR_DELAY {
		t.Fatalf("OPEN NOCREATE conflict status = %d, want NFS4ERR_DELAY (%d)", result.Status, types.NFS4ERR_DELAY)
	}
}

// TestOpen_ClaimNull_CreateUnchecked_ExistingFile_DelegationConflict_ReturnsDelay
// guards the OPEN4_CREATE + UNCHECKED4 "open existing file" sub-path. When the
// target already exists, UNCHECKED4 degenerates to an open of the existing file
// (lookupErr == nil), so a conflicting delegation held by another client MUST
// trigger a recall and return NFS4ERR_DELAY. The pre-creation conflict check
// only runs against the parent directory and does not cover this case; without
// the per-file check on the existing-file branch, this OPEN would silently
// succeed and bypass the held delegation.
func TestOpen_ClaimNull_CreateUnchecked_ExistingFile_DelegationConflict_ReturnsDelay(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	const clientA = uint64(0xAAAA)
	const clientB = uint64(0xBBBB)

	// Client A holds a WRITE delegation on the existing file itself. Note that
	// NO delegation is registered against the parent directory handle, so the
	// pre-creation parent-handle check cannot fire — the conflict can only be
	// detected on the existing-file branch.
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "existing.txt", 0o644, 0, 0)
	if d := fx.handler.StateManager.GrantDelegation(clientA, []byte(fileHandle), types.OPEN_DELEGATE_WRITE); d == nil {
		t.Fatalf("GrantDelegation returned nil")
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	// OPEN4_CREATE + UNCHECKED4 against the name that already exists: the handler
	// takes the lookupErr == nil branch (open existing), not the create branch.
	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientB,
		[]byte("owner-b"),
		types.OPEN4_CREATE,
		types.UNCHECKED4,
		types.CLAIM_NULL,
		"existing.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4ERR_DELAY {
		t.Fatalf("OPEN CREATE/UNCHECKED4 existing-file conflict status = %d, want NFS4ERR_DELAY (%d)",
			result.Status, types.NFS4ERR_DELAY)
	}
}

// TestOpen_ClaimNull_CreateNew_NoDelegationConflict_Succeeds is a regression
// guard ensuring the pre-creation conflict check is a no-op when no conflicting
// delegation exists.
func TestOpen_ClaimNull_CreateNew_NoDelegationConflict_Succeeds(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		0xCCCC,
		[]byte("owner-c"),
		types.OPEN4_CREATE,
		types.UNCHECKED4,
		types.CLAIM_NULL,
		"created.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN CREATE status = %d, want NFS4_OK", result.Status)
	}

	authCtx := newTestAuthCtx(0, 0)
	if _, lookupErr := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "created.txt"); lookupErr != nil {
		t.Fatalf("created.txt should exist after a successful OPEN CREATE: %v", lookupErr)
	}
}

// ============================================================================
// Bug 2 — CLAIM_DELEGATE_CUR v4.1 auto-confirm
// ============================================================================

// TestOpenClaimDelegateCur_V41_AutoConfirm verifies that on a v4.1 session
// (SkipOwnerSeqid=true) the CLAIM_DELEGATE_CUR path strips OPEN4_RESULT_CONFIRM
// from the reply and auto-confirms the open-owner. A v4.1 client cannot issue
// OPEN_CONFIRM, so without the guard the owner would stay unconfirmed forever.
func TestOpenClaimDelegateCur_V41_AutoConfirm(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	const clientA = uint64(0xA11CE)
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "deleg.txt", 0o644, 0, 0)
	deleg := fx.handler.StateManager.GrantDelegation(clientA, []byte(fileHandle), types.OPEN_DELEGATE_WRITE)
	if deleg == nil {
		t.Fatalf("GrantDelegation returned nil")
	}

	ctx := newRealFSContext(0, 0)
	ctx.SkipOwnerSeqid = true // v4.1 session
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	owner := []byte("owner-deleg")
	args := encodeOpenArgsDelegateCur(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientA,
		owner,
		types.OPEN4_NOCREATE,
		&deleg.Stateid,
		"deleg.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN CLAIM_DELEGATE_CUR status = %d, want NFS4_OK", result.Status)
	}

	// The CONFIRM flag must NOT be set in a reply to a v4.1 client.
	rflags := rflagsFromOpenResult(t, result.Data)
	if rflags&types.OPEN4_RESULT_CONFIRM != 0 {
		t.Fatalf("rflags = %#x, OPEN4_RESULT_CONFIRM must not be set for a v4.1 client", rflags)
	}

	// Prove the owner is confirmed: a second OPEN by the same owner must not
	// request confirmation again. An unconfirmed owner would re-set the flag.
	ctx2 := newRealFSContext(0, 0)
	ctx2.SkipOwnerSeqid = true
	ctx2.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx2.CurrentFH, fx.rootHandle)
	args2 := encodeOpenArgsDelegateCur(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientA,
		owner,
		types.OPEN4_NOCREATE,
		&deleg.Stateid,
		"deleg.txt",
	)
	result2 := fx.handler.handleOpen(ctx2, bytes.NewReader(args2))
	if result2.Status != types.NFS4_OK {
		t.Fatalf("second OPEN status = %d, want NFS4_OK", result2.Status)
	}
	rflags2 := rflagsFromOpenResult(t, result2.Data)
	if rflags2&types.OPEN4_RESULT_CONFIRM != 0 {
		t.Fatalf("second OPEN rflags = %#x: owner was not confirmed by the first CLAIM_DELEGATE_CUR", rflags2)
	}
}

// TestOpenClaimDelegateCur_V40_ConfirmFlagPreserved is a regression guard: on a
// v4.0 session the CONFIRM flag must still be set for a new owner so the client
// performs OPEN_CONFIRM.
func TestOpenClaimDelegateCur_V40_ConfirmFlagPreserved(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	const clientA = uint64(0xB0B)
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "deleg40.txt", 0o644, 0, 0)
	deleg := fx.handler.StateManager.GrantDelegation(clientA, []byte(fileHandle), types.OPEN_DELEGATE_WRITE)
	if deleg == nil {
		t.Fatalf("GrantDelegation returned nil")
	}

	ctx := newRealFSContext(0, 0)
	ctx.SkipOwnerSeqid = false // v4.0 session
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgsDelegateCur(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientA,
		[]byte("owner-deleg40"),
		types.OPEN4_NOCREATE,
		&deleg.Stateid,
		"deleg40.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN CLAIM_DELEGATE_CUR (v4.0) status = %d, want NFS4_OK", result.Status)
	}

	rflags := rflagsFromOpenResult(t, result.Data)
	if rflags&types.OPEN4_RESULT_CONFIRM == 0 {
		t.Fatalf("rflags = %#x, OPEN4_RESULT_CONFIRM must be set for a new v4.0 owner", rflags)
	}
}

// ============================================================================
// Bug 3 — CLAIM_DELEGATE_CUR security and grace-period handling (#1127)
// ============================================================================

// TestOpenClaimDelegateCur_DuringGrace_Succeeds verifies that CLAIM_DELEGATE_CUR
// is honored during the grace period (RFC 7530 Section 8.4.2): a client that
// held a delegation before restart must be able to reclaim via this path. Before
// the fix the handler passed CLAIM_NULL to OpenFile, which the grace gate
// rejected with NFS4ERR_GRACE.
func TestOpenClaimDelegateCur_DuringGrace_Succeeds(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	const clientA = uint64(0xC0FFEE)
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "grace.txt", 0o644, 0, 0)
	deleg := fx.handler.StateManager.GrantDelegation(clientA, []byte(fileHandle), types.OPEN_DELEGATE_WRITE)
	if deleg == nil {
		t.Fatalf("GrantDelegation returned nil")
	}

	// Put the server into grace, as it would be just after a restart.
	fx.handler.StateManager.StartGracePeriod([]uint64{clientA})
	if !fx.handler.StateManager.IsInGrace() {
		t.Fatalf("expected server to be in grace")
	}

	ctx := newRealFSContext(0, 0)
	ctx.SkipOwnerSeqid = true // v4.1 session
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgsDelegateCur(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientA,
		[]byte("owner-grace"),
		types.OPEN4_NOCREATE,
		&deleg.Stateid,
		"grace.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN CLAIM_DELEGATE_CUR during grace status = %d, want NFS4_OK", result.Status)
	}
}

// TestOpenClaimDelegateCur_ForeignDelegation_Rejected verifies that a client
// cannot use a delegation stateid owned by a different client. Before the fix
// the delegation's ClientID was discarded and never compared to the OPEN arg's
// clientID, letting any client open the file under a foreign delegation.
func TestOpenClaimDelegateCur_ForeignDelegation_Rejected(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	const ownerClient = uint64(0xA11CE)
	const attackerClient = uint64(0xBADBAD)
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "foreign.txt", 0o644, 0, 0)
	deleg := fx.handler.StateManager.GrantDelegation(ownerClient, []byte(fileHandle), types.OPEN_DELEGATE_WRITE)
	if deleg == nil {
		t.Fatalf("GrantDelegation returned nil")
	}

	ctx := newRealFSContext(0, 0)
	ctx.SkipOwnerSeqid = true
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	// Attacker presents the victim's delegation stateid with its OWN clientID.
	args := encodeOpenArgsDelegateCur(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		attackerClient,
		[]byte("attacker-owner"),
		types.OPEN4_NOCREATE,
		&deleg.Stateid,
		"foreign.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4ERR_BAD_STATEID {
		t.Fatalf("OPEN CLAIM_DELEGATE_CUR with foreign delegation status = %d, want NFS4ERR_BAD_STATEID", result.Status)
	}
}

// TestOpenClaimDelegateCur_EnforcesFilePermissions verifies that the path runs
// the same POSIX permission check as the CLAIM_NULL paths. A non-root client
// opening a file it has no write access to must be denied even with a valid,
// self-owned delegation stateid. Before the fix checkOpenAccess was never
// called on this path.
func TestOpenClaimDelegateCur_EnforcesFilePermissions(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	const clientA = uint64(0xD00D)
	// File owned by uid 1000, mode 0600 (rw for owner only).
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "secret.txt", 0o600, 1000, 1000)
	deleg := fx.handler.StateManager.GrantDelegation(clientA, []byte(fileHandle), types.OPEN_DELEGATE_WRITE)
	if deleg == nil {
		t.Fatalf("GrantDelegation returned nil")
	}

	// Requester is uid 2000 — no access to a 0600 file owned by uid 1000.
	ctx := newRealFSContext(2000, 2000)
	ctx.SkipOwnerSeqid = true
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgsDelegateCur(
		1,
		types.OPEN4_SHARE_ACCESS_WRITE,
		types.OPEN4_SHARE_DENY_NONE,
		clientA,
		[]byte("owner-noperm"),
		types.OPEN4_NOCREATE,
		&deleg.Stateid,
		"secret.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status == types.NFS4_OK {
		t.Fatalf("OPEN CLAIM_DELEGATE_CUR with no file permission succeeded; want access error")
	}
	if result.Status != types.NFS4ERR_ACCESS {
		t.Fatalf("OPEN CLAIM_DELEGATE_CUR status = %d, want NFS4ERR_ACCESS", result.Status)
	}
}
