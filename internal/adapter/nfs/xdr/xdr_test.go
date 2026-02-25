package xdr

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Helper Functions
// ============================================================================

func validFileAttr() *types.NFSFileAttr {
	now := time.Now()
	return &types.NFSFileAttr{
		Type:   types.NF3REG,
		Mode:   0644,
		Nlink:  1,
		UID:    1000,
		GID:    1000,
		Size:   1024,
		Used:   4096,
		Rdev:   types.SpecData{Major: 0, Minor: 0},
		Fsid:   1,
		Fileid: 12345,
		Atime:  types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
		Mtime:  types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
		Ctime:  types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
	}
}

func validDirAttr() *types.NFSFileAttr {
	now := time.Now()
	return &types.NFSFileAttr{
		Type:   types.NF3DIR,
		Mode:   0755,
		Nlink:  2,
		UID:    1000,
		GID:    1000,
		Size:   4096,
		Used:   4096,
		Rdev:   types.SpecData{Major: 0, Minor: 0},
		Fsid:   1,
		Fileid: 54321,
		Atime:  types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
		Mtime:  types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
		Ctime:  types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
	}
}

func validWccAttr() *types.WccAttr {
	now := time.Now()
	return &types.WccAttr{
		Size:  1024,
		Mtime: types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
		Ctime: types.TimeVal{Seconds: uint32(now.Unix()), Nseconds: uint32(now.Nanosecond())},
	}
}

// ============================================================================
// EncodeOptionalOpaque Tests
// ============================================================================

func TestEncodeOptionalOpaque(t *testing.T) {
	t.Run("EncodesEmptyAsNotPresent", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := EncodeOptionalOpaque(buf, []byte{})
		require.NoError(t, err)
		assert.Equal(t, []byte{0, 0, 0, 0}, buf.Bytes())
	})

	t.Run("EncodesNilAsNotPresent", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := EncodeOptionalOpaque(buf, nil)
		require.NoError(t, err)
		assert.Equal(t, []byte{0, 0, 0, 0}, buf.Bytes())
	})

	t.Run("EncodesNonEmptyWithLength", func(t *testing.T) {
		buf := new(bytes.Buffer)
		data := []byte{0x01, 0x02, 0x03, 0x04}
		err := EncodeOptionalOpaque(buf, data)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 1, // present flag
			0, 0, 0, 4, // length
			0x01, 0x02, 0x03, 0x04, // data
		}
		assert.Equal(t, expected, buf.Bytes())
	})

	t.Run("EncodesWithProperPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		data := []byte{0x01, 0x02, 0x03} // 3 bytes, needs 1 byte padding
		err := EncodeOptionalOpaque(buf, data)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 1, // present flag
			0, 0, 0, 3, // length
			0x01, 0x02, 0x03, 0, // data + 1 byte padding
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})
}

// ============================================================================
// EncodeOptionalFileAttr Tests
// ============================================================================

func TestEncodeOptionalFileAttr(t *testing.T) {
	t.Run("EncodesNilAsNotPresent", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := EncodeOptionalFileAttr(buf, nil)
		require.NoError(t, err)
		assert.Equal(t, []byte{0, 0, 0, 0}, buf.Bytes())
	})

	t.Run("EncodesValidAttrAsPresent", func(t *testing.T) {
		buf := new(bytes.Buffer)
		attr := validFileAttr()
		err := EncodeOptionalFileAttr(buf, attr)
		require.NoError(t, err)

		assert.Equal(t, uint32(1), binary.BigEndian.Uint32(buf.Bytes()[0:4]))
		assert.Greater(t, len(buf.Bytes()), 4)
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned")
	})

	t.Run("EncodesDirectoryAttr", func(t *testing.T) {
		buf := new(bytes.Buffer)
		attr := validDirAttr()
		err := EncodeOptionalFileAttr(buf, attr)
		require.NoError(t, err)

		assert.Equal(t, uint32(1), binary.BigEndian.Uint32(buf.Bytes()[0:4]))
		assert.Equal(t, uint32(types.NF3DIR), binary.BigEndian.Uint32(buf.Bytes()[4:8]))
	})
}

