package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// encodeGetDirDelegationArgs encodes GET_DIR_DELEGATION4args for testing.
func encodeGetDirDelegationArgs(notifMask uint32) []byte {
	var buf bytes.Buffer
	args := types.GetDirDelegationArgs{
		Signal:            false,
		NotificationTypes: types.Bitmap4{notifMask},
		ChildAttrDelay:    types.NFS4Time{Seconds: 0, Nseconds: 0},
		DirAttrDelay:      types.NFS4Time{Seconds: 0, Nseconds: 0},
		ChildAttrs:        types.Bitmap4{},
		DirAttrs:          types.Bitmap4{},
	}
	_ = args.Encode(&buf)
	return buf.Bytes()
}

// encodePutFHArgs encodes PUTFH args (opaque filehandle) for testing.
func encodePutFHArgs(fh []byte) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteXDROpaque(&buf, fh)
	return buf.Bytes()
}

func TestGetDirDelegation_Success(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Set up a fake directory filehandle via PUTFH
	dirFH := []byte("test-dir-fh-001")

	notifMask := uint32((1 << types.NOTIFY4_ADD_ENTRY) | (1 << types.NOTIFY4_REMOVE_ENTRY))

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	gddArgs := encodeGetDirDelegationArgs(notifMask)

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTFH, data: encodePutFHArgs(dirFH)},
		{opCode: types.OP_GET_DIR_DELEGATION, data: gddArgs},
	}
	data := buildCompoundArgsWithOps([]byte("gdd-ok"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
	}
	_, _ = xdr.DecodeOpaque(reader)           // tag
	numResults, _ := xdr.DecodeUint32(reader) // numResults
	if numResults != 3 {
		t.Fatalf("numResults = %d, want 3 (SEQUENCE + PUTFH + GET_DIR_DELEGATION)", numResults)
	}

	// Skip SEQUENCE result
	_, _ = xdr.DecodeUint32(reader) // SEQUENCE opcode
	var seqRes types.SequenceRes
	_ = seqRes.Decode(reader)

	// Skip PUTFH result
	_, _ = xdr.DecodeUint32(reader) // PUTFH opcode
	putfhStatus, _ := xdr.DecodeUint32(reader)
	if putfhStatus != types.NFS4_OK {
		t.Fatalf("PUTFH status = %d, want NFS4_OK", putfhStatus)
	}

	// Decode GET_DIR_DELEGATION result
	gddOpCode, _ := xdr.DecodeUint32(reader)
	if gddOpCode != types.OP_GET_DIR_DELEGATION {
		t.Errorf("result opcode = %d, want OP_GET_DIR_DELEGATION", gddOpCode)
	}
	var gddRes types.GetDirDelegationRes
	if err := gddRes.Decode(reader); err != nil {
		t.Fatalf("decode GetDirDelegationRes: %v", err)
	}
	if gddRes.Status != types.NFS4_OK {
		t.Errorf("GET_DIR_DELEGATION status = %d, want NFS4_OK", gddRes.Status)
	}
	if gddRes.NonFatalStatus != types.GDD4_OK {
		t.Errorf("NonFatalStatus = %d, want GDD4_OK", gddRes.NonFatalStatus)
	}
	if gddRes.OK == nil {
		t.Fatal("GDD4_OK response is nil")
	}
	if gddRes.OK.Stateid.Seqid != 1 {
		t.Errorf("stateid seqid = %d, want 1", gddRes.OK.Stateid.Seqid)
	}
	// Verify notification types are echoed back
	if len(gddRes.OK.NotificationTypes) == 0 || gddRes.OK.NotificationTypes[0] != notifMask {
		t.Errorf("notification types = %v, want [%d]", gddRes.OK.NotificationTypes, notifMask)
	}
}

