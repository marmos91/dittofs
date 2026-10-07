package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
	"github.com/stretchr/testify/require"
)

// The SMB rename ChangeTime preserve puts back the ChangeTime the renamed inode
// had before Move stamped its own, but only while that stamp is still what the
// store holds. Both values come from inside Move's transaction, which is what
// makes the restore safe to apply and possible to apply at all.

// newRenameFixture builds a Service over an in-memory Badger store.
func newRenameFixture(t *testing.T) (*metadata.Service, metadata.FileHandle, string) {
	t.Helper()
	return registerRenameStore(t, newRenameStore(t))
}

// newRenameStore builds the bare store, so a test can wrap it before
// registering it with the Service.
func newRenameStore(t *testing.T) *badger.BadgerMetadataStore {
	t.Helper()
	return badgertest.NewInMemory(t)
}

// registerRenameStore creates the share root and wires the store into a Service.
func registerRenameStore(t *testing.T, store metadata.Store) (*metadata.Service, metadata.FileHandle, string) {
	t.Helper()
	ctx := context.Background()
	const share = "/rn"
	root, err := store.CreateRootDirectory(ctx, share,
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(share, store))
	return svc, rootHandle, share
}

// TestRenameCtimeRestore_MovePopulatesBothWccTimestamps pins the two timestamps
// Move reports, independently of any restore. They are the only inputs the
// conditional has, so a rename that leaves them zero, swaps them, or reports a
// stamp the store did not take makes the restore either a no-op or a write of
// the wrong value, and no test of the restore itself would say which.
//
// What this deliberately does NOT cover: whether the pre-rename value was read
// inside the rename's transaction or outside it. Absent a concurrent writer the
// two reads return the same bytes — nothing mutates the inode between them and
// the namespace relink does not touch ChangeTime — so the distinction is not
// observable without landing a write inside that window. It matters only under
// concurrency, and it is what keeps an advance committed just before the rename
// from being erased.
func TestRenameCtimeRestore_MovePopulatesBothWccTimestamps(t *testing.T) {
	svc, rootHandle, share := newRenameFixture(t)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "f.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	// Pin the starting ChangeTime so the assertions do not depend on clock
	// resolution. It must be in the past, or the rename's own stamp would not be later than it.
	pinned := time.Date(2001, 4, 5, 6, 7, 8, 123456789, time.UTC)
	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Ctime: &pinned})
	require.NoError(t, err)
	before, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)

	_, wcc, err := svc.Move(root, rootHandle, "f.bin", rootHandle, "g.bin")
	require.NoError(t, err)
	require.NotNil(t, wcc)

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)

	require.False(t, wcc.SourcePreCtime.IsZero(), "SourcePreCtime must be populated")
	require.False(t, wcc.SourceCtime.IsZero(), "SourceCtime must be populated")
	require.True(t, wcc.SourcePreCtime.Equal(before.Ctime),
		"SourcePreCtime = %v; want the ChangeTime held before the rename, %v",
		wcc.SourcePreCtime.UTC(), before.Ctime.UTC())
	require.True(t, wcc.SourceCtime.Equal(after.Ctime),
		"SourceCtime = %v; want the ChangeTime the store holds after the rename, %v",
		wcc.SourceCtime.UTC(), after.Ctime.UTC())
	require.True(t, wcc.SourceCtime.After(wcc.SourcePreCtime),
		"the rename must advance ChangeTime: pre=%v post=%v",
		wcc.SourcePreCtime.UTC(), wcc.SourceCtime.UTC())
}

// rootAuth is the identity every test here runs as: the restore is not
// permission-gated, so nothing in these tests turns on who the caller is.
func rootAuth() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
	}
}