// ============================================================================
// EncodeWccData Tests
// ============================================================================

func TestEncodeWccData(t *testing.T) {
	t.Run("EncodesWithoutBeforeAttr", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := EncodeWccData(buf, nil, validFileAttr())
		require.NoError(t, err)
		assert.Equal(t, uint32(0), binary.BigEndian.Uint32(buf.Bytes()[0:4]))
	})

	t.Run("EncodesWithoutAfterAttr", func(t *testing.T) {
		buf := new(bytes.Buffer)
		before := validWccAttr()
		err := EncodeWccData(buf, before, nil)
		require.NoError(t, err)
		assert.Equal(t, uint32(1), binary.BigEndian.Uint32(buf.Bytes()[0:4]))
	})

	t.Run("EncodesWithBothAttrs", func(t *testing.T) {
		buf := new(bytes.Buffer)
		before := validWccAttr()
		after := validFileAttr()
		err := EncodeWccData(buf, before, after)
		require.NoError(t, err)

		assert.Equal(t, uint32(1), binary.BigEndian.Uint32(buf.Bytes()[0:4]))
		assert.Greater(t, len(buf.Bytes()), 50)
	})
}

// ============================================================================
// ExtractFileID Tests
// ============================================================================

func TestExtractFileID(t *testing.T) {
	t.Run("ExtractsFileIDFromShareHandle", func(t *testing.T) {
		// Create a share-aware handle with a UUID
		id := uuid.New()
		handle, _ := metadata.EncodeShareHandle("export", id)
		fileID := ExtractFileID(handle)

		// File ID should be non-zero and deterministic
		assert.NotEqual(t, uint64(0), fileID)

		// Same handle should always produce same file ID
		fileID2 := ExtractFileID(handle)
		assert.Equal(t, fileID, fileID2)
	})

	t.Run("DifferentHandlesProduceDifferentIDs", func(t *testing.T) {
		id1 := uuid.New()
		id2 := uuid.New()
		handle1, _ := metadata.EncodeShareHandle("export", id1)
		handle2, _ := metadata.EncodeShareHandle("export", id2)

		fileID1 := ExtractFileID(handle1)
		fileID2 := ExtractFileID(handle2)

		assert.NotEqual(t, fileID1, fileID2)
	})

	t.Run("SameUUIDDifferentSharesProduceDifferentIDs", func(t *testing.T) {
		// Use the same UUID but different shares
		id := uuid.New()
		handle1, _ := metadata.EncodeShareHandle("share1", id)
		handle2, _ := metadata.EncodeShareHandle("share2", id)

		fileID1 := ExtractFileID(handle1)
		fileID2 := ExtractFileID(handle2)

		assert.NotEqual(t, fileID1, fileID2)
	})

	t.Run("ReturnsZeroForEmptyHandle", func(t *testing.T) {
		handle := metadata.FileHandle([]byte{})
		fileID := ExtractFileID(handle)
		assert.Equal(t, uint64(0), fileID)
	})

	t.Run("HandlesDifferentUUIDs", func(t *testing.T) {
		// Different UUIDs should produce different file IDs
		id1 := uuid.New()
		handle1, _ := metadata.EncodeShareHandle("export", id1)
		fileID1 := ExtractFileID(handle1)
		assert.NotEqual(t, uint64(0), fileID1)

		id2 := uuid.New()
		handle2, _ := metadata.EncodeShareHandle("export", id2)
		fileID2 := ExtractFileID(handle2)
		assert.NotEqual(t, uint64(0), fileID2)

		assert.NotEqual(t, fileID1, fileID2)
	})
}

// ============================================================================
// DecodeOpaque Tests
// ============================================================================

