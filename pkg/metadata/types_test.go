package metadata

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// EncodeFileHandle Tests
// ============================================================================

func TestEncodeFileHandle(t *testing.T) {
	t.Parallel()

	t.Run("encodes valid file", func(t *testing.T) {
		t.Parallel()
		id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
		file := &File{
			ID:        id,
			ShareName: "/export",
		}

		handle, err := EncodeFileHandle(file)

		require.NoError(t, err)
		assert.Equal(t, "/export:550e8400-e29b-41d4-a716-446655440000", string(handle))
	})

	t.Run("rejects handle exceeding 64 bytes", func(t *testing.T) {
		t.Parallel()
		id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
		// Create a share name that makes the total exceed 64 bytes
		// UUID is 36 chars, colon is 1 char, so share name > 27 chars will exceed
		file := &File{
			ID:        id,
			ShareName: "/this-is-a-very-long-share-name-that-exceeds",
		}

		handle, err := EncodeFileHandle(file)

		assert.Error(t, err)
		assert.Nil(t, handle)
		assert.Contains(t, err.Error(), "file handle too long")
	})
}

// ============================================================================
// EncodeShareHandle Tests
// ============================================================================

func TestEncodeShareHandle(t *testing.T) {
	t.Parallel()

	t.Run("encodes valid share and UUID", func(t *testing.T) {
		t.Parallel()
		id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

		handle, err := EncodeShareHandle("/export", id)

		require.NoError(t, err)
		assert.Equal(t, "/export:550e8400-e29b-41d4-a716-446655440000", string(handle))
	})

	t.Run("rejects handle exceeding 64 bytes", func(t *testing.T) {
		t.Parallel()
		id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

		handle, err := EncodeShareHandle("/this-is-a-very-long-share-name-that-exceeds", id)

		assert.Error(t, err)
		assert.Nil(t, handle)
	})
}

// ============================================================================
// DecodeFileHandle Tests
// ============================================================================

func TestDecodeFileHandle(t *testing.T) {
	t.Parallel()

	t.Run("decodes valid handle", func(t *testing.T) {
		t.Parallel()
		handle := FileHandle("/export:550e8400-e29b-41d4-a716-446655440000")

		shareName, id, err := DecodeFileHandle(handle)

		require.NoError(t, err)
		assert.Equal(t, "/export", shareName)
		assert.Equal(t, uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"), id)
	})

	t.Run("rejects missing colon separator", func(t *testing.T) {
		t.Parallel()
		handle := FileHandle("export550e8400-e29b-41d4-a716-446655440000")

		shareName, id, err := DecodeFileHandle(handle)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing ':' separator")
		assert.Empty(t, shareName)
		assert.Equal(t, uuid.Nil, id)
	})

	t.Run("rejects empty share name", func(t *testing.T) {
		t.Parallel()
		handle := FileHandle(":550e8400-e29b-41d4-a716-446655440000")

		shareName, id, err := DecodeFileHandle(handle)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "empty share name")
		assert.Empty(t, shareName)
		assert.Equal(t, uuid.Nil, id)
	})

	t.Run("rejects invalid UUID", func(t *testing.T) {
		t.Parallel()
		handle := FileHandle("/export:not-a-valid-uuid")

		shareName, id, err := DecodeFileHandle(handle)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "malformed UUID")
		assert.Empty(t, shareName)
		assert.Equal(t, uuid.Nil, id)
	})

	t.Run("handles share name with colon", func(t *testing.T) {
		t.Parallel()
		// Only first colon is used as separator
		handle := FileHandle("/export:path:550e8400-e29b-41d4-a716-446655440000")

		shareName, id, err := DecodeFileHandle(handle)

		// This should fail because "path:550e..." is not a valid UUID
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "malformed UUID")
		assert.Empty(t, shareName)
		assert.Equal(t, uuid.Nil, id)
	})
}

// ============================================================================
// GenerateNewHandle Tests
// ============================================================================

func TestGenerateNewHandle(t *testing.T) {
	t.Parallel()

	t.Run("creates valid handle", func(t *testing.T) {
		t.Parallel()
		handle, err := GenerateNewHandle("/export")

		require.NoError(t, err)
		require.NotNil(t, handle)

		// Verify it can be decoded
		shareName, id, err := DecodeFileHandle(handle)
		require.NoError(t, err)
		assert.Equal(t, "/export", shareName)
		assert.NotEqual(t, uuid.Nil, id)
	})

	t.Run("creates unique handles", func(t *testing.T) {
		t.Parallel()
		handle1, err1 := GenerateNewHandle("/export")
		handle2, err2 := GenerateNewHandle("/export")

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.NotEqual(t, string(handle1), string(handle2))
	})

	t.Run("different shares have different handles", func(t *testing.T) {
		t.Parallel()
		handle1, err1 := GenerateNewHandle("/export1")
		handle2, err2 := GenerateNewHandle("/export2")

		require.NoError(t, err1)
		require.NoError(t, err2)

		shareName1, _, _ := DecodeFileHandle(handle1)
		shareName2, _, _ := DecodeFileHandle(handle2)

		assert.Equal(t, "/export1", shareName1)
		assert.Equal(t, "/export2", shareName2)
	})
}

// ============================================================================
// HandleToINode Tests
// ============================================================================

func TestHandleToINode(t *testing.T) {
	t.Parallel()

	t.Run("empty handle returns zero", func(t *testing.T) {
		t.Parallel()
		inode := HandleToINode(FileHandle{})
		assert.Equal(t, uint64(0), inode)
	})

	t.Run("nil handle returns zero", func(t *testing.T) {
		t.Parallel()
		inode := HandleToINode(nil)
		assert.Equal(t, uint64(0), inode)
	})

	t.Run("consistent hash for same handle", func(t *testing.T) {
		t.Parallel()
		handle := FileHandle("/export:550e8400-e29b-41d4-a716-446655440000")

		inode1 := HandleToINode(handle)
		inode2 := HandleToINode(handle)

		assert.Equal(t, inode1, inode2)
		assert.NotEqual(t, uint64(0), inode1)
	})

	t.Run("different handles produce different inodes", func(t *testing.T) {
		t.Parallel()
		handle1 := FileHandle("/export:550e8400-e29b-41d4-a716-446655440000")
		handle2 := FileHandle("/export:550e8400-e29b-41d4-a716-446655440001")

		inode1 := HandleToINode(handle1)
		inode2 := HandleToINode(handle2)

		// While hash collisions are theoretically possible, they should be rare
		assert.NotEqual(t, inode1, inode2)
	})

	t.Run("returns non-zero for valid handle", func(t *testing.T) {
		t.Parallel()
		handle, _ := GenerateNewHandle("/test")

		inode := HandleToINode(handle)

		// SHA-256 of non-empty data should produce non-zero first 8 bytes
		assert.NotEqual(t, uint64(0), inode)
	})
}

// ============================================================================
// Roundtrip Tests
// ============================================================================

func TestFileHandleRoundtrip(t *testing.T) {
	t.Parallel()

	t.Run("encode then decode preserves data", func(t *testing.T) {
		t.Parallel()
		originalID := uuid.New()
		originalShare := "/myshare"

		file := &File{
			ID:        originalID,
			ShareName: originalShare,
		}

		// Encode
		handle, err := EncodeFileHandle(file)
		require.NoError(t, err)

		// Decode
		shareName, id, err := DecodeFileHandle(handle)
		require.NoError(t, err)

		assert.Equal(t, originalShare, shareName)
		assert.Equal(t, originalID, id)
	})
}
