package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBatchA_Move_DescendantPathUpdate verifies that a directory rename updates
// the persisted Path of descendant files. The fix propagates tx.PutFile errors
// inside updateDescendantPaths instead of silently discarding them; this
// regression guard confirms the success path persists the new descendant path.
func TestBatchA_Move_DescendantPathUpdate(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	_, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "parent", &metadata.FileAttr{Mode: 0o755})
	require.NoError(t, err)

	parentHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "parent")
	require.NoError(t, err)

	_, _, err = fx.service.CreateFile(fx.rootContext(), parentHandle, "child.txt", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)

	childHandle, err := fx.store.GetChild(ctx, parentHandle, "child.txt")
	require.NoError(t, err)

	childBefore, err := fx.store.GetFile(ctx, childHandle)
	require.NoError(t, err)
	require.Equal(t, "/parent/child.txt", childBefore.Path)

	_, err = fx.service.Move(fx.rootContext(), fx.rootHandle, "parent", fx.rootHandle, "renamed")
	require.NoError(t, err, "rename of directory with children must succeed")

	childAfter, err := fx.store.GetFile(ctx, childHandle)
	require.NoError(t, err)
	assert.Equal(t, "/renamed/child.txt", childAfter.Path, "descendant path must be updated after directory rename")
}

// TestBatchA_SetFileAttributes_AtimeNow verifies AtimeNow is applied.
func TestBatchA_SetFileAttributes_AtimeNow(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "f.txt", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	fh, err := fx.store.GetChild(ctx, fx.rootHandle, "f.txt")
	require.NoError(t, err)

	before, err := fx.store.GetFile(ctx, fh)
	require.NoError(t, err)
	oldAtime := before.Atime

	time.Sleep(2 * time.Millisecond)

	_, err = fx.service.SetFileAttributes(fx.rootContext(), fh, &metadata.SetAttrs{AtimeNow: true})
	require.NoError(t, err)

	after, err := fx.store.GetFile(ctx, fh)
	require.NoError(t, err)
	assert.True(t, after.Atime.After(oldAtime), "AtimeNow must update Atime to current time")
}

// TestBatchA_SetFileAttributes_MtimeNow verifies MtimeNow is applied.
func TestBatchA_SetFileAttributes_MtimeNow(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "f.txt", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	fh, err := fx.store.GetChild(ctx, fx.rootHandle, "f.txt")
	require.NoError(t, err)

	before, err := fx.store.GetFile(ctx, fh)
	require.NoError(t, err)
	oldMtime := before.Mtime

	time.Sleep(2 * time.Millisecond)

	_, err = fx.service.SetFileAttributes(fx.rootContext(), fh, &metadata.SetAttrs{MtimeNow: true})
	require.NoError(t, err)

	after, err := fx.store.GetFile(ctx, fh)
	require.NoError(t, err)
	assert.True(t, after.Mtime.After(oldMtime), "MtimeNow must update Mtime to current time")
}

// TestBatchA_PrepareWrite_DOSReadonlyDeniesOwner verifies the DOS READONLY bit
// is enforced even for the file owner, and that removing it restores access.
func TestBatchA_PrepareWrite_DOSReadonlyDeniesOwner(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	const modeDOSReadonly = uint32(0x100000)

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "ro.txt", &metadata.FileAttr{
		Mode: 0o644 | modeDOSReadonly,
		UID:  1000,
		GID:  1000,
	})
	require.NoError(t, err)

	fh, err := fx.store.GetChild(ctx, fx.rootHandle, "ro.txt")
	require.NoError(t, err)

	ownerCtx := fx.authContext(1000, 1000)
	_, err = fx.service.PrepareWrite(ownerCtx, fh, 512)
	require.Error(t, err, "owner write to DOS READONLY file must be denied")
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, metadata.ErrAccessDenied, storeErr.Code)

	// Root bypasses DOS READONLY.
	_, err = fx.service.PrepareWrite(fx.rootContext(), fh, 512)
	require.NoError(t, err, "root must bypass DOS READONLY")

	// A file without the READONLY bit allows owner write.
	_, _, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "rw.txt", &metadata.FileAttr{
		Mode: 0o644,
		UID:  1000,
		GID:  1000,
	})
	require.NoError(t, err)
	fh2, err := fx.store.GetChild(ctx, fx.rootHandle, "rw.txt")
	require.NoError(t, err)
	_, err = fx.service.PrepareWrite(ownerCtx, fh2, 512)
	require.NoError(t, err, "owner write to non-READONLY file must succeed")
}
