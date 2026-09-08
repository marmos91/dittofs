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
	mkShare := func(share string) metadata.FileHandle {
		root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		})
		require.NoError(t, err)
		require.NoError(t, svc.RegisterStoreForShare(share, store))

		handle, err := metadata.EncodeShareHandle(share, root.ID)
		require.NoError(t, err)
		return handle
	}
	dirRoot := mkShare("/share-a")
	targetRoot := mkShare("/share-b")

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

	target, _, err := svc.CreateFile(authCtx, targetRoot, "victim.txt",
		&metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)

	targetHandle, err := metadata.EncodeShareHandle("/share-b", target.ID)
	require.NoError(t, err)

	// Directory in share A, target file in share B.
	_, err = svc.CreateHardLink(authCtx, dirRoot, "stolen.txt", targetHandle)
	require.Error(t, err, "hard link across shares must be rejected")

	// The rejection must leave no entry behind and no inflated link count.
	_, err = svc.Lookup(authCtx, dirRoot, "stolen.txt")
	require.Error(t, err, "no directory entry may be created for a rejected link")

	after, err := svc.GetFile(ctx, targetHandle)
	require.NoError(t, err)
	require.EqualValues(t, target.Nlink, after.Nlink, "target nlink must not change")
}

// TestMove_RejectsCrossShareDestination pins the same rule for Move, the
// backend behind RENAME: only the source handle selects the store, so a
// destination directory in another share would otherwise resolve and the entry
// would be re-parented across the boundary.
func TestMove_RejectsCrossShareDestination(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.NewMemoryMetadataStoreWithDefaults()
	svc := metadata.New()

	mkShare := func(share string) metadata.FileHandle {
		root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		})
		require.NoError(t, err)
		require.NoError(t, svc.RegisterStoreForShare(share, store))

		handle, err := metadata.EncodeShareHandle(share, root.ID)
		require.NoError(t, err)
		return handle
	}
	srcRoot := mkShare("/move-a")
	dstRoot := mkShare("/move-b")

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

	_, _, err := svc.CreateFile(authCtx, srcRoot, "movable.txt", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)

	_, _, err = svc.Move(authCtx, srcRoot, "movable.txt", dstRoot, "stolen.txt")
	require.Error(t, err, "move across shares must be rejected")

	// The source entry must survive and no entry may appear in the destination.
	_, err = svc.Lookup(authCtx, srcRoot, "movable.txt")
	require.NoError(t, err, "source entry must be left in place")

	_, err = svc.Lookup(authCtx, dstRoot, "stolen.txt")
	require.Error(t, err, "no entry may be created in the foreign share")
}
