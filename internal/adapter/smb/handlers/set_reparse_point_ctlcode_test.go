package handlers

import (
	"encoding/hex"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// capturedMacOSSetReparseInput is the byte-for-byte InputBuffer a macOS
// mount_smbfs client sends for `ln -s t.txt l.txt`, taken off a loopback SMB
// session against a DittoFS server. Decoded, it is a REPARSE_DATA_BUFFER
// [MS-FSCC] 2.1.2.4:
//
//	ReparseTag           0xA000000C  IO_REPARSE_TAG_SYMLINK
//	ReparseDataLength    32
//	SubstituteNameOffset 0    SubstituteNameLength 10
//	PrintNameOffset      10   PrintNameLength      10
//	Flags                1    SYMLINK_FLAG_RELATIVE
//	PathBuffer           "t.txt" "t.txt" (UTF-16LE)
//
// It is kept as raw bytes rather than rebuilt with buildSymlinkReparseBuffer so
// the test asserts against what a real client emits, not against our own encoder.
const capturedMacOSSetReparseInput = "0c0000a02000000000000a000a000a000100000074002e0074007800740074002e00740078007400"

// TestFsctlSetReparsePoint_MatchesWireValue pins the control code to the literal
// value, never to the constant. The pre-existing test for this handler builds its
// request with FsctlSetReparsePoint and so passed for as long as the constant was
// wrong (0x000900D4), because request and dispatcher moved together. MS-FSCC 2.3
// gives FSCTL_SET_REPARSE_POINT as 0x900A4, Samba uses 0x000900A4, and a macOS
// client was observed sending 0x000900A4 on the wire.
func TestFsctlSetReparsePoint_MatchesWireValue(t *testing.T) {
	const wire = 0x000900A4
	if FsctlSetReparsePoint != wire {
		t.Errorf("FsctlSetReparsePoint = 0x%08X, want 0x%08X", FsctlSetReparsePoint, uint32(wire))
	}
	// The neighbouring GET code shares the 0x9009x/0x900Ax range and is a likely
	// copy-paste source, so pin that the two are distinct.
	if FsctlGetReparsePoint == FsctlSetReparsePoint {
		t.Error("GET and SET reparse-point control codes must differ")
	}
}

// TestIoctl_SetReparsePoint_DispatchesCapturedMacOSRequest drives the real IOCTL
// dispatch path with the literal control code and a captured client payload. It
// is the end-to-end form the handler never had: every existing test calls
// handleSetReparsePoint directly, so none of them touch the dispatch table, which
// is exactly where the defect lived.
func TestIoctl_SetReparsePoint_DispatchesCapturedMacOSRequest(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)

	input, err := hex.DecodeString(capturedMacOSSetReparseInput)
	if err != nil {
		t.Fatalf("decode captured input: %v", err)
	}

	// Body built with the literal code, not FsctlSetReparsePoint.
	w := smbenc.NewWriter(56 + len(input))
	w.WriteUint16(57) // StructureSize
	w.WriteUint16(0)  // Reserved
	w.WriteUint32(0x000900A4)
	w.WriteBytes(fileID[:])
	w.WriteUint32(120)                // InputOffset (SMB2 header 64 + fixed body 56)
	w.WriteUint32(uint32(len(input))) // InputCount
	w.WriteUint32(0)                  // MaxInputResponse
	w.WriteUint32(0)                  // OutputOffset
	w.WriteUint32(0)                  // OutputCount
	w.WriteUint32(0)                  // MaxOutputResponse
	w.WriteUint32(1)                  // Flags: SMB2_0_IOCTL_IS_FSCTL, as captured
	w.WriteUint32(0)                  // Reserved2

	resp, err := h.Ioctl(smbCtx, append(w.Bytes(), input...))
	if err != nil {
		t.Fatalf("Ioctl: %v", err)
	}
	if resp.Status == types.StatusNotSupported {
		t.Fatal("dispatch returned STATUS_NOT_SUPPORTED: the control code never reached the handler")
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08X, want STATUS_SUCCESS", uint32(resp.Status))
	}

	gotType, gotTarget := symlinkTargetAfter(t, h, smbCtx, rootHandle)
	if gotType != metadata.FileTypeSymlink {
		t.Fatalf("type = %v, want FileTypeSymlink", gotType)
	}
	if gotTarget != "t.txt" {
		t.Fatalf("target = %q, want %q", gotTarget, "t.txt")
	}
}
