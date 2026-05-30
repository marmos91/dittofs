package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// =============================================================================
// effectiveAllocationSize Tests
// =============================================================================

func TestEffectiveAllocationSize(t *testing.T) {
	cases := []struct {
		name      string
		size      uint64
		requested uint64
		want      uint64
	}{
		{"empty file no request", 0, 0, 0},
		// An empty file opened with a requested allocation must report a
		// non-zero, cluster-aligned AllocationSize (smb2.durable-open.alloc-size).
		{"empty file requested 0x1000", 0, 0x1000, 0x1000},
		{"requested rounds up to cluster", 0, 0x1001, 0x2000},
		{"file size exceeds request", 0x3000, 0x1000, 0x3000},
		{"request exceeds file size", 0x1000, 0x5000, 0x5000},
		// A client-controlled requested allocation within clusterSize-1 of the
		// uint64 max must saturate, not wrap to a small value.
		{"requested near-max saturates", 0, ^uint64(0), 0xFFFFFFFFFFFFF000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveAllocationSize(tc.size, tc.requested); got != tc.want {
				t.Errorf("effectiveAllocationSize(%d, %d) = %d, want %d",
					tc.size, tc.requested, got, tc.want)
			}
		})
	}
}

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

func TestFileAttrToSMBAttributes_SystemBitRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeSystem|types.FileAttributeArchive, false)
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	if got&types.FileAttributeSystem == 0 {
		t.Errorf("SYSTEM missing after round-trip: attrs=0x%x mode=0x%x", got, mode)
	}
}

func TestFileAttrToSMBAttributesWithName_HiddenByMetadata(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Hidden: true}
	got := FileAttrToSMBAttributesWithName(attr, "normalname")
	if got&types.FileAttributeHidden == 0 {
		t.Errorf("HIDDEN missing when attr.Hidden=true, attrs=0x%x", got)
	}
}

func TestFileAttrToSMBAttributesWithName_HiddenByDotPrefix(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Hidden: false}
	got := FileAttrToSMBAttributesWithName(attr, ".dotfile")
	if got&types.FileAttributeHidden == 0 {
		t.Errorf("HIDDEN missing for dot-prefix file, attrs=0x%x", got)
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

// TestSMBModeFromAttrs_ReadonlyDoesNotStripWriteBit pins the contract that
// applying READONLY via SET_INFO does NOT mutate POSIX owner-write — the
// READONLY bit is tracked in modeDOSReadonly instead. Stripping owner-write
// would change the mode-synthesized DACL, breaking smb2.winattr
// (source4/torture/smb2/attr.c:365) which captures the DACL before any
// SET_INFO and asserts ACEs are unchanged across attribute round-trips.
func TestSMBModeFromAttrs_ReadonlyDoesNotStripWriteBit(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeReadonly, false)
	if mode&0o200 == 0 {
		t.Errorf("SMBModeFromAttrs(READONLY) stripped owner-write: mode=0o%o (want 0o200 preserved)", mode)
	}
	if mode&modeDOSReadonly == 0 {
		t.Errorf("SMBModeFromAttrs(READONLY) did not set modeDOSReadonly: mode=0x%x", mode)
	}
}

// TestFileAttrToSMBAttributes_ReadonlyRoundTrip verifies SET_INFO -> QUERY_INFO
// round-trip via the high-bit storage path.
func TestFileAttrToSMBAttributes_ReadonlyRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeReadonly, false)
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	if got&types.FileAttributeReadonly == 0 {
		t.Errorf("READONLY missing after round-trip: attrs=0x%x mode=0x%x", got, mode)
	}
	if got&types.FileAttributeArchive != 0 {
		t.Errorf("ARCHIVE leaked into READONLY-only round-trip: attrs=0x%x", got)
	}
}

