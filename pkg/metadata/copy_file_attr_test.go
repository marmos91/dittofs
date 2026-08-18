package metadata

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// TestCopyFileAttr_CopiesEveryField walks FileAttr reflectively rather than
// asserting on a hand-written list, because a hand-written list is exactly what
// went stale: fields added to the struct never reached the copy. Any future
// field is covered the moment it is declared.
func TestCopyFileAttr_CopiesEveryField(t *testing.T) {
	deletedAt := time.Unix(1700000000, 0).UTC()
	src := &FileAttr{
		Type:               FileTypeRegular,
		Mode:               0o644,
		UID:                1000,
		GID:                1000,
		Nlink:              3,
		Size:               4096,
		Atime:              time.Unix(1, 0).UTC(),
		Mtime:              time.Unix(2, 0).UTC(),
		Ctime:              time.Unix(3, 0).UTC(),
		CreationTime:       time.Unix(4, 0).UTC(),
		PayloadID:          "payload-1",
		LinkTarget:         "/target",
		Rdev:               42,
		Hidden:             true,
		EAs:                map[string][]byte{"user.k": []byte("v")},
		IdempotencyToken:   99,
		Blocks:             []block.ChunkRef{{Offset: 0}},
		BlocksDirty:        true,
		BlocksDirtyOffsets: []uint64{0, 8192},
		NewInode:           true,
		DeletedAt:          &deletedAt,
		OriginalPath:       "/was/here",
		DeletedBy:          "someone",
		ACL: &acl.ACL{
			ACEs:          []acl.ACE{{}},
			Protected:     true,
			AutoInherited: true,
			NullDACL:      true,
		},
	}

	got := CopyFileAttr(src)
	require.NotNil(t, got)

	sv, gv := reflect.ValueOf(*src), reflect.ValueOf(*got)
	for i := 0; i < sv.NumField(); i++ {
		name := sv.Type().Field(i).Name
		require.True(t, reflect.DeepEqual(sv.Field(i).Interface(), gv.Field(i).Interface()),
			"field %s was not carried over by CopyFileAttr", name)
	}
}

// TestCopyFileAttr_IsDeep confirms the copy does not alias the source, which is
// the whole reason callers reach for it.
func TestCopyFileAttr_IsDeep(t *testing.T) {
	deletedAt := time.Unix(1700000000, 0).UTC()
	src := &FileAttr{
		EAs:                map[string][]byte{"user.k": []byte("v")},
		Blocks:             []block.ChunkRef{{Offset: 0}},
		BlocksDirtyOffsets: []uint64{1},
		DeletedAt:          &deletedAt,
		ACL:                &acl.ACL{ACEs: []acl.ACE{{}}, NullDACL: true},
	}

	got := CopyFileAttr(src)

	got.EAs["user.k"][0] = 'X'
	got.EAs["added"] = []byte("new")
	got.Blocks[0].Offset = 777
	got.BlocksDirtyOffsets[0] = 777
	*got.DeletedAt = time.Unix(0, 0).UTC()
	got.ACL.NullDACL = false
	got.ACL.ACEs = append(got.ACL.ACEs, acl.ACE{})

	require.Equal(t, byte('v'), src.EAs["user.k"][0])
	require.NotContains(t, src.EAs, "added")
	require.Equal(t, uint64(0), src.Blocks[0].Offset)
	require.Equal(t, uint64(1), src.BlocksDirtyOffsets[0])
	require.Equal(t, deletedAt, *src.DeletedAt)
	require.True(t, src.ACL.NullDACL, "mutating the copy must not clear the source's null-DACL flag")
	require.Len(t, src.ACL.ACEs, 1)
}

// TestCopyFileAttr_Nil pins the nil passthrough.
func TestCopyFileAttr_Nil(t *testing.T) {
	require.Nil(t, CopyFileAttr(nil))
}
