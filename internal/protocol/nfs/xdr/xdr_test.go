package xdr

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
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

func validFileHandle(id uint64) metadata.FileHandle {
	handle := make([]byte, 8)
	binary.BigEndian.PutUint64(handle, id)
	return metadata.FileHandle(handle)
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
	t.Run("ExtractsValidFileID", func(t *testing.T) {
		handle := validFileHandle(12345)
		fileID := ExtractFileID(handle)
		assert.Equal(t, uint64(12345), fileID)
	})

	t.Run("ExtractsZeroFileID", func(t *testing.T) {
		handle := validFileHandle(0)
		fileID := ExtractFileID(handle)
		assert.Equal(t, uint64(0), fileID)
	})

	t.Run("ExtractsMaxUint64", func(t *testing.T) {
		handle := validFileHandle(^uint64(0))
		fileID := ExtractFileID(handle)
		assert.Equal(t, uint64(^uint64(0)), fileID)
	})

	t.Run("ReturnsZeroForShortHandle", func(t *testing.T) {
		handle := metadata.FileHandle([]byte{0x01, 0x02})
		fileID := ExtractFileID(handle)
		assert.Equal(t, uint64(0), fileID)
	})

	t.Run("ReturnsZeroForEmptyHandle", func(t *testing.T) {
		handle := metadata.FileHandle([]byte{})
		fileID := ExtractFileID(handle)
		assert.Equal(t, uint64(0), fileID)
	})
}

// ============================================================================
// DecodeOpaque Tests
// ============================================================================

func TestDecodeOpaque(t *testing.T) {
	t.Run("DecodesEmptyOpaque", func(t *testing.T) {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.BigEndian, uint32(0))

		data, err := DecodeOpaque(buf)
		require.NoError(t, err)
		assert.Empty(t, data)
	})

	t.Run("DecodesOpaqueWithoutPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.BigEndian, uint32(4))
		buf.Write([]byte{0x01, 0x02, 0x03, 0x04})

		data, err := DecodeOpaque(buf)
		require.NoError(t, err)
		assert.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, data)
	})

	t.Run("DecodesOpaqueWithPadding", func(t *testing.T) {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.BigEndian, uint32(3))
		buf.Write([]byte{0x01, 0x02, 0x03, 0x00})

		data, err := DecodeOpaque(buf)
		require.NoError(t, err)
		assert.Equal(t, []byte{0x01, 0x02, 0x03}, data)
	})

	t.Run("RejectsExcessiveLength", func(t *testing.T) {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.BigEndian, uint32(2*1024*1024))

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
		binary.Write(buf, binary.BigEndian, uint32(0))

		str, err := DecodeString(buf)
		require.NoError(t, err)
		assert.Empty(t, str)
	})

	t.Run("DecodesSimpleString", func(t *testing.T) {
		buf := new(bytes.Buffer)
		testStr := "hello"
		binary.Write(buf, binary.BigEndian, uint32(len(testStr)))
		buf.WriteString(testStr)
		buf.Write([]byte{0, 0, 0}) // padding

		str, err := DecodeString(buf)
		require.NoError(t, err)
		assert.Equal(t, "hello", str)
	})
}
