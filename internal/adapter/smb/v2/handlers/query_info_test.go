package handlers

import (
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

		info, err := h.buildFileInfoFromStore(file, openFileStub, types.FileCompressionInformation)
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

		// CompressionFormat should be 0 (COMPRESSION_FORMAT_NONE)
		compFormat := binary.LittleEndian.Uint16(info[8:10])
		if compFormat != 0 {
			t.Errorf("CompressionFormat = %d, want 0", compFormat)
		}

		// Remaining bytes (shifts + reserved) should all be zero
		for i := 10; i < 16; i++ {
			if info[i] != 0 {
				t.Errorf("info[%d] = %d, want 0", i, info[i])
			}
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

		info, err := h.buildFileInfoFromStore(file, openFileStub, types.FileCompressionInformation)
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

		info, err := h.buildFileInfoFromStore(file, openFileStub, types.FileAttributeTagInformation)
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

		info, err := h.buildFileInfoFromStore(file, openFileStub, types.FileAttributeTagInformation)
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

		info, err := h.buildFileInfoFromStore(file, openFileStub, types.FileStreamInformation)
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
		if len(streamName) != len(expectedName) {
			t.Fatalf("StreamName length = %d, want %d", len(streamName), len(expectedName))
		}
		for i := range expectedName {
			if streamName[i] != expectedName[i] {
				t.Errorf("StreamName[%d] = 0x%02x, want 0x%02x", i, streamName[i], expectedName[i])
			}
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

		info, err := h.buildFileInfoFromStore(file, openFileStub, types.FileStreamInformation)
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

		info, err := h.buildFileInfoFromStore(file, openFileStub, types.FileStreamInformation)
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
