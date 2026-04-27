package badger

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestBadgerEncodeFile_BlocksRoundTrip verifies that FileAttr.Blocks
// (Phase 12 META-01) round-trips through the Badger JSON encoder
// (encodeFile/decodeFile) without loss, including ContentHash bytes.
func TestBadgerEncodeFile_BlocksRoundTrip(t *testing.T) {
	// Build three deterministic content hashes.
	var h1, h2, h3 blockstore.ContentHash
	for i := range h1 {
		h1[i] = byte(i)
		h2[i] = byte(0xff - i)
		h3[i] = byte(0xaa)
	}

	original := &metadata.File{
		ID:        uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		ShareName: "/test",
		Path:      "/foo.bin",
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeRegular,
			Mode:         0o644,
			UID:          1000,
			GID:          1000,
			Size:         12 * 1024 * 1024,
			Mtime:        time.Unix(1700000000, 0).UTC(),
			Ctime:        time.Unix(1700000000, 0).UTC(),
			Atime:        time.Unix(1700000000, 0).UTC(),
			CreationTime: time.Unix(1700000000, 0).UTC(),
			Blocks: []blockstore.BlockRef{
				{Hash: h1, Offset: 0, Size: 4 * 1024 * 1024},
				{Hash: h2, Offset: 4 * 1024 * 1024, Size: 4 * 1024 * 1024},
				{Hash: h3, Offset: 8 * 1024 * 1024, Size: 4 * 1024 * 1024},
			},
		},
	}

	encoded, err := encodeFile(original)
	require.NoError(t, err)

	decoded, err := decodeFile(encoded)
	require.NoError(t, err)
	require.NotNil(t, decoded)

	require.Len(t, decoded.Blocks, 3)
	assert.Equal(t, original.Blocks[0], decoded.Blocks[0])
	assert.Equal(t, original.Blocks[1], decoded.Blocks[1])
	assert.Equal(t, original.Blocks[2], decoded.Blocks[2])

	// Hash bytes preserved exactly.
	assert.True(t, bytes.Equal(original.Blocks[0].Hash[:], decoded.Blocks[0].Hash[:]))
	assert.True(t, bytes.Equal(original.Blocks[1].Hash[:], decoded.Blocks[1].Hash[:]))
	assert.True(t, bytes.Equal(original.Blocks[2].Hash[:], decoded.Blocks[2].Hash[:]))

	// Non-Blocks fields preserved.
	assert.Equal(t, original.ID, decoded.ID)
	assert.Equal(t, original.ShareName, decoded.ShareName)
	assert.Equal(t, original.Path, decoded.Path)
	assert.Equal(t, original.Size, decoded.Size)
	assert.Equal(t, original.Mode, decoded.Mode)
}

// TestBadgerEncodeFile_LegacyBlobNoBlocks asserts that a pre-Phase-12
// JSON blob (no "blocks" key at all) deserializes cleanly with Blocks==nil.
// This locks in D-06 (legacy compat) and threat T-12-08 (mitigation:
// json+omitempty handles legacy blobs).
func TestBadgerEncodeFile_LegacyBlobNoBlocks(t *testing.T) {
	// Hand-crafted minimal JSON blob representing a file persisted before
	// Phase 12 added the "blocks" field. No "blocks" key — must decode
	// with no error and result in Blocks == nil.
	legacyJSON := []byte(`{
		"id": "11111111-2222-3333-4444-555555555555",
		"share_name": "/legacy",
		"path": "/old.txt",
		"type": 0,
		"mode": 420,
		"uid": 0,
		"gid": 0,
		"nlink": 1,
		"size": 42,
		"atime": "2026-01-01T00:00:00Z",
		"mtime": "2026-01-01T00:00:00Z",
		"ctime": "2026-01-01T00:00:00Z",
		"creation_time": "2026-01-01T00:00:00Z",
		"content_id": "old-payload"
	}`)

	decoded, err := decodeFile(legacyJSON)
	require.NoError(t, err)
	require.NotNil(t, decoded)

	assert.Nil(t, decoded.Blocks, "legacy blob (no blocks key) must decode to nil Blocks")
	assert.Equal(t, "/legacy", decoded.ShareName)
	assert.Equal(t, "/old.txt", decoded.Path)
	assert.Equal(t, uint64(42), decoded.Size)
}

// TestBadgerEncodeFile_NilBlocksOmitted asserts the omitempty tag on
// FileAttr.Blocks so encoding a file with nil Blocks does NOT emit a
// "blocks" key — keeps legacy/zero-value blobs free of churn (D-05).
func TestBadgerEncodeFile_NilBlocksOmitted(t *testing.T) {
	file := &metadata.File{
		ID:        uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		ShareName: "/test",
		Path:      "/empty.txt",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			// Blocks left nil
		},
	}

	encoded, err := encodeFile(file)
	require.NoError(t, err)

	// json.Marshal must omit the "blocks" key entirely.
	assert.False(t, strings.Contains(string(encoded), `"blocks"`),
		"nil Blocks must be omitted from JSON via omitempty (got %s)", string(encoded))

	// Sanity: it should still be valid JSON for a File (round-trip).
	var roundtrip metadata.File
	require.NoError(t, json.Unmarshal(encoded, &roundtrip))
	assert.Nil(t, roundtrip.Blocks)
}
