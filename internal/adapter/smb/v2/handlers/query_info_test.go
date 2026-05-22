package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestFileCompressionInformation(t *testing.T) {
	h := NewHandler()
	openFileStub := &OpenFile{FileName: "test.txt", Path: "test.txt"}

	t.Run("RegularFile", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Size: 65536,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileCompressionInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Must be exactly 16 bytes
		if len(info) != 16 {
			t.Fatalf("info length = %d, want 16", len(info))
		}

		// CompressedFileSize should equal EndOfFile
		compressedSize := binary.LittleEndian.Uint64(info[0:8])
		if compressedSize != 65536 {
			t.Errorf("CompressedFileSize = %d, want 65536", compressedSize)
		}

		// CompressionFormat should be COMPRESSION_FORMAT_NONE (0x0000)
		// for a file that has not been marked compressed via FSCTL_SET_COMPRESSION.
		compFormat := binary.LittleEndian.Uint16(info[8:10])
		if compFormat != 0x0000 {
			t.Errorf("CompressionFormat = %d, want 0 (NONE)", compFormat)
		}

		// Remaining bytes (shifts + reserved) should all be zero
		for i := 10; i < 16; i++ {
			if info[i] != 0 {
				t.Errorf("info[%d] = %d, want 0", i, info[i])
			}
		}
	})

	t.Run("CompressedFile", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Size: 65536,
				Mode: modeDOSCompressed | 0o644,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileCompressionInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(info) != 16 {
			t.Fatalf("info length = %d, want 16", len(info))
		}

		compressedSize := binary.LittleEndian.Uint64(info[0:8])
		if compressedSize != 65536 {
			t.Errorf("CompressedFileSize = %d, want 65536", compressedSize)
		}

		// CompressionFormat should be COMPRESSION_FORMAT_LZNT1 (0x0002)
		compFormat := binary.LittleEndian.Uint16(info[8:10])
		if compFormat != 0x0002 {
			t.Errorf("CompressionFormat = %d, want 2 (LZNT1)", compFormat)
		}
	})

	t.Run("ZeroSizeFile", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Size: 0,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileCompressionInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		compressedSize := binary.LittleEndian.Uint64(info[0:8])
		if compressedSize != 0 {
			t.Errorf("CompressedFileSize = %d, want 0", compressedSize)
		}
	})
}

func TestFileAttributeTagInformation(t *testing.T) {
	h := NewHandler()
	openFileStub := &OpenFile{FileName: "test.txt", Path: "test.txt"}

	t.Run("RegularFile", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Size: 100,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileAttributeTagInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Must be exactly 8 bytes
		if len(info) != 8 {
			t.Fatalf("info length = %d, want 8", len(info))
		}

		// FileAttributes should include FILE_ATTRIBUTE_ARCHIVE for regular files
		attrs := types.FileAttributes(binary.LittleEndian.Uint32(info[0:4]))
		if attrs&types.FileAttributeArchive == 0 {
			t.Errorf("FileAttributes = 0x%x, expected FILE_ATTRIBUTE_ARCHIVE", attrs)
		}

		// ReparseTag should be 0 for non-reparse points
		reparseTag := binary.LittleEndian.Uint32(info[4:8])
		if reparseTag != 0 {
			t.Errorf("ReparseTag = 0x%x, want 0", reparseTag)
		}
	})

	t.Run("Directory", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeDirectory,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileAttributeTagInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		attrs := types.FileAttributes(binary.LittleEndian.Uint32(info[0:4]))
		if attrs&types.FileAttributeDirectory == 0 {
			t.Errorf("FileAttributes = 0x%x, expected FILE_ATTRIBUTE_DIRECTORY", attrs)
		}
	})
}

