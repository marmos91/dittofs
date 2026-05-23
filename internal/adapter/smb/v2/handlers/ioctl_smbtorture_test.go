package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildSmbtortureIoctlRequest builds the minimal 24-byte IOCTL request body
// for an FSCTL_SMBTORTURE_* control code. The sentinel FileID (0xFF...) is
// what smbtorture actually sends — these FSCTLs are not bound to an open
// file handle (Samba's `smb2_ioctl_smbtorture` lists them under the
// no-handle group).
func buildSmbtortureIoctlRequest(ctlCode uint32) []byte {
	w := smbenc.NewWriter(24)
	w.WriteUint16(57)
	w.WriteUint16(0)
	w.WriteUint32(ctlCode)
	w.WriteBytes(bytes.Repeat([]byte{0xFF}, 16))
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
