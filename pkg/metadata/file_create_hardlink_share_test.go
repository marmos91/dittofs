package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// TestCreateHardLink_RejectsCrossShareTarget pins that a hard link never spans
// two shares, even when both shares live in one metadata store and the target
// row is therefore reachable.
//
// Only the directory handle selects the store, and a store resolves whichever
// handle it is handed, so without an explicit comparison the directory entry
// and the nlink bump both land on a file the share does not own.
func TestCreateHardLink_RejectsCrossShareTarget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.NewMemoryMetadataStoreWithDefaults()
	svc := metadata.New()

	// Two shares backed by the same store: the foreign file is genuinely
	// readable through the directory's store, which is what makes an
	// unguarded link succeed rather than fail as a lookup miss.
	handles := make(map[string]metadata.FileHandle, 2)
	for _, share := range []string{"/share-a", "/share-b"} {
		root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		})
		require.NoError(t, err)

		handle, err := metadata.EncodeShareHandle(share, root.ID)
		require.NoError(t, err)
		handles[share] = handle

		require.NoError(t, svc.RegisterStoreForShare(share, store))
	}

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}

	target, _, err := svc.CreateFile(authCtx, handles["/share-b"], "victim.txt",
		&metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)

	targetHandle, err := metadata.EncodeShareHandle("/share-b", target.ID)
	require.NoError(t, err)

	// Directory in share A, target file in share B.
	_, err = svc.CreateHardLink(authCtx, handles["/share-a"], "stolen.txt", targetHandle)
	require.Error(t, err, "hard link across shares must be rejected")

	// The rejection must leave no entry behind and no inflated link count.
	_, err = svc.Lookup(authCtx, handles["/share-a"], "stolen.txt")
	require.Error(t, err, "no directory entry may be created for a rejected link")

	after, err := svc.GetFile(ctx, targetHandle)
	require.NoError(t, err)
	require.EqualValues(t, target.Nlink, after.Nlink, "target nlink must not change")
}