func TestBuildFileInfoFromStore_FileStreamInformation(t *testing.T) {
	h := NewHandler()

	// OpenFile stub used for tests that don't depend on OpenFile fields.
	openFileStub := &OpenFile{FileName: "test.txt", Path: "test.txt"}

	t.Run("RegularFile", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Size: 12345,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileStreamInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// NextEntryOffset should be 0 (last entry)
		nextEntry := binary.LittleEndian.Uint32(info[0:4])
		if nextEntry != 0 {
			t.Errorf("NextEntryOffset = %d, want 0", nextEntry)
		}

		// StreamNameLength should be 14 bytes ("::$DATA" in UTF-16LE = 7 chars * 2 bytes)
		nameLen := binary.LittleEndian.Uint32(info[4:8])
		if nameLen != 14 {
			t.Errorf("StreamNameLength = %d, want 14", nameLen)
		}

		// StreamSize should match file size
		streamSize := binary.LittleEndian.Uint64(info[8:16])
		if streamSize != 12345 {
			t.Errorf("StreamSize = %d, want 12345", streamSize)
		}

		// StreamAllocationSize should be cluster-aligned
		allocSize := binary.LittleEndian.Uint64(info[16:24])
		expectedAlloc := calculateAllocationSize(12345)
		if allocSize != expectedAlloc {
			t.Errorf("StreamAllocationSize = %d, want %d", allocSize, expectedAlloc)
		}

		// Stream name should be "::$DATA" in UTF-16LE
		expectedName := []byte{':', 0, ':', 0, '$', 0, 'D', 0, 'A', 0, 'T', 0, 'A', 0}
		streamName := info[24:]
		if !bytes.Equal(streamName, expectedName) {
			t.Errorf("StreamName = %x, want %x", streamName, expectedName)
		}

		// Total size: 24 header + 14 name = 38 bytes
		if len(info) != 38 {
			t.Errorf("total info length = %d, want 38", len(info))
		}
	})

	t.Run("Symlink", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeSymlink,
				Size: 10,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileStreamInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// StreamSize should use getSMBSize (MFsymlink size), not raw file.Size
		streamSize := binary.LittleEndian.Uint64(info[8:16])
		smbSize := getSMBSize(&file.FileAttr)
		if streamSize != smbSize {
			t.Errorf("StreamSize = %d, want %d (MFsymlink size)", streamSize, smbSize)
		}

		// StreamAllocationSize should be cluster-aligned MFsymlink size
		allocSize := binary.LittleEndian.Uint64(info[16:24])
		expectedAlloc := calculateAllocationSize(smbSize)
		if allocSize != expectedAlloc {
			t.Errorf("StreamAllocationSize = %d, want %d", allocSize, expectedAlloc)
		}
	})

	t.Run("ZeroSizeFile", func(t *testing.T) {
		file := &metadata.File{
			ID: uuid.New(),
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Size: 0,
			},
		}

		info, err := h.buildFileInfoFromStore(&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}}, file, openFileStub, types.FileStreamInformation)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		streamSize := binary.LittleEndian.Uint64(info[8:16])
		if streamSize != 0 {
			t.Errorf("StreamSize = %d, want 0", streamSize)
		}

		allocSize := binary.LittleEndian.Uint64(info[16:24])
		if allocSize != 0 {
			t.Errorf("StreamAllocationSize = %d, want 0", allocSize)
		}
	})
}

func TestFileInfoClassRequiredAccess(t *testing.T) {
	tests := []struct {
		name     string
		class    types.FileInfoClass
		want     uint32
		wantGate bool
	}{
		// MS-SMB2 §3.3.5.20.1: gated on FILE_READ_ATTRIBUTES.
		{"basic", types.FileBasicInformation, uint32(types.FileReadAttributes), true},
		{"all", types.FileAllInformation, uint32(types.FileReadAttributes), true},
		{"network_open", types.FileNetworkOpenInformation, uint32(types.FileReadAttributes), true},
		{"attribute_tag", types.FileAttributeTagInformation, uint32(types.FileReadAttributes), true},
		// MS-SMB2 §3.3.5.20.1: gated on FILE_READ_EA.
		{"full_ea", 15, uint32(types.FileReadEA), true},
		// Not gated by the spec — must succeed on any open. This is the
		// acls.CREATOR / CHECK_ACCESS_FLAGS case that was previously broken
		// by the blanket FILE_READ_ATTRIBUTES gate.
		{"access_information", types.FileAccessInformation, 0, false},
		{"standard_information", types.FileStandardInformation, 0, false},
		{"internal_information", types.FileInternalInformation, 0, false},
		{"name_information", types.FileNameInformation, 0, false},
		{"position_information", types.FilePositionInformation, 0, false},
		{"mode_information", types.FileModeInformation, 0, false},
		{"alignment_information", types.FileAlignmentInformation, 0, false},
		{"stream_information", types.FileStreamInformation, 0, false},
		{"alternate_name_information", types.FileAlternateNameInformation, 0, false},
		{"normalized_name_information", types.FileNormalizedNameInformation, 0, false},
		{"id_information", types.FileIdInformation, 0, false},
		{"ea_information", types.FileEaInformation, 0, false},
		{"compression_information", types.FileCompressionInformation, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRight, gotGate := fileInfoClassRequiredAccess(tt.class)
			if gotGate != tt.wantGate {
				t.Errorf("fileInfoClassRequiredAccess(%d) gated=%v, want %v", tt.class, gotGate, tt.wantGate)
			}
			if gotRight != tt.want {
				t.Errorf("fileInfoClassRequiredAccess(%d) right=0x%x, want 0x%x", tt.class, gotRight, tt.want)
			}
		})
	}
}

