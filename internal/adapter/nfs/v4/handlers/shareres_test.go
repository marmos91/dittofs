package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// readBypassStateidH returns the all-ones READ-bypass special stateid.
func readBypassStateidH() *types.Stateid4 {
	sid := &types.Stateid4{Seqid: 0xFFFFFFFF}
	for i := range sid.Other {
		sid.Other[i] = 0xFF
	}
	return sid
}

// openFileAndGetStateid creates+opens a file via the OPEN handler with the given
// share_access and returns the file handle and the real (confirmed) open stateid.
// It drives OPEN then OPEN_CONFIRM through the handler so the returned stateid is
// usable on subsequent READ/WRITE ops.
func openFileAndGetStateid(t *testing.T, fx *ioTestFixture, owner string, shareAccess uint32) (metadata.FileHandle, *types.Stateid4) {
	t.Helper()

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		shareAccess,
		types.OPEN4_SHARE_DENY_NONE,
		0x1111,
		[]byte(owner),
		types.OPEN4_CREATE,
		types.UNCHECKED4,
		types.CLAIM_NULL,
		owner+".txt",
	)
	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN(%s) status = %d, want NFS4_OK", owner, result.Status)
	}

	// Decode the stateid from the OPEN response.
	reader := bytes.NewReader(result.Data)
	if _, err := xdr.DecodeUint32(reader); err != nil { // status
		t.Fatalf("decode OPEN status: %v", err)
	}
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		t.Fatalf("decode OPEN stateid: %v", err)
	}

	// CurrentFH now points at the opened file.
	fileHandle := make(metadata.FileHandle, len(ctx.CurrentFH))
	copy(fileHandle, ctx.CurrentFH)

	// Confirm the new owner so the stateid seqid advances to a usable value.
	confirmArgs := encodeOpenConfirmArgs(stateid, 2)
	confirmResult := fx.handler.handleOpenConfirm(ctx, bytes.NewReader(confirmArgs))
	if confirmResult.Status != types.NFS4_OK {
		t.Fatalf("OPEN_CONFIRM(%s) status = %d, want NFS4_OK", owner, confirmResult.Status)
	}
	creader := bytes.NewReader(confirmResult.Data)
	if _, err := xdr.DecodeUint32(creader); err != nil { // status
		t.Fatalf("decode OPEN_CONFIRM status: %v", err)
	}
	confirmed, err := types.DecodeStateid4(creader)
	if err != nil {
		t.Fatalf("decode OPEN_CONFIRM stateid: %v", err)
	}

	return fileHandle, confirmed
}

// ============================================================================
// H3 — all-ones (READ-bypass) special stateid on write-family ops
// ============================================================================

func TestWrite_ReadBypassStateid_ReturnsBadStateid(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "wbypass.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeWriteArgs(readBypassStateidH(), 0, types.UNSTABLE4, []byte("nope"))
	result := fx.handler.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("WRITE with all-ones stateid status = %d, want NFS4ERR_BAD_STATEID (%d)",
			result.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestSetAttr_ReadBypassStateid_OnSizeChange_ReturnsBadStateid(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "sbypass.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	var attrVals bytes.Buffer
	_ = xdr.WriteUint64(&attrVals, 0) // truncate to 0
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)

	args := encodeSetAttrArgs(t, readBypassStateidH(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("SETATTR(size) with all-ones stateid status = %d, want NFS4ERR_BAD_STATEID (%d)",
			result.Status, types.NFS4ERR_BAD_STATEID)
	}
}

// encodeSetAttrSizeArgs builds SETATTR4args with a FATTR4_SIZE change to the
// given size using the supplied stateid.
func encodeSetAttrSizeArgs(t *testing.T, stateid *types.Stateid4, size uint64) []byte {
	t.Helper()
	var attrVals bytes.Buffer
	_ = xdr.WriteUint64(&attrVals, size)
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)
	return encodeSetAttrArgs(t, stateid, bitmap, attrVals.Bytes())
}

// TestSetAttr_BogusStateid_OnSizeChange_ReturnsBadStateid verifies a SETATTR
// that changes size with an unknown (never-OPENed) real stateid is rejected by
// ValidateStateid (RFC 7530 Section 16.32), mirroring WRITE.
func TestSetAttr_BogusStateid_OnSizeChange_ReturnsBadStateid(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "bogus.txt", 0o644, 0, 0)
	fx.writeContent(t, fileHandle, []byte("0123456789"))

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	// A non-special stateid that was never issued by the server.
	bogus := &types.Stateid4{Seqid: 1}
	bogus.Other[0] = 0xDE
	bogus.Other[1] = 0xAD

	args := encodeSetAttrSizeArgs(t, bogus, 0)
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))
	// A never-issued stateid is rejected as BAD_STATEID (current epoch) or
	// STALE_STATEID (prior-epoch byte pattern) — either is a valid rejection;
	// the point is the size change is NOT silently accepted.
	if result.Status != types.NFS4ERR_BAD_STATEID && result.Status != types.NFS4ERR_STALE_STATEID {
		t.Errorf("SETATTR(size) with bogus stateid status = %d, want NFS4ERR_BAD_STATEID (%d) or NFS4ERR_STALE_STATEID (%d)",
			result.Status, types.NFS4ERR_BAD_STATEID, types.NFS4ERR_STALE_STATEID)
	}
}

