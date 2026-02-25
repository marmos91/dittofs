package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// End-to-end stateful lifecycle tests
//
// These test the full COMPOUND chain:
// SETCLIENTID -> SETCLIENTID_CONFIRM -> OPEN -> OPEN_CONFIRM -> WRITE -> READ -> CLOSE
// ============================================================================

func TestStatefulLifecycle_FullCompound(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	// ========================================================================
	// Step 1: SETCLIENTID
	// ========================================================================

	ctx := newRealFSContext(0, 0)

	var scidArgs bytes.Buffer
	scidArgs.Write(make([]byte, 8))                            // client verifier
	_ = xdr.WriteXDRString(&scidArgs, "test-client-lifecycle") // client id string
	_ = xdr.WriteUint32(&scidArgs, 0x40000000)                 // callback program
	_ = xdr.WriteXDRString(&scidArgs, "tcp")                   // callback netid
	_ = xdr.WriteXDRString(&scidArgs, "127.0.0.1.8.1")         // callback addr
	_ = xdr.WriteUint32(&scidArgs, 1)                          // callback_ident

	scidResult := fx.handler.handleSetClientID(ctx, bytes.NewReader(scidArgs.Bytes()))
	if scidResult.Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID status = %d, want NFS4_OK", scidResult.Status)
	}

	// Parse SETCLIENTID response: status + clientid + confirm_verifier
	scidReader := bytes.NewReader(scidResult.Data)
	status, _ := xdr.DecodeUint32(scidReader) // status
	if status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID encoded status = %d", status)
	}
	clientID, _ := xdr.DecodeUint64(scidReader)
	var confirmVerifier [8]byte
	_, _ = scidReader.Read(confirmVerifier[:])

	t.Logf("SETCLIENTID: clientID=%d", clientID)

	// ========================================================================
	// Step 2: SETCLIENTID_CONFIRM
	// ========================================================================

	var confirmArgs bytes.Buffer
	_ = xdr.WriteUint64(&confirmArgs, clientID)
	confirmArgs.Write(confirmVerifier[:])

	confirmResult := fx.handler.handleSetClientIDConfirm(ctx, bytes.NewReader(confirmArgs.Bytes()))
	if confirmResult.Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID_CONFIRM status = %d, want NFS4_OK", confirmResult.Status)
	}

	// ========================================================================
	// Step 3: OPEN (create file)
	// ========================================================================

	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	openArgs := encodeOpenArgs(
		1,                             // seqid
		types.OPEN4_SHARE_ACCESS_BOTH, // share_access
		types.OPEN4_SHARE_DENY_NONE,   // share_deny
		clientID,                      // clientid
		[]byte("lifecycle-owner"),     // owner
		types.OPEN4_CREATE,            // opentype
		types.UNCHECKED4,              // createmode
		types.CLAIM_NULL,              // claim_type
		"lifecycle-test.txt",          // filename
	)

	openResult := fx.handler.handleOpen(ctx, bytes.NewReader(openArgs))
	if openResult.Status != types.NFS4_OK {
		t.Fatalf("OPEN status = %d, want NFS4_OK", openResult.Status)
	}

	// Parse OPEN response
	openReader := bytes.NewReader(openResult.Data)
	_, _ = xdr.DecodeUint32(openReader) // status
	openStateid, err := types.DecodeStateid4(openReader)
	if err != nil {
		t.Fatalf("decode open stateid: %v", err)
	}

	if openStateid.Seqid != 1 {
		t.Errorf("open stateid seqid = %d, want 1", openStateid.Seqid)
	}

	// Read change_info4
	_, _ = xdr.DecodeUint32(openReader) // atomic
	_, _ = xdr.DecodeUint64(openReader) // before
	_, _ = xdr.DecodeUint64(openReader) // after

	// Read rflags
	rflags, _ := xdr.DecodeUint32(openReader)
	if rflags&types.OPEN4_RESULT_CONFIRM == 0 {
		t.Error("new owner should require OPEN_CONFIRM")
	}

	t.Logf("OPEN: stateid_seqid=%d, rflags=0x%x", openStateid.Seqid, rflags)

	// ========================================================================
	// Step 4: OPEN_CONFIRM
	// ========================================================================

	confirmOpenArgs := encodeOpenConfirmArgs(openStateid, 2) // seqid=2
	confirmOpenResult := fx.handler.handleOpenConfirm(ctx, bytes.NewReader(confirmOpenArgs))
	if confirmOpenResult.Status != types.NFS4_OK {
		t.Fatalf("OPEN_CONFIRM status = %d, want NFS4_OK", confirmOpenResult.Status)
	}

	// Parse OPEN_CONFIRM response
	confirmOpenReader := bytes.NewReader(confirmOpenResult.Data)
	_, _ = xdr.DecodeUint32(confirmOpenReader) // status
	confirmedStateid, err := types.DecodeStateid4(confirmOpenReader)
	if err != nil {
		t.Fatalf("decode confirmed stateid: %v", err)
	}

	if confirmedStateid.Seqid != openStateid.Seqid+1 {
		t.Errorf("confirmed stateid seqid = %d, want %d",
			confirmedStateid.Seqid, openStateid.Seqid+1)
	}

	t.Logf("OPEN_CONFIRM: stateid_seqid=%d", confirmedStateid.Seqid)

	// ========================================================================
	// Step 5: WRITE (using confirmed stateid)
	// ========================================================================

	content := []byte("Hello, stateful NFSv4 world!")
	writeArgs := encodeWriteArgs(confirmedStateid, 0, types.UNSTABLE4, content)
	writeResult := fx.handler.handleWrite(ctx, bytes.NewReader(writeArgs))
	if writeResult.Status != types.NFS4_OK {
		t.Fatalf("WRITE status = %d, want NFS4_OK", writeResult.Status)
	}

	// Parse WRITE response
	writeReader := bytes.NewReader(writeResult.Data)
	_, _ = xdr.DecodeUint32(writeReader) // status
	writeCount, _ := xdr.DecodeUint32(writeReader)
	if writeCount != uint32(len(content)) {
		t.Errorf("write count = %d, want %d", writeCount, len(content))
	}

	t.Logf("WRITE: %d bytes written", writeCount)

	// ========================================================================
	// Step 6: READ (using confirmed stateid)
	// ========================================================================

	readArgs := encodeReadArgs(confirmedStateid, 0, 4096)
	readResult := fx.handler.handleRead(ctx, bytes.NewReader(readArgs))
	if readResult.Status != types.NFS4_OK {
		t.Fatalf("READ status = %d, want NFS4_OK", readResult.Status)
	}

	// Parse READ response
	readReader := bytes.NewReader(readResult.Data)
	_, _ = xdr.DecodeUint32(readReader) // status
	eof, _ := xdr.DecodeUint32(readReader)
	readData, _ := xdr.DecodeOpaque(readReader)

	if !bytes.Equal(readData, content) {
		t.Errorf("read data = %q, want %q", string(readData), string(content))
	}
	if eof != 1 {
		t.Errorf("eof = %d, want 1", eof)
	}

	t.Logf("READ: %d bytes, eof=%d", len(readData), eof)

	// ========================================================================
	// Step 7: RENEW
	// ========================================================================

	var renewArgs bytes.Buffer
	_ = xdr.WriteUint64(&renewArgs, clientID)

	renewResult := fx.handler.handleRenew(ctx, bytes.NewReader(renewArgs.Bytes()))
	if renewResult.Status != types.NFS4_OK {
		t.Fatalf("RENEW status = %d, want NFS4_OK", renewResult.Status)
	}

	t.Logf("RENEW: success for clientID=%d", clientID)

	// ========================================================================
	// Step 8: CLOSE (using confirmed stateid)
	// ========================================================================

	closeArgs := encodeCloseArgs(3, confirmedStateid) // seqid=3
	closeResult := fx.handler.handleClose(ctx, bytes.NewReader(closeArgs))
	if closeResult.Status != types.NFS4_OK {
		t.Fatalf("CLOSE status = %d, want NFS4_OK", closeResult.Status)
	}

	// Parse CLOSE response
	closeReader := bytes.NewReader(closeResult.Data)
	_, _ = xdr.DecodeUint32(closeReader) // status
	closedStateid, err := types.DecodeStateid4(closeReader)
	if err != nil {
		t.Fatalf("decode closed stateid: %v", err)
	}

	// Verify zeroed
	if closedStateid.Seqid != 0 {
		t.Errorf("closed seqid = %d, want 0", closedStateid.Seqid)
	}
	for i, b := range closedStateid.Other {
		if b != 0 {
			t.Errorf("closed other[%d] = %d, want 0", i, b)
		}
	}

	t.Logf("CLOSE: returned zeroed stateid")

	// ========================================================================
	// Verify: State is cleaned up
	// ========================================================================

	// Using the closed stateid for READ should now fail (not special, not found)
	readArgs2 := encodeReadArgs(confirmedStateid, 0, 4096)
	readResult2 := fx.handler.handleRead(ctx, bytes.NewReader(readArgs2))

	// Since the state was closed, the stateid should be invalid
	if readResult2.Status == types.NFS4_OK {
		t.Error("READ with closed stateid should fail")
	}

	t.Logf("Post-CLOSE READ: status=%d (expected error)", readResult2.Status)
}

