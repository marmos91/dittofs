package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

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