// TestFileAccessInformation_ReturnsGrantedAccess locks the contract added
// in #548: FileAccessInformation MUST report OpenFile.GrantedAccess (the
// DACL-evaluated effective rights), NOT a re-resolved DesiredAccess.
// smbtorture smb2.acls.GENERIC at acls.c:440 fails when this is wrong.
func TestFileAccessInformation_ReturnsGrantedAccess(t *testing.T) {
	h := NewHandler()
	file := &metadata.File{
		ID: uuid.New(),
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Size: 0,
		},
	}
	// Simulate an open made with MAXIMUM_ALLOWED whose DACL only granted
	// the GENERIC_WRITE specific bits + the smbtorture "expected_mask"
	// standard rights. This is the exact 0x00070080 the GENERIC test asks
	// for; the pre-fix code would have returned 0x001F01FF (FILE_ALL_ACCESS).
	const expected uint32 = 0x00070080
	openFile := &OpenFile{
		FileName:      "test.txt",
		Path:          "test.txt",
		DesiredAccess: uint32(types.MaximumAllowed),
		GrantedAccess: expected,
	}
	info, err := h.buildFileInfoFromStore(
		&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}},
		file, openFile, types.FileAccessInformation,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(info) != 4 {
		t.Fatalf("info length = %d, want 4", len(info))
	}
	got := binary.LittleEndian.Uint32(info[0:4])
	if got != expected {
		t.Errorf("FileAccessInformation = 0x%08x, want 0x%08x (GrantedAccess)", got, expected)
	}
}

// TestFileAllInformation_AccessInformationReturnsGrantedAccess locks the
// contract added in #548 for the FileAllInformation embedded
// AccessInformation field. Per MS-FSCC §2.4.2 the AccessInformation block
// sits at fixed offset 76 (Basic 40 + Standard 24 + Internal 8 + EA 4 = 76)
// and MUST report the DACL-evaluated effective rights — i.e. mirror what a
// standalone FileAccessInformation query would return — NOT a re-resolved
// DesiredAccess. Companion to TestFileAccessInformation_ReturnsGrantedAccess.
func TestFileAllInformation_AccessInformationReturnsGrantedAccess(t *testing.T) {
	h := NewHandler()
	file := &metadata.File{
		ID: uuid.New(),
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Size: 0,
		},
	}
	// Mirrors the FileAccessInformation test: open made with MAXIMUM_ALLOWED
	// whose effective DACL grant is 0x00070080, not FILE_ALL_ACCESS.
	const expected uint32 = 0x00070080
	openFile := &OpenFile{
		FileName:      "test.txt",
		Path:          "test.txt",
		DesiredAccess: uint32(types.MaximumAllowed),
		GrantedAccess: expected,
	}
	info, err := h.buildFileInfoFromStore(
		&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{}},
		file, openFile, types.FileAllInformation,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// FileAllInformation fixed prefix is 100 bytes (40+24+8+4+4+8+4+4+4).
	// AccessInformation is the 4-byte field at offset 76.
	if len(info) < 80 {
		t.Fatalf("info length = %d, want >= 80", len(info))
	}
	got := binary.LittleEndian.Uint32(info[76:80])
	if got != expected {
		t.Errorf("FileAllInformation AccessInformation @ offset 76 = 0x%08x, want 0x%08x (GrantedAccess)", got, expected)
	}
}

func TestResolveAccessFlags(t *testing.T) {
	tests := []struct {
		name     string
		input    uint32
		expected uint32
	}{
		{"explicit read attrs", uint32(types.FileReadAttributes), uint32(types.FileReadAttributes)},
		{"generic all", uint32(types.GenericAll), 0x001F01FF},
		{"maximum allowed", uint32(types.MaximumAllowed), 0x001F01FF},
		{"generic read", uint32(types.GenericRead), uint32(types.FileReadData) | uint32(types.FileReadEA) | uint32(types.FileReadAttributes) | uint32(types.ReadControl) | uint32(types.Synchronize)},
		{"generic write", uint32(types.GenericWrite), uint32(types.FileWriteData) | uint32(types.FileAppendData) | uint32(types.FileWriteEA) | uint32(types.FileWriteAttributes) | uint32(types.ReadControl) | uint32(types.Synchronize)},
		{"no generic bits in output", uint32(types.GenericAll) | uint32(types.GenericRead), 0x001F01FF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAccessFlags(tt.input)
			if got != tt.expected {
				t.Errorf("resolveAccessFlags(0x%x) = 0x%x, want 0x%x", tt.input, got, tt.expected)
			}
			// Verify no generic/maximum bits remain
			genericMask := uint32(types.MaximumAllowed) | uint32(types.GenericAll) | uint32(types.GenericRead) | uint32(types.GenericWrite) | uint32(types.GenericExecute)
			if got&genericMask != 0 {
				t.Errorf("resolveAccessFlags(0x%x) still has generic bits: 0x%x", tt.input, got&genericMask)
			}
		})
	}
}