func TestDecodeOpaque(t *testing.T) {
	t.Run("DecodesEmptyOpaque", func(t *testing.T) {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.BigEndian, uint32(0))

		data, err := DecodeOpaque(buf)
		require.NoError(t, err)
		assert.Empty(t, data)
	})

	t.Run("DecodesOpaqueWithoutPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.BigEndian, uint32(4))
		_, _ = buf.Write([]byte{0x01, 0x02, 0x03, 0x04})

		data, err := DecodeOpaque(buf)
		require.NoError(t, err)
		assert.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, data)
	})

	t.Run("DecodesOpaqueWithPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.BigEndian, uint32(3))
		_, _ = buf.Write([]byte{0x01, 0x02, 0x03, 0x00})

		data, err := DecodeOpaque(buf)
		require.NoError(t, err)
		assert.Equal(t, []byte{0x01, 0x02, 0x03}, data)
	})

	t.Run("RejectsExcessiveLength", func(t *testing.T) {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.BigEndian, uint32(2*1024*1024))

		_, err := DecodeOpaque(buf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds maximum")
	})
}

// ============================================================================
// DecodeString Tests
// ============================================================================

func TestDecodeString(t *testing.T) {
	t.Run("DecodesEmptyString", func(t *testing.T) {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.BigEndian, uint32(0))

		str, err := DecodeString(buf)
		require.NoError(t, err)
		assert.Empty(t, str)
	})

	t.Run("DecodesSimpleString", func(t *testing.T) {
		buf := new(bytes.Buffer)
		testStr := "hello"
		_ = binary.Write(buf, binary.BigEndian, uint32(len(testStr)))
		_, _ = buf.WriteString(testStr)
		_, _ = buf.Write([]byte{0, 0, 0}) // padding

		str, err := DecodeString(buf)
		require.NoError(t, err)
		assert.Equal(t, "hello", str)
	})
}

// ============================================================================
// WriteXDROpaque Tests
// ============================================================================

func TestWriteXDROpaque(t *testing.T) {
	t.Run("EncodesEmptyOpaque", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := WriteXDROpaque(buf, []byte{})
		require.NoError(t, err)

		expected := []byte{0, 0, 0, 0} // length = 0
		assert.Equal(t, expected, buf.Bytes())
	})

	t.Run("EncodesOpaqueWithoutPaddingNeeded", func(t *testing.T) {
		buf := new(bytes.Buffer)
		data := []byte{0x01, 0x02, 0x03, 0x04} // 4 bytes, no padding needed
		err := WriteXDROpaque(buf, data)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 4, // length = 4
			0x01, 0x02, 0x03, 0x04, // data
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})

	t.Run("EncodesOpaqueWith1BytePadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		data := []byte{0x01, 0x02, 0x03} // 3 bytes, needs 1 byte padding
		err := WriteXDROpaque(buf, data)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 3, // length = 3
			0x01, 0x02, 0x03, 0, // data + 1 byte padding
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})

	t.Run("EncodesOpaqueWith2BytesPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		data := []byte{0x01, 0x02} // 2 bytes, needs 2 bytes padding
		err := WriteXDROpaque(buf, data)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 2, // length = 2
			0x01, 0x02, 0, 0, // data + 2 bytes padding
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})

	t.Run("EncodesOpaqueWith3BytesPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		data := []byte{0x01} // 1 byte, needs 3 bytes padding
		err := WriteXDROpaque(buf, data)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 1, // length = 1
			0x01, 0, 0, 0, // data + 3 bytes padding
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})
}

// ============================================================================
// WriteXDRString Tests
// ============================================================================

