package badger

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// TestBadgerEncodeFile_ManifestSplit verifies that the chunk manifest is NOT
// carried in the f: attribute blob (it lives in the sibling fm: key), while the
// manifest codec round-trips Blocks — including ContentHash bytes — without loss.
func TestBadgerEncodeFile_ManifestSplit(t *testing.T) {
	// Build three deterministic content hashes.
	var h1, h2, h3 block.ContentHash
	for i := range h1 {
		h1[i] = byte(i)
		h2[i] = byte(0xff - i)
		h3[i] = byte(0xaa)
	}

	blocks := []block.ChunkRef{
		{Hash: h1, Offset: 0, Size: 4 * 1024 * 1024},
		{Hash: h2, Offset: 4 * 1024 * 1024, Size: 4 * 1024 * 1024},
		{Hash: h3, Offset: 8 * 1024 * 1024, Size: 4 * 1024 * 1024},
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
			Blocks:       blocks,
		},
	}

	encoded, err := encodeFile(original)
	require.NoError(t, err)

	decoded, err := decodeFile(encoded)
	require.NoError(t, err)
	require.NotNil(t, decoded)

	// The manifest is split out of the attribute blob: encodeFile drops it.
	assert.Empty(t, decoded.Blocks, "encodeFile must not embed the chunk manifest")

	// Non-Blocks fields still round-trip.
	assert.Equal(t, original.ID, decoded.ID)
	assert.Equal(t, original.ShareName, decoded.ShareName)
	assert.Equal(t, original.Size, decoded.Size)
	assert.Equal(t, original.Mode, decoded.Mode)

	// The manifest codec (fm: value) round-trips the chunk list exactly.
	mEnc, err := encodeManifest(blocks)
	require.NoError(t, err)
	mDec, err := decodeManifest(mEnc)
	require.NoError(t, err)
	require.Len(t, mDec, 3)
	assert.Equal(t, blocks[0], mDec[0])
	assert.Equal(t, blocks[1], mDec[1])
	assert.Equal(t, blocks[2], mDec[2])
	assert.True(t, bytes.Equal(blocks[0].Hash[:], mDec[0].Hash[:]))
	assert.True(t, bytes.Equal(blocks[2].Hash[:], mDec[2].Hash[:]))

	// Path is intentionally NOT persisted (#1166): it is always derived from
	// parent edges on read, so encodeFile zeroes it before serializing. This
	// prevents a stale stored path (e.g. the pre-move path on a rename) from
	// ever landing on disk. decodeFile tolerates the empty field; GetFile
	// overwrites Path via derivePath. The caller's struct is not mutated.
	assert.Empty(t, decoded.Path, "encodeFile must not persist File.Path")
	assert.Equal(t, "/foo.bin", original.Path, "encodeFile must not mutate the caller's File")
}

// TestBadgerEncodeFile_LegacyBlobNoBlocks asserts that a
// JSON blob (no "blocks" key at all) deserializes cleanly with Blocks==nil.
// This locks in (legacy compat) and (mitigation:
// json+omitempty handles legacy blobs).
func TestBadgerEncodeFile_LegacyBlobNoBlocks(t *testing.T) {
	// Hand-crafted minimal JSON blob representing a file persisted before
	// added the "blocks" field. No "blocks" key — must decode
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

// TestBadgerEncodeFile_NilBlocksOmitted asserts a file with nil Blocks does not
// emit the Blocks field (no churn) and round-trips back to nil. The codec is now
// the binary format (0xD5 magic), so we assert the binary invariant rather than
// a JSON "blocks" key.
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

	// New writes are binary, not JSON.
	require.GreaterOrEqual(t, len(encoded), 3)
	assert.Equal(t, byte(fileMagic0), encoded[0], "binary magic byte 0")
	assert.NotEqual(t, byte('{'), encoded[0], "must not be JSON")

	roundtrip, err := decodeFile(encoded)
	require.NoError(t, err)
	assert.Nil(t, roundtrip.Blocks, "nil Blocks must round-trip to nil")
}

// TestEncodeDecodeFile_AllFields exercises every persisted FileAttr field
// (including ACL, EAs, ObjectID, DeletedAt, unicode names, empty/nil maps and a
// zero-length EA value) through encode -> binary decode. Path and BlocksDirty
// are intentionally not persisted.
func TestEncodeDecodeFile_AllFields(t *testing.T) {
	var oid block.ContentHash
	for i := range oid {
		oid[i] = byte(i + 1)
	}
	del := time.Unix(1700009999, 500).UTC()
	orig := &metadata.File{
		ID:        uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		ShareName: "/share-π",
		Path:      "/should/not/persist",
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeSymlink,
			Mode:         0o4755,
			UID:          4294967295, // max uint32
			GID:          123,
			Nlink:        7,
			Size:         1<<40 + 3,
			Atime:        time.Unix(1700000001, 111).UTC(),
			Mtime:        time.Unix(1700000002, 222).UTC(),
			Ctime:        time.Unix(1700000003, 333).UTC(),
			CreationTime: time.Unix(1700000004, 444).UTC(),
			PayloadID:    "share-π/файл",
			LinkTarget:   "../targ€t/文件",
			Rdev:         0xDEADBEEF,
			Hidden:       true,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{{
					Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
					Flag:       0,
					AccessMask: 0x00000003,
					Who:        acl.SpecialOwner,
				}},
				Source: acl.ACLSourceNFSExplicit,
			},
			EAs: map[string][]byte{
				"user.comment":  []byte("héllo"),
				"user.emptyval": {}, // zero-length EA must round-trip as present
				"user.binary":   {0x00, 0xFF, 0x7B, 0xD5},
			},
			IdempotencyToken: 0x1122334455667788,
			ObjectID:         oid,
			DeletedAt:        &del,
			OriginalPath:     "documents/старый.txt",
			DeletedBy:        "user€",
		},
	}

	enc, err := encodeFile(orig)
	require.NoError(t, err)
	require.NotEqual(t, byte('{'), enc[0], "must be binary")

	got, err := decodeFile(enc)
	require.NoError(t, err)

	assert.Empty(t, got.Path, "Path must not persist")
	got.Path = orig.Path // normalize for full-struct compare below

	// time.Time compares poorly with == across marshal; check each explicitly.
	for _, tc := range []struct {
		name string
		a, b time.Time
	}{
		{"atime", got.Atime, orig.Atime}, {"mtime", got.Mtime, orig.Mtime},
		{"ctime", got.Ctime, orig.Ctime}, {"creation", got.CreationTime, orig.CreationTime},
	} {
		assert.True(t, tc.a.Equal(tc.b), "%s: got %v want %v", tc.name, tc.a, tc.b)
	}
	require.NotNil(t, got.DeletedAt)
	assert.True(t, got.DeletedAt.Equal(del))

	// Zero the times so ObjectsAreEqual on the rest is exact.
	z := func(f *metadata.File) {
		f.Atime, f.Mtime, f.Ctime, f.CreationTime = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		f.DeletedAt = nil
	}
	gc, oc := *got, *orig
	z(&gc)
	z(&oc)
	assert.Equal(t, oc, gc, "all non-time fields must round-trip exactly")

	// Zero-length EA distinguishable from absent.
	v, ok := got.LookupEA("user.emptyval")
	assert.True(t, ok)
	assert.NotNil(t, v)
	assert.Len(t, v, 0)
}