func TestGetDirDelegation_Unavail_LimitReached(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Set max delegations to 1 and fill it so next grant hits the limit
	h.StateManager.SetMaxDelegations(1)
	session := h.StateManager.GetSession(sessionID)
	if session == nil {
		t.Fatal("session not found")
	}
	// Grant one delegation to fill the limit
	_, err := h.StateManager.GrantDirDelegation(session.ClientID, []byte("filler-fh"), 0x1)
	if err != nil {
		t.Fatalf("pre-fill GrantDirDelegation error: %v", err)
	}

	dirFH := []byte("test-dir-fh-limit")
	notifMask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	gddArgs := encodeGetDirDelegationArgs(notifMask)

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTFH, data: encodePutFHArgs(dirFH)},
		{opCode: types.OP_GET_DIR_DELEGATION, data: gddArgs},
	}
	data := buildCompoundArgsWithOps([]byte("gdd-limit"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK (GDD4_UNAVAIL is non-fatal)", overallStatus)
	}
	_, _ = xdr.DecodeOpaque(reader)           // tag
	numResults, _ := xdr.DecodeUint32(reader) // numResults
	if numResults != 3 {
		t.Fatalf("numResults = %d, want 3", numResults)
	}

	// Skip SEQUENCE
	_, _ = xdr.DecodeUint32(reader)
	var seqRes types.SequenceRes
	_ = seqRes.Decode(reader)

	// Skip PUTFH
	_, _ = xdr.DecodeUint32(reader)
	_, _ = xdr.DecodeUint32(reader) // putfh status

	// Decode GET_DIR_DELEGATION result
	_, _ = xdr.DecodeUint32(reader) // opcode
	var gddRes types.GetDirDelegationRes
	if err := gddRes.Decode(reader); err != nil {
		t.Fatalf("decode GetDirDelegationRes: %v", err)
	}
	if gddRes.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK", gddRes.Status)
	}
	if gddRes.NonFatalStatus != types.GDD4_UNAVAIL {
		t.Errorf("NonFatalStatus = %d, want GDD4_UNAVAIL", gddRes.NonFatalStatus)
	}
	if gddRes.WillSignalDelegAvail {
		t.Errorf("WillSignalDelegAvail = true, want false")
	}
}

func TestGetDirDelegation_Unavail_Disabled(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Disable delegations
	h.StateManager.SetDelegationsEnabled(false)

	dirFH := []byte("test-dir-fh-disabled")
	notifMask := uint32(1 << types.NOTIFY4_REMOVE_ENTRY)

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	gddArgs := encodeGetDirDelegationArgs(notifMask)

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTFH, data: encodePutFHArgs(dirFH)},
		{opCode: types.OP_GET_DIR_DELEGATION, data: gddArgs},
	}
	data := buildCompoundArgsWithOps([]byte("gdd-disabled"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
	}
	_, _ = xdr.DecodeOpaque(reader)
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 3 {
		t.Fatalf("numResults = %d, want 3", numResults)
	}

	// Skip SEQUENCE + PUTFH
	_, _ = xdr.DecodeUint32(reader)
	var seqRes types.SequenceRes
	_ = seqRes.Decode(reader)
	_, _ = xdr.DecodeUint32(reader)
	_, _ = xdr.DecodeUint32(reader)

	// Decode GET_DIR_DELEGATION result
	_, _ = xdr.DecodeUint32(reader) // opcode
	var gddRes types.GetDirDelegationRes
	if err := gddRes.Decode(reader); err != nil {
		t.Fatalf("decode GetDirDelegationRes: %v", err)
	}
	if gddRes.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK", gddRes.Status)
	}
	if gddRes.NonFatalStatus != types.GDD4_UNAVAIL {
		t.Errorf("NonFatalStatus = %d, want GDD4_UNAVAIL", gddRes.NonFatalStatus)
	}
}