func TestStatefulLifecycle_RenewUnknownClient(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)

	// RENEW with unknown client ID
	var renewArgs bytes.Buffer
	_ = xdr.WriteUint64(&renewArgs, 0xDEADBEEF)

	renewResult := fx.handler.handleRenew(ctx, bytes.NewReader(renewArgs.Bytes()))
	if renewResult.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Errorf("RENEW unknown client status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			renewResult.Status, types.NFS4ERR_STALE_CLIENTID)
	}
}

func TestStatefulLifecycle_OpenSecondFile_NoConfirmNeeded(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	// Setup: SETCLIENTID + CONFIRM
	ctx := newRealFSContext(0, 0)

	var scidArgs bytes.Buffer
	scidArgs.Write(make([]byte, 8))
	_ = xdr.WriteXDRString(&scidArgs, "multi-file-client")
	_ = xdr.WriteUint32(&scidArgs, 0x40000000)
	_ = xdr.WriteXDRString(&scidArgs, "tcp")
	_ = xdr.WriteXDRString(&scidArgs, "127.0.0.1.8.1")
	_ = xdr.WriteUint32(&scidArgs, 1)

	scidResult := fx.handler.handleSetClientID(ctx, bytes.NewReader(scidArgs.Bytes()))
	if scidResult.Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID: %d", scidResult.Status)
	}

	scidReader := bytes.NewReader(scidResult.Data)
	_, _ = xdr.DecodeUint32(scidReader)
	clientID, _ := xdr.DecodeUint64(scidReader)
	var confirmVerifier [8]byte
	_, _ = scidReader.Read(confirmVerifier[:])

	var confirmArgs bytes.Buffer
	_ = xdr.WriteUint64(&confirmArgs, clientID)
	confirmArgs.Write(confirmVerifier[:])
	fx.handler.handleSetClientIDConfirm(ctx, bytes.NewReader(confirmArgs.Bytes()))

	// OPEN first file (new owner -> needs confirmation)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	openArgs1 := encodeOpenArgs(
		1, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE,
		clientID, []byte("owner-multi"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, "file1.txt",
	)
	openResult1 := fx.handler.handleOpen(ctx, bytes.NewReader(openArgs1))
	if openResult1.Status != types.NFS4_OK {
		t.Fatalf("OPEN 1: %d", openResult1.Status)
	}

	openReader1 := bytes.NewReader(openResult1.Data)
	_, _ = xdr.DecodeUint32(openReader1) // status
	stateid1, _ := types.DecodeStateid4(openReader1)
	_, _ = xdr.DecodeUint32(openReader1) // atomic
	_, _ = xdr.DecodeUint64(openReader1) // before
	_, _ = xdr.DecodeUint64(openReader1) // after
	rflags1, _ := xdr.DecodeUint32(openReader1)

	if rflags1&types.OPEN4_RESULT_CONFIRM == 0 {
		t.Error("first OPEN should require CONFIRM")
	}

	// OPEN_CONFIRM for first file
	confirmOpen1 := encodeOpenConfirmArgs(stateid1, 2)
	confirmOpenResult1 := fx.handler.handleOpenConfirm(ctx, bytes.NewReader(confirmOpen1))
	if confirmOpenResult1.Status != types.NFS4_OK {
		t.Fatalf("OPEN_CONFIRM 1: %d", confirmOpenResult1.Status)
	}

	// OPEN second file (same owner, now confirmed -> no CONFIRM needed)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	openArgs2 := encodeOpenArgs(
		3, types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE,
		clientID, []byte("owner-multi"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, "file2.txt",
	)
	openResult2 := fx.handler.handleOpen(ctx, bytes.NewReader(openArgs2))
	if openResult2.Status != types.NFS4_OK {
		t.Fatalf("OPEN 2: %d", openResult2.Status)
	}

	openReader2 := bytes.NewReader(openResult2.Data)
	_, _ = xdr.DecodeUint32(openReader2) // status
	_, _ = types.DecodeStateid4(openReader2)
	_, _ = xdr.DecodeUint32(openReader2) // atomic
	_, _ = xdr.DecodeUint64(openReader2) // before
	_, _ = xdr.DecodeUint64(openReader2) // after
	rflags2, _ := xdr.DecodeUint32(openReader2)

	if rflags2&types.OPEN4_RESULT_CONFIRM != 0 {
		t.Error("second OPEN with confirmed owner should NOT require CONFIRM")
	}
}
