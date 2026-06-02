package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestRead_ClampsLengthToMaxReadSize verifies a READ whose Length exceeds the
// MaxReadSize advertised in NEGOTIATE is rejected with STATUS_INVALID_PARAMETER
// (MS-SMB2 3.3.5.13). Without the clamp the request would be bounded only by
// the 64 MB NetBIOS frame cap, letting one READ allocate 64x the advertised
// ceiling. Mirrors the Windows/Samba reject of an over-max READ.
func TestRead_ClampsLengthToMaxReadSize(t *testing.T) {
	h := NewHandler()
	if h.MaxReadSize == 0 {
		t.Fatal("precondition: MaxReadSize should be non-zero after construction")
	}

	fileID := [16]byte{0x11}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/file",
		GrantedAccess: uint32(types.FileReadData),
	})

	// Length one byte over the advertised maximum.
	req := &ReadRequest{
		FileID: fileID,
		Offset: 0,
		Length: h.MaxReadSize + 1,
	}
	resp, err := h.Read(&SMBHandlerContext{}, req)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if resp.Status != types.StatusInvalidParameter {
		t.Fatalf("over-max READ status = 0x%x, want STATUS_INVALID_PARAMETER (0x%x)",
			resp.Status, types.StatusInvalidParameter)
	}
}

// TestRead_AtMaxReadSizeNotRejectedByClamp verifies a READ exactly at
// MaxReadSize passes the clamp (it is rejected later for other reasons in this
// minimal fixture, but NOT with the over-max INVALID_PARAMETER at the clamp
// step). The clamp must use a strict greater-than so the advertised ceiling is
// itself a legal request size.
func TestRead_AtMaxReadSizeNotRejectedByClamp(t *testing.T) {
	h := NewHandler()

	fileID := [16]byte{0x22}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/file",
		GrantedAccess: uint32(types.FileReadData),
	})

	req := &ReadRequest{
		FileID: fileID,
		Offset: 0,
		Length: h.MaxReadSize, // exactly at the advertised ceiling
	}
	resp, err := h.Read(&SMBHandlerContext{}, req)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	// The clamp uses a strict greater-than, so a length == MaxReadSize is
	// admitted past Step 3b and must NOT be rejected with the over-max
	// STATUS_INVALID_PARAMETER. (It fails downstream with a different status in
	// this minimal fixture — no metadata store wired — which is fine; we only
	// assert the clamp did not reject the at-ceiling request.)
	if resp.Status == types.StatusInvalidParameter {
		t.Fatalf("READ at exactly MaxReadSize was rejected with INVALID_PARAMETER; clamp must use strict > (length == ceiling is legal)")
	}
}

// TestWrite_ClampsLengthToMaxWriteSize verifies a WRITE whose payload exceeds
// MaxWriteSize is rejected with STATUS_INVALID_PARAMETER — the symmetric
// contract to the READ clamp (MS-SMB2 3.3.5.13).
func TestWrite_ClampsLengthToMaxWriteSize(t *testing.T) {
	h := NewHandler()
	if h.MaxWriteSize == 0 {
		t.Fatal("precondition: MaxWriteSize should be non-zero after construction")
	}

	fileID := [16]byte{0x33}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/file",
		GrantedAccess: uint32(types.FileWriteData),
	})

	req := &WriteRequest{
		FileID: fileID,
		Offset: 0,
		Length: h.MaxWriteSize + 1,
		Data:   make([]byte, h.MaxWriteSize+1),
	}
	resp, err := h.Write(&SMBHandlerContext{}, req)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if resp.Status != types.StatusInvalidParameter {
		t.Fatalf("over-max WRITE status = 0x%x, want STATUS_INVALID_PARAMETER (0x%x)",
			resp.Status, types.StatusInvalidParameter)
	}
}
