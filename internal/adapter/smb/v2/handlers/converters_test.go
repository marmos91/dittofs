package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// =============================================================================
// FileAttrToFileStandardInfo Tests
// =============================================================================

func TestFileAttrToFileStandardInfo_NumberOfLinks(t *testing.T) {
	tests := []struct {
		name     string
		nlink    uint32
		expected uint32
	}{
		{"normal file with one link", 1, 1},
		{"hard linked file with three links", 3, 3},
		{"uninitialized zero defaults to one", 0, 1},
		{"directory with dot and dotdot", 2, 2},
		{"large link count", 100, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attr := &metadata.FileAttr{
				Type:  metadata.FileTypeRegular,
				Nlink: tt.nlink,
			}
			info := FileAttrToFileStandardInfo(attr, false)
			if info.NumberOfLinks != tt.expected {
				t.Errorf("NumberOfLinks = %d, expected %d", info.NumberOfLinks, tt.expected)
			}
		})
	}
}

func TestFileAttrToFileStandardInfo_Directory(t *testing.T) {
	attr := &metadata.FileAttr{
		Type:  metadata.FileTypeDirectory,
		Nlink: 2, // standard . and ..
	}
	info := FileAttrToFileStandardInfo(attr, false)

	if !info.Directory {
		t.Error("Directory should be true for FileTypeDirectory")
	}
	if info.NumberOfLinks != 2 {
		t.Errorf("NumberOfLinks = %d, expected 2 for directory", info.NumberOfLinks)
	}
}

func TestFileAttrToFileStandardInfo_DeletePending(t *testing.T) {
	attr := &metadata.FileAttr{
		Type:  metadata.FileTypeRegular,
		Nlink: 1,
	}

	t.Run("not delete pending", func(t *testing.T) {
		info := FileAttrToFileStandardInfo(attr, false)
		if info.DeletePending {
			t.Error("DeletePending should be false")
		}
	})

	t.Run("delete pending", func(t *testing.T) {
		info := FileAttrToFileStandardInfo(attr, true)
		if !info.DeletePending {
			t.Error("DeletePending should be true")
		}
	})
}

func TestFileAttrToFileStandardInfo_Sizes(t *testing.T) {
	attr := &metadata.FileAttr{
		Type:  metadata.FileTypeRegular,
		Size:  5000,
		Nlink: 1,
	}
	info := FileAttrToFileStandardInfo(attr, false)

	if info.EndOfFile != 5000 {
		t.Errorf("EndOfFile = %d, expected 5000", info.EndOfFile)
	}

	// AllocationSize should be cluster-aligned (4096-byte clusters)
	expectedAlloc := uint64(8192) // 5000 rounded up to next 4096 boundary
	if info.AllocationSize != expectedAlloc {
		t.Errorf("AllocationSize = %d, expected %d", info.AllocationSize, expectedAlloc)
	}
}

// =============================================================================
// FileAttrToSMBAttributes Tests
// =============================================================================

func TestFileAttrToSMBAttributes_RegularFile(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular}
	attrs := FileAttrToSMBAttributes(attr)

	// Regular files should have ARCHIVE attribute
	if attrs&0x20 == 0 { // FileAttributeArchive = 0x20
		t.Error("Regular file should have Archive attribute")
	}
}

func TestFileAttrToSMBAttributes_Directory(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeDirectory}
	attrs := FileAttrToSMBAttributes(attr)

	if attrs&0x10 == 0 { // FileAttributeDirectory = 0x10
		t.Error("Directory should have Directory attribute")
	}
}

// TestSMBModeFromAttrs_OverwriteForcesArchive locks down the contract that
// overwriteFile relies on: per MS-FSA 2.1.5.1.2.1, OVERWRITE/SUPERSEDE always
// sets FILE_ATTRIBUTE_ARCHIVE on the post-overwrite metadata, so the mode
// produced from the request attrs OR'd with ARCHIVE must round-trip to a
// FileAttr whose SMB attrs include ARCHIVE — even when the client only sent
// FILE_ATTRIBUTE_NORMAL (the case smbtorture breaking2/breaking5 exercises).
func TestSMBModeFromAttrs_OverwriteForcesArchive(t *testing.T) {
	cases := []struct {
		name    string
		reqAttr types.FileAttributes
	}{
		{"client sent zero", 0},
		{"client sent NORMAL", types.FileAttributeNormal},
		{"client sent HIDDEN", types.FileAttributeHidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forced := tc.reqAttr | types.FileAttributeArchive
			mode := SMBModeFromAttrs(forced, false)
			attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode}
			got := FileAttrToSMBAttributes(attr)
			if got&types.FileAttributeArchive == 0 {
				t.Errorf("ARCHIVE missing from round-trip attrs 0x%x (mode 0x%x)", got, mode)
			}
		})
	}
}
