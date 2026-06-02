package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildSmbtortureIoctlRequest builds a spec-compliant 56-byte SMB2 IOCTL
// request body for an FSCTL_SMBTORTURE_* control code (MS-SMB2 2.2.31).
// StructureSize=57 implies the full 56-byte fixed envelope is present; the
// input/output offsets and counts are zero because these FSCTLs are
// buffer-less. The sentinel FileID (16×0xFF) is what smbtorture actually
// sends — these FSCTLs are not bound to an open file handle (Samba's
// `smb2_ioctl_smbtorture` lists them under the no-handle group).
func buildSmbtortureIoctlRequest(ctlCode uint32) []byte {
	w := smbenc.NewWriter(56)
	w.WriteUint16(57)         // StructureSize
	w.WriteUint16(0)          // Reserved
	w.WriteUint32(ctlCode)    // CtlCode
	w.WriteBytes(allFFFileID) // FileId (sentinel)
	w.WriteUint32(0)          // InputOffset
	w.WriteUint32(0)          // InputCount
	w.WriteUint32(0)          // MaxInputResponse
	w.WriteUint32(0)          // OutputOffset
	w.WriteUint32(0)          // OutputCount
	w.WriteUint32(0)          // MaxOutputResponse
	w.WriteUint32(0)          // Flags
	w.WriteUint32(0)          // Reserved2
	return w.Bytes()
}

// TestIoctl_SmbtortureForceUnackedTimeout_AcceptedAsNoop pins the regression
// for issue #436 (smb2.multichannel.leases.test3).
//
// Without this acceptance smbtorture's `test_block_smb2_transport` returns
// `block_ok = false`, test2 aborts before its `done:` cleanup, leases on
// `lease_break_test{1,2}.dat` leak into test3 and the very first
// `CHECK_VAL(lease_break_info.count, 0)` after test3's `unlink fname1`
// fails (the server legitimately fires a Handle break to the still-live
// session-2 transports). Returning STATUS_SUCCESS for FSCTL 0x83848003
// lets test2 reach `done:` (its later assertions still fail because we
// don't actually stall responses, but the framework cleanup path does
// run) and leaves clean state for test3.
//
// The FSCTL is Samba-private (libcli/smb/smb_constants.h
// FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT). Real Windows clients never
// issue it; production code paths are unaffected.
func TestIoctl_SmbtortureForceUnackedTimeout_AcceptedAsNoop(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	body := buildSmbtortureIoctlRequest(FsctlSmbtortureForceUnackedTimeout)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusSuccess {
		t.Fatalf("FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT must be accepted as no-op (STATUS_SUCCESS); got %v",
			result.Status)
	}

	// Sanity-check the IOCTL response envelope: the wire response must
	// echo the same CtlCode and FileID, with zero-length output. Smbtorture
	// only consumes NTSTATUS, but a malformed body would confuse other
	// clients and is cheap to verify here.
	if len(result.Data) < 48 {
		t.Fatalf("IOCTL response shorter than 48-byte fixed header: got %d", len(result.Data))
	}
	gotCtlCode := smbenc.NewReader(result.Data[4:8]).ReadUint32()
	if gotCtlCode != FsctlSmbtortureForceUnackedTimeout {
		t.Errorf("response CtlCode mismatch: want 0x%08X got 0x%08X",
			FsctlSmbtortureForceUnackedTimeout, gotCtlCode)
	}
}

// buildSmbtortureFspAsyncSleepRequest builds an IOCTL request with a 1-byte
// InputBuffer carrying the millisecond delay (matches Samba's
// `state->in_input.data[0]` semantics). The bound FileID is the caller's
// open-file ID — not the all-0xFF sentinel — because FSP_ASYNC_SLEEP is a
// per-handle FSCTL (only valid against an open file per Samba's
// `state->fsp == NULL` check).
func buildSmbtortureFspAsyncSleepRequest(fileID [16]byte, delayMs uint8) []byte {
	// 56-byte fixed envelope + 1 InputBuffer byte = 57 bytes.
	w := smbenc.NewWriter(57)
	w.WriteUint16(57) // StructureSize
	w.WriteUint16(0)  // Reserved
	w.WriteUint32(FsctlSmbtortureFspAsyncSleep)
	w.WriteBytes(fileID[:])       // FileId
	w.WriteUint32(64 + 56)        // InputOffset (header is 64 bytes before body, then 56-byte envelope)
	w.WriteUint32(1)              // InputCount
	w.WriteUint32(0)              // MaxInputResponse
	w.WriteUint32(0)              // OutputOffset
	w.WriteUint32(0)              // OutputCount
	w.WriteUint32(0)              // MaxOutputResponse
	w.WriteUint32(0)              // Flags
	w.WriteUint32(0)              // Reserved2
	w.WriteBytes([]byte{delayMs}) // InputBuffer
	return w.Bytes()
}

