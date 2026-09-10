package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// newShareOnStore creates a share rooted in an existing store and registers it
// with svc, returning the root handle. Putting two shares in one store is what
// makes these tests meaningful: the foreign row is genuinely reachable through
// the other share's store, so an unguarded operation succeeds rather than
// failing as a lookup miss.
func newShareOnStore(
	t *testing.T,
	svc *metadata.Service,
	store *memory.MemoryMetadataStore,
	share string,
) metadata.FileHandle {
	t.Helper()

	root, err := store.CreateRootDirectory(context.Background(), share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	require.NoError(t, err)
	require.NoError(t, svc.RegisterStoreForShare(share, store))

	handle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)
	return handle
}

// TestCreateHardLink_RejectsCrossShareTarget pins that a hard link never spans
// two shares. Only the directory handle selects the store, so without an
// explicit comparison the directory entry and the nlink bump both land on a
// file the share does not own.
func TestCreateHardLink_RejectsCrossShareTarget(t *testing.T) {
	t.Parallel()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	svc := metadata.New()
	dirRoot := newShareOnStore(t, svc, store, "/link-a")
	targetRoot := newShareOnStore(t, svc, store, "/link-b")
	authCtx := mkCtx(0, 0)

	target, _, err := svc.CreateFile(authCtx, targetRoot, "victim.txt",
		&metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)

	targetHandle, err := metadata.EncodeShareHandle("/link-b", target.ID)
	require.NoError(t, err)

	_, err = svc.CreateHardLink(authCtx, dirRoot, "stolen.txt", targetHandle)
	require.Error(t, err, "hard link across shares must be rejected")

	// The rejection must leave no entry behind and no inflated link count.
	_, err = svc.Lookup(authCtx, dirRoot, "stolen.txt")
	require.Error(t, err, "no directory entry may be created for a rejected link")

	after, err := svc.GetFile(context.Background(), targetHandle)
	require.NoError(t, err)
	require.EqualValues(t, target.Nlink, after.Nlink, "target nlink must not change")
}

// TestMove_RejectsCrossShareDestination pins the same rule for Move, the
// backend behind RENAME: only the source handle selects the store, so a
// destination directory in another share would otherwise resolve and the entry
// would be re-parented across the boundary.
func TestMove_RejectsCrossShareDestination(t *testing.T) {
	t.Parallel()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	svc := metadata.New()
	srcRoot := newShareOnStore(t, svc, store, "/move-a")
	dstRoot := newShareOnStore(t, svc, store, "/move-b")
	authCtx := mkCtx(0, 0)

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