// TestFileAttrToSMBAttributes_ReadonlyLegacyMode covers files whose POSIX
// owner-write was stripped out-of-band (e.g., NFS chmod or shell chmod with no
// DOS bits set). They must still report READONLY to SMB clients per MS-FSCC
// §2.6, matching Samba dosmode.c::dos_mode_from_sbuf.
func TestFileAttrToSMBAttributes_ReadonlyLegacyMode(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o444}
	got := FileAttrToSMBAttributes(attr)
	if got&types.FileAttributeReadonly == 0 {
		t.Errorf("READONLY missing for owner-write-stripped legacy mode 0o444: attrs=0x%x", got)
	}
}

// TestFileAttrToSMBAttributes_DirectoryArchiveRoundTrip verifies the
// smb2.winattr directory loop (source4/torture/smb2/attr.c:439): SET_INFO of
// ARCHIVE on a directory must round-trip as DIRECTORY|ARCHIVE.
func TestFileAttrToSMBAttributes_DirectoryArchiveRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeArchive, true)
	attr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	want := types.FileAttributeDirectory | types.FileAttributeArchive
	if got != want {
		t.Errorf("DIR+ARCHIVE round-trip: got 0x%x, want 0x%x (mode 0x%x)", got, want, mode)
	}
}

// TestFileAttrToSMBAttributes_DirectoryReadonlyRoundTrip verifies READONLY
// round-trip on directories — same winattr loop, j=2.
func TestFileAttrToSMBAttributes_DirectoryReadonlyRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeReadonly, true)
	attr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	want := types.FileAttributeDirectory | types.FileAttributeReadonly
	if got != want {
		t.Errorf("DIR+READONLY round-trip: got 0x%x, want 0x%x (mode 0x%x)", got, want, mode)
	}
}

// TestFileAttrToSMBAttributes_AllDOSBitsRoundTrip covers the winattr open_attrs
// combinations (ARCHIVE | READONLY | HIDDEN | SYSTEM) — every combination must
// round-trip exactly when stored via SMBModeFromAttrs.
func TestFileAttrToSMBAttributes_AllDOSBitsRoundTrip(t *testing.T) {
	cases := []types.FileAttributes{
		types.FileAttributeArchive,
		types.FileAttributeReadonly,
		types.FileAttributeHidden,
		types.FileAttributeSystem,
		types.FileAttributeArchive | types.FileAttributeReadonly,
		types.FileAttributeArchive | types.FileAttributeHidden,
		types.FileAttributeArchive | types.FileAttributeSystem,
		types.FileAttributeArchive | types.FileAttributeReadonly | types.FileAttributeHidden,
		types.FileAttributeArchive | types.FileAttributeReadonly | types.FileAttributeSystem,
		types.FileAttributeArchive | types.FileAttributeHidden | types.FileAttributeSystem,
		types.FileAttributeReadonly | types.FileAttributeHidden,
		types.FileAttributeReadonly | types.FileAttributeSystem,
		types.FileAttributeReadonly | types.FileAttributeHidden | types.FileAttributeSystem,
	}
	for _, in := range cases {
		mode := SMBModeFromAttrs(in, false)
		attr := &metadata.FileAttr{
			Type:   metadata.FileTypeRegular,
			Mode:   mode,
			Hidden: in&types.FileAttributeHidden != 0,
		}
		got := FileAttrToSMBAttributes(attr)
		if got != in {
			t.Errorf("attrs round-trip: in=0x%x got=0x%x (mode=0x%x)", in, got, mode)
		}
	}
}

// TestFileAttrToFileBasicInfoWithName_DotPrefixHidden ensures the name-aware
// variant marks dot-prefixed files HIDDEN even when attr.Hidden is false.
// Required by smbtorture smb2.dosmode (dosmode.c:158).
func TestFileAttrToFileBasicInfoWithName_DotPrefixHidden(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}
	info := FileAttrToFileBasicInfoWithName(attr, ".dotfile")
	if info.FileAttributes&types.FileAttributeHidden == 0 {
		t.Errorf("dot-prefix file missing HIDDEN: attrs=0x%x", info.FileAttributes)
	}
}
