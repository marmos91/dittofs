package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// These tests pin the DesiredAccess-vs-GrantedAccess fix class for COPYCHUNK:
// validateCopyChunkAccess must gate on the effective (post-DACL) GrantedAccess
// mask, not the raw client-requested DesiredAccess.

func TestCopyChunkAccess_SrcDenies_GrantedAccessStripped(t *testing.T) {
	src := &OpenFile{
		DesiredAccess: uint32(types.FileReadData),
		GrantedAccess: 0, // DACL stripped read
	}
	dst := &OpenFile{
		DesiredAccess: uint32(types.FileWriteData),
		GrantedAccess: uint32(types.FileWriteData | types.FileReadData),
	}
	if got := validateCopyChunkAccess(FsctlSrvCopyChunk, src, dst); got != types.StatusAccessDenied {
		t.Fatalf("expected StatusAccessDenied, got 0x%08X", got)
	}
}

func TestCopyChunkAccess_SrcAllows_GrantedAccess(t *testing.T) {
	src := &OpenFile{
		DesiredAccess: 0,
		GrantedAccess: uint32(types.FileReadData),
	}
	dst := &OpenFile{
		DesiredAccess: 0,
		GrantedAccess: uint32(types.FileWriteData | types.FileReadData),
	}
	if got := validateCopyChunkAccess(FsctlSrvCopyChunk, src, dst); got != types.StatusSuccess {
		t.Fatalf("expected StatusSuccess, got 0x%08X", got)
	}
}

func TestCopyChunkAccess_DstWriteDenies_GrantedAccessStripped(t *testing.T) {
	src := &OpenFile{
		DesiredAccess: uint32(types.FileReadData),
		GrantedAccess: uint32(types.FileReadData),
	}
	dst := &OpenFile{
		DesiredAccess: uint32(types.FileWriteData | types.FileReadData),
		GrantedAccess: 0, // DACL stripped write
	}
	if got := validateCopyChunkAccess(FsctlSrvCopyChunk, src, dst); got != types.StatusAccessDenied {
		t.Fatalf("expected StatusAccessDenied, got 0x%08X", got)
	}
}

func TestCopyChunkAccess_DstReadDenies_GrantedAccessStripped_NonWriteVariant(t *testing.T) {
	src := &OpenFile{
		DesiredAccess: uint32(types.FileReadData),
		GrantedAccess: uint32(types.FileReadData),
	}
	dst := &OpenFile{
		DesiredAccess: uint32(types.FileWriteData | types.FileReadData),
		GrantedAccess: uint32(types.FileWriteData), // read stripped from grant
	}
	// FSCTL_SRV_COPYCHUNK (not _WRITE) requires FileReadData on dst as well.
	if got := validateCopyChunkAccess(FsctlSrvCopyChunk, src, dst); got != types.StatusAccessDenied {
		t.Fatalf("expected StatusAccessDenied, got 0x%08X", got)
	}
}