func TestGetDirDelegation_NoFilehandle(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	notifMask := uint32(1 << types.NOTIFY4_ADD_ENTRY)
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	gddArgs := encodeGetDirDelegationArgs(notifMask)

	// No PUTFH -- current FH is nil
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_GET_DIR_DELEGATION, data: gddArgs},
	}
	data := buildCompoundArgsWithOps([]byte("gdd-nofh"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("overall status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			overallStatus, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestGetDirDelegation_BadSession(t *testing.T) {
	h, _ := createTestSession(t)
	ctx := newTestCompoundContext()

	// Use a fake session ID
	var fakeSessionID types.SessionId4
	copy(fakeSessionID[:], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

	dirFH := []byte("test-dir-fh-badsession")
	notifMask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	seqArgs := encodeSequenceArgs(fakeSessionID, 0, 1, 0, false)
	gddArgs := encodeGetDirDelegationArgs(notifMask)

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTFH, data: encodePutFHArgs(dirFH)},
		{opCode: types.OP_GET_DIR_DELEGATION, data: gddArgs},
	}
	data := buildCompoundArgsWithOps([]byte("gdd-badsess"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	// SEQUENCE with bad session should return NFS4ERR_BADSESSION
	if overallStatus != types.NFS4ERR_BADSESSION {
		t.Errorf("overall status = %d, want NFS4ERR_BADSESSION (%d)",
			overallStatus, types.NFS4ERR_BADSESSION)
	}
}

func TestGetDirDelegation_BadXDR(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	dirFH := []byte("test-dir-fh-badxdr")

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTFH, data: encodePutFHArgs(dirFH)},
		{opCode: types.OP_GET_DIR_DELEGATION, data: []byte{0x00, 0x01}}, // truncated
	}
	data := buildCompoundArgsWithOps([]byte("gdd-badxdr"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4ERR_BADXDR {
		t.Errorf("overall status = %d, want NFS4ERR_BADXDR (%d)",
			overallStatus, types.NFS4ERR_BADXDR)
	}
}

func TestDelegReturn_FlushesDirectoryNotifications(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Grant a directory delegation
	dirFH := []byte("test-dir-fh-flush")
	notifMask := uint32((1 << types.NOTIFY4_ADD_ENTRY) | (1 << types.NOTIFY4_REMOVE_ENTRY))

	// Get the client ID from the session
	session := h.StateManager.GetSession(sessionID)
	if session == nil {
		t.Fatal("session not found")
	}

	deleg, err := h.StateManager.GrantDirDelegation(session.ClientID, dirFH, notifMask)
	if err != nil {
		t.Fatalf("GrantDirDelegation error: %v", err)
	}

	// Add pending notifications directly
	deleg.NotifMu.Lock()
	deleg.PendingNotifs = append(deleg.PendingNotifs, state.DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "new-file.txt",
		Cookie:    42,
	})
	deleg.PendingNotifs = append(deleg.PendingNotifs, state.DirNotification{
		Type:      types.NOTIFY4_REMOVE_ENTRY,
		EntryName: "old-file.txt",
		Cookie:    43,
	})
	notifCount := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()

	if notifCount != 2 {
		t.Fatalf("expected 2 pending notifications, got %d", notifCount)
	}

	// DELEGRETURN via COMPOUND
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)

	var delegReturnBuf bytes.Buffer
	types.EncodeStateid4(&delegReturnBuf, &deleg.Stateid)

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTFH, data: encodePutFHArgs(dirFH)},
		{opCode: types.OP_DELEGRETURN, data: delegReturnBuf.Bytes()},
	}
	data := buildCompoundArgsWithOps([]byte("dr-flush"), 1, ops)

	resp, respErr := h.ProcessCompound(ctx, data)
	if respErr != nil {
		t.Fatalf("ProcessCompound error: %v", respErr)
	}

	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
	}

	// Verify the delegation was returned (notifications were flushed)
	// After DELEGRETURN, the delegation should no longer exist
	delegs := h.StateManager.GetDelegationsForFile(dirFH)
	if len(delegs) != 0 {
		t.Errorf("delegation still exists after DELEGRETURN, count = %d", len(delegs))
	}

	// Verify pending notifications were drained (flushed before removal)
	deleg.NotifMu.Lock()
	remainingNotifs := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if remainingNotifs != 0 {
		t.Errorf("pending notifications not flushed: %d remaining", remainingNotifs)
	}
}
