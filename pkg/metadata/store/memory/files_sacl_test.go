package memory

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// putFileWithACL stamps a file carrying the given ACL through the public
// Put/Get path on MemoryMetadataStore and returns (handle, file).
func putFileWithACL(t *testing.T, store *MemoryMetadataStore, shareName, name string, a *acl.ACL) (metadata.FileHandle, *metadata.File) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	rootAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)

	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	now := time.Now().UTC()
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000,
			Mtime: now, Ctime: now, Atime: now, CreationTime: now,
			ACL: a,
		},
	}
	require.NoError(t, store.PutFile(ctx, file))
	require.NoError(t, store.SetParent(ctx, handle, rootHandle))
	require.NoError(t, store.SetChild(ctx, rootHandle, name, handle))
	return handle, file
}

// TestMemoryStore_SACL_RoundTrip verifies a stored SACL survives Put/Get in the
// memory backend.
func TestMemoryStore_SACL_RoundTrip(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	in := &acl.ACL{
		ACEs: []acl.ACE{{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialOwner}},
		SACL: []acl.ACE{{
			Type:       acl.ACE4_SYSTEM_AUDIT_ACE_TYPE,
			Flag:       acl.ACE4_FAILED_ACCESS_ACE_FLAG,
			AccessMask: acl.ACE4_WRITE_DATA,
			Who:        acl.SpecialEveryone,
		}},
	}
	handle, _ := putFileWithACL(t, store, "share-sacl", "f", in)

	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.NotNil(t, got.ACL)
	require.Len(t, got.ACL.SACL, 1, "stored SACL must survive Put/Get")
	require.Equal(t, uint32(acl.ACE4_SYSTEM_AUDIT_ACE_TYPE), got.ACL.SACL[0].Type)
	require.Equal(t, uint32(acl.ACE4_FAILED_ACCESS_ACE_FLAG), got.ACL.SACL[0].Flag)
}

// TestMemoryStore_SACL_DeepCopy verifies cloneACL deep-copies the SACL slice so
// a caller-side mutation after PutFile does not leak into stored state.
func TestMemoryStore_SACL_DeepCopy(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	in := &acl.ACL{
		SACL: []acl.ACE{{Type: acl.ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: acl.ACE4_WRITE_DATA, Who: acl.SpecialEveryone}},
	}
	handle, file := putFileWithACL(t, store, "share-sacl-copy", "f", in)

	// Mutate the caller-side SACL after PutFile returned.
	file.ACL.SACL[0].AccessMask = acl.ACE4_READ_DATA

	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.NotNil(t, got.ACL)
	require.Len(t, got.ACL.SACL, 1)
	require.Equal(t, uint32(acl.ACE4_WRITE_DATA), got.ACL.SACL[0].AccessMask,
		"PutFile must deep-copy SACL: caller-side mutation leaked into stored state")
}