// TestIoctl_SmbtortureFspAsyncSleep_MissingHandle pins the precondition: the
// dispatcher's per-FileID gate must reject FSP_ASYNC_SLEEP for a handle that
// isn't open. (smbtorture bug14769 expects the handle-bound semantics — see
// the constant comment in stub_handlers.go.) When no handle is registered the
// generic IOCTL gate returns FILE_CLOSED before our handler runs.
func TestIoctl_SmbtortureFspAsyncSleep_MissingHandle(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	body := buildSmbtortureFspAsyncSleepRequest([16]byte{1, 2, 3, 4}, 1)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusFileClosed {
		t.Errorf("expected StatusFileClosed (no open handle), got %v", result.Status)
	}
}

// TestIoctl_SmbtortureFspAsyncSleep_RespectsSleepDuration pins the basic
// contract: the FSCTL completes successfully after at least the requested
// delay has elapsed. smbtorture bug14769's CLOSE-vs-IOCTL ordering check is
// covered end-to-end by the conformance suite; isolating that ordering here
// would require synchronising the test against the lazy in-flight tracker
// in a way that introduces its own races.
func TestIoctl_SmbtortureFspAsyncSleep_RespectsSleepDuration(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	fileID := [16]byte{0xAB, 0xCD, 0xEF}
	h.StoreOpenFile(&OpenFile{FileID: fileID, Path: "bug14769"})

	const delayMs uint8 = 30
	body := buildSmbtortureFspAsyncSleepRequest(fileID, delayMs)

	start := time.Now()
	result, err := h.Ioctl(ctx, body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusSuccess {
		t.Fatalf("expected StatusSuccess, got %v", result.Status)
	}
	if min := time.Duration(delayMs) * time.Millisecond; elapsed < min {
		t.Errorf("FSCTL returned too fast: elapsed=%v want >=%v", elapsed, min)
	}
}

// TestIoctl_SmbtortureFspAsyncSleep_BadInputLengthInvalidParameter pins
// Samba's `state->in_input.length != 1` gate. The FSCTL requires exactly one
// input byte; anything else must be INVALID_PARAMETER so smbtorture's
// malformed-request paths exercise the right error code.
func TestIoctl_SmbtortureFspAsyncSleep_BadInputLengthInvalidParameter(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	fileID := [16]byte{0x11, 0x22, 0x33}
	h.StoreOpenFile(&OpenFile{FileID: fileID, Path: "bad-input"})

	// 0-byte InputBuffer (InputCount=0). Envelope is just the fixed 56 bytes.
	body := buildSmbtortureIoctlRequest(FsctlSmbtortureFspAsyncSleep)
	// The buildSmbtortureIoctlRequest helper writes the sentinel FileID; rewrite the FileID slot.
	copy(body[8:24], fileID[:])

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Errorf("expected StatusInvalidParameter for zero-length input, got %v", result.Status)
	}
}

// TestIoctl_SmbtortureForceUnackedTimeout_BypassesFileHandleGate asserts the
// no-handle whitelist accepts the sentinel 0xFF... FileID — without that
// bypass `Ioctl` short-circuits with STATUS_FILE_CLOSED before reaching
// `handleSmbtortureForceUnackedTimeout`, which is exactly what the
// pre-#436 server did (see dittofs.log line "IOCTL file handle not found
// (closed) ctlCode=0x83848003").
func TestIoctl_SmbtortureForceUnackedTimeout_BypassesFileHandleGate(t *testing.T) {
	if !ioctlNoHandleFSCTL(FsctlSmbtortureForceUnackedTimeout) {
		t.Fatal("FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT must be in the no-handle whitelist " +
			"so dispatch doesn't STATUS_FILE_CLOSED on the sentinel FileID")
	}
}