// TestSetAttr_ReadOnlyOpenStateid_OnSizeChange_ReturnsOpenMode verifies a
// SETATTR truncate using a READ-only open's real stateid is rejected with
// NFS4ERR_OPENMODE — the open-mode gate WRITE enforces must also apply to a
// size-changing SETATTR (round-1 H18).
func TestSetAttr_ReadOnlyOpenStateid_OnSizeChange_ReturnsOpenMode(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle, stateid := openFileAndGetStateid(t, fx, "rdonly", types.OPEN4_SHARE_ACCESS_READ)
	fx.writeContent(t, fileHandle, []byte("content"))

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeSetAttrSizeArgs(t, stateid, 0)
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4ERR_OPENMODE {
		t.Errorf("SETATTR(size) with read-only open stateid status = %d, want NFS4ERR_OPENMODE (%d)",
			result.Status, types.NFS4ERR_OPENMODE)
	}
}

// TestSetAttr_WriteOpenStateid_OnSizeChange_Allowed verifies a SETATTR truncate
// with a valid read-write open stateid succeeds (regression guard for the
// stateid validation added for H18 not being over-strict).
func TestSetAttr_WriteOpenStateid_OnSizeChange_Allowed(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle, stateid := openFileAndGetStateid(t, fx, "rdwr", types.OPEN4_SHARE_ACCESS_BOTH)
	fx.writeContent(t, fileHandle, []byte("content"))

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeSetAttrSizeArgs(t, stateid, 0)
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4_OK {
		t.Errorf("SETATTR(size) with read-write open stateid status = %d, want NFS4_OK", result.Status)
	}
}

func TestRead_ReadBypassStateid_Allowed(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "rbypass.txt", 0o644, 0, 0)
	fx.writeContent(t, fileHandle, []byte("readable"))

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeReadArgs(readBypassStateidH(), 0, 1024)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Errorf("READ with all-ones stateid status = %d, want NFS4_OK", result.Status)
	}
}

func TestWrite_AnonymousStateid_Allowed(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	fileHandle := fx.createRegularFile(t, fx.rootHandle, "anon.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeWriteArgs(&anonymousStateid, 0, types.UNSTABLE4, []byte("data"))
	result := fx.handler.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Errorf("WRITE with anonymous stateid status = %d, want NFS4_OK", result.Status)
	}
}

// ============================================================================
// H19 — READ enforces SHARE_ACCESS_READ on the open stateid
// ============================================================================

func TestRead_WriteOnlyOpenStateid_ReturnsOpenMode(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	// Open write-only and grab the real stateid.
	fileHandle, stateid := openFileAndGetStateid(t, fx, "writeonly", types.OPEN4_SHARE_ACCESS_WRITE)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeReadArgs(stateid, 0, 1024)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_OPENMODE {
		t.Errorf("READ with write-only open stateid status = %d, want NFS4ERR_OPENMODE (%d)",
			result.Status, types.NFS4ERR_OPENMODE)
	}
}

func TestRead_ReadWriteOpenStateid_Allowed(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle, stateid := openFileAndGetStateid(t, fx, "readwrite", types.OPEN4_SHARE_ACCESS_BOTH)
	fx.writeContent(t, fileHandle, []byte("content"))

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeReadArgs(stateid, 0, 1024)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Errorf("READ with read-write open stateid status = %d, want NFS4_OK", result.Status)
	}
}

// ============================================================================
// H2 — OPEN decode validation of share_access / share_deny
// ============================================================================

func TestOpen_InvalidShareAccess_ReturnsInval(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)

	// share_access = 0 is illegal.
	args := encodeOpenArgs(
		1, 0, types.OPEN4_SHARE_DENY_NONE,
		0x2222, []byte("badaccess"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, "bad.txt",
	)
	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("OPEN with share_access=0 status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestOpen_InvalidShareDeny_ReturnsInval(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)

	// share_deny with an out-of-range bit (0x04) is illegal.
	args := encodeOpenArgs(
		1, types.OPEN4_SHARE_ACCESS_READ, 0x04,
		0x3333, []byte("baddeny"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, "bad.txt",
	)
	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("OPEN with illegal share_deny status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

// ============================================================================
// H2 — share reservation enforcement end-to-end through the OPEN handler
// ============================================================================

func TestOpen_ShareDenyWrite_BlocksSecondWriterAcrossOwners(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)

	// Owner A creates+opens "shared.txt" WRITE with DENY_WRITE.
	argsA := encodeOpenArgs(
		1, types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_WRITE,
		0x4444, []byte("ownerA"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, "shared.txt",
	)
	resA := fx.handler.handleOpen(ctx, bytes.NewReader(argsA))
	if resA.Status != types.NFS4_OK {
		t.Fatalf("OPEN ownerA status = %d, want NFS4_OK", resA.Status)
	}

	// Owner B opens the same file WRITE — must be refused with SHARE_DENIED.
	setCurrentFH(ctx, fx.rootHandle)
	argsB := encodeOpenArgs(
		1, types.OPEN4_SHARE_ACCESS_WRITE, types.OPEN4_SHARE_DENY_NONE,
		0x5555, []byte("ownerB"),
		types.OPEN4_NOCREATE, 0, types.CLAIM_NULL, "shared.txt",
	)
	resB := fx.handler.handleOpen(ctx, bytes.NewReader(argsB))
	if resB.Status != types.NFS4ERR_SHARE_DENIED {
		t.Errorf("OPEN ownerB status = %d, want NFS4ERR_SHARE_DENIED (%d)",
			resB.Status, types.NFS4ERR_SHARE_DENIED)
	}
}