func TestWriteXDRString(t *testing.T) {
	t.Run("EncodesEmptyString", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := WriteXDRString(buf, "")
		require.NoError(t, err)

		expected := []byte{0, 0, 0, 0} // length = 0
		assert.Equal(t, expected, buf.Bytes())
	})

	t.Run("EncodesStringWithoutPaddingNeeded", func(t *testing.T) {
		buf := new(bytes.Buffer)
		str := "test" // 4 bytes, no padding needed
		err := WriteXDRString(buf, str)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 4, // length = 4
			't', 'e', 's', 't', // data
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})

	t.Run("EncodesStringWith1BytePadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		str := "abc" // 3 bytes, needs 1 byte padding
		err := WriteXDRString(buf, str)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 3, // length = 3
			'a', 'b', 'c', 0, // data + 1 byte padding
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})

	t.Run("EncodesStringWith2BytesPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		str := "hi" // 2 bytes, needs 2 bytes padding
		err := WriteXDRString(buf, str)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 2, // length = 2
			'h', 'i', 0, 0, // data + 2 bytes padding
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})

	t.Run("EncodesStringWith3BytesPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		str := "x" // 1 byte, needs 3 bytes padding
		err := WriteXDRString(buf, str)
		require.NoError(t, err)

		expected := []byte{
			0, 0, 0, 1, // length = 1
			'x', 0, 0, 0, // data + 3 bytes padding
		}
		assert.Equal(t, expected, buf.Bytes())
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})

	t.Run("EncodesLongerString", func(t *testing.T) {
		buf := new(bytes.Buffer)
		str := "/export/test" // 12 bytes, no padding needed
		err := WriteXDRString(buf, str)
		require.NoError(t, err)

		// Verify length field
		assert.Equal(t, uint32(12), binary.BigEndian.Uint32(buf.Bytes()[0:4]))
		// Verify string content
		assert.Equal(t, []byte(str), buf.Bytes()[4:16])
		// Verify alignment
		assert.Equal(t, 0, len(buf.Bytes())%4, "data should be aligned to 4-byte boundary")
	})
}

// ============================================================================
// WriteXDRPadding Tests
// ============================================================================

func TestWriteXDRPadding(t *testing.T) {
	t.Run("WritesNoPaddingForAlignedLength", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := WriteXDRPadding(buf, 4)
		require.NoError(t, err)
		assert.Equal(t, 0, buf.Len(), "no padding needed for 4-byte aligned data")

		buf.Reset()
		err = WriteXDRPadding(buf, 8)
		require.NoError(t, err)
		assert.Equal(t, 0, buf.Len(), "no padding needed for 8-byte aligned data")
	})

	t.Run("Writes1BytePadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := WriteXDRPadding(buf, 3)
		require.NoError(t, err)
		assert.Equal(t, []byte{0}, buf.Bytes(), "should write 1 padding byte for length 3")
	})

	t.Run("Writes2BytesPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := WriteXDRPadding(buf, 2)
		require.NoError(t, err)
		assert.Equal(t, []byte{0, 0}, buf.Bytes(), "should write 2 padding bytes for length 2")
	})

	t.Run("Writes3BytesPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		err := WriteXDRPadding(buf, 1)
		require.NoError(t, err)
		assert.Equal(t, []byte{0, 0, 0}, buf.Bytes(), "should write 3 padding bytes for length 1")
	})

	t.Run("HandlesDifferentLengths", func(t *testing.T) {
		testCases := []struct {
			length          uint32
			expectedPadding int
		}{
			{0, 0},
			{1, 3},
			{2, 2},
			{3, 1},
			{4, 0},
			{5, 3},
			{6, 2},
			{7, 1},
			{8, 0},
			{100, 0},
			{101, 3},
		}

		for _, tc := range testCases {
			buf := new(bytes.Buffer)
			err := WriteXDRPadding(buf, tc.length)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedPadding, buf.Len(),
				"length %d should produce %d bytes of padding", tc.length, tc.expectedPadding)

			// Verify all padding bytes are zeros
			for _, b := range buf.Bytes() {
				assert.Equal(t, byte(0), b, "padding bytes must be zero")
			}
		}
	})
}