// TestDecodeFile_NilAndEmptyMaps verifies nil ACL/EAs/Blocks and empty EAs map
// all round-trip to nil (the omit-when-empty invariant), and an explicit
// non-nil empty ACL stays non-nil (it denies all access — a real distinction).
func TestDecodeFile_NilAndEmptyMaps(t *testing.T) {
	base := &metadata.File{ID: uuid.New(), ShareName: "/s", FileAttr: metadata.FileAttr{Mode: 0o644}}
	base.EAs = map[string][]byte{} // empty, not nil
	enc, err := encodeFile(base)
	require.NoError(t, err)
	got, err := decodeFile(enc)
	require.NoError(t, err)
	assert.Nil(t, got.ACL)
	assert.Nil(t, got.EAs, "empty EAs map must not persist")
	assert.Nil(t, got.Blocks)
	assert.True(t, got.ObjectID.IsZero())
	assert.Nil(t, got.DeletedAt)

	// Explicit empty (non-nil) ACL: the deny-all case must survive as non-nil.
	base.ACL = &acl.ACL{}
	enc, err = encodeFile(base)
	require.NoError(t, err)
	got, err = decodeFile(enc)
	require.NoError(t, err)
	require.NotNil(t, got.ACL, "explicit empty ACL must stay non-nil (deny-all)")
}

// TestDecodeFile_JSONFallback proves a real JSON-encoded record (produced by
// the pre-#1735 encoder) still decodes via decodeFile. This is the dual-read
// migration path.
func TestDecodeFile_JSONFallback(t *testing.T) {
	orig := &metadata.File{
		ID:        uuid.New(),
		ShareName: "/legacy",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o600, UID: 1, GID: 2, Nlink: 1,
			Size: 99, Mtime: time.Unix(1700000000, 0).UTC(), PayloadID: "legacy/pl",
			EAs: map[string][]byte{"user.x": []byte("y")},
		},
	}
	// Emulate the old encoder: JSON with Path zeroed.
	clone := *orig
	clone.Path = ""
	jsonBlob, err := json.Marshal(&clone)
	require.NoError(t, err)
	require.Equal(t, byte('{'), jsonBlob[0], "legacy blob must start with 0x7b")

	got, err := decodeFile(jsonBlob)
	require.NoError(t, err)
	assert.Equal(t, orig.ID, got.ID)
	assert.Equal(t, orig.Size, got.Size)
	assert.Equal(t, []byte("y"), got.EAs["user.x"])
}

// TestDecodeFile_SchemaEvolution demonstrates the self-describing property: a
// binary record written by a HYPOTHETICAL future writer that appended an unknown
// field still decodes on this (older) reader — the unknown field is skipped and
// known fields survive.
func TestDecodeFile_SchemaEvolution(t *testing.T) {
	orig := &metadata.File{ID: uuid.New(), ShareName: "/s", FileAttr: metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644, Size: 5, PayloadID: "s/p",
	}}
	enc, err := encodeFile(orig)
	require.NoError(t, err)

	// Splice in an unknown future field (id 250, 4-byte payload) at the end.
	future := putField(enc, 250, []byte{0xCA, 0xFE, 0xBA, 0xBE})

	got, err := decodeFile(future)
	require.NoError(t, err, "unknown future field must be skipped, not fatal")
	assert.Equal(t, orig.ID, got.ID)
	assert.Equal(t, orig.Size, got.Size)
	assert.Equal(t, orig.PayloadID, got.PayloadID)
}

// TestDecodeFile_Corruption guards that a truncated/garbled binary record errors
// instead of panicking.
func TestDecodeFile_Corruption(t *testing.T) {
	orig := &metadata.File{ID: uuid.New(), ShareName: "/s", FileAttr: metadata.FileAttr{Size: 5}}
	enc, err := encodeFile(orig)
	require.NoError(t, err)
	for cut := 3; cut < len(enc); cut++ {
		_, _ = decodeFile(enc[:cut]) // must not panic; error is acceptable
	}
	_, err = decodeFile([]byte{fileMagic0, fileMagic1, 0xFF}) // bad version
	require.Error(t, err)
}
