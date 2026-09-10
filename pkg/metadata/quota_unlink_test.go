package metadata_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestRemoveFileReleasesShareUsage walks the production unlink path and pins
// that it gives the share's bytes back.
//
// RemoveFile deliberately does not delete the inode — it drops the directory
// entry and sets nlink to 0, so fstat(2) on a descriptor that is still open
// keeps working. The usage counter therefore cannot key off the row's
// existence, and a quota'd share whose counter only ever rises fills
// permanently: a delete frees no space and the next write is refused.
func TestRemoveFileReleasesShareUsage(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	svc := metadata.New()
	svc.SetDeferredCommit(false)

	const share = "/usage"
	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o777,
	})
	require.NoError(t, err)
	require.NoError(t, svc.RegisterStoreForShare(share, store))
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	auth := &metadata.AuthContext{
		Context: ctx, AuthMethod: "unix",
		Identity: &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0), GIDs: []uint32{0}},
	}

	file, _, err := svc.CreateFile(auth, rootHandle, "big.bin", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, file.ID)
	require.NoError(t, err)

	require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		f, gErr := tx.GetFile(ctx, handle)
		if gErr != nil {
			return gErr
		}
		f.Size = 50 << 20
		return tx.UpdateAttrs(ctx, f)
	}))

	used, err := store.GetUsedBytesForShare(ctx, share)
	require.NoError(t, err)
	require.Equal(t, int64(50<<20), used, "the write must be charged to the share")

	_, _, err = svc.RemoveFile(auth, rootHandle, "big.bin")
	require.NoError(t, err)

	used, err = store.GetUsedBytesForShare(ctx, share)
	require.NoError(t, err)
	require.Zero(t, used, "unlinking the only file must give the share its bytes back")

	// The inode is still there for any descriptor that outlived the unlink.
	_, err = store.GetFile(ctx, handle)
	require.NoError(t, err, "RemoveFile must keep the inode for open descriptors")

	// A quota that the stale figure would have filled still admits a write.
	svc.SetQuotaForShare(share, 60<<20)
	next, _, err := svc.CreateFile(auth, rootHandle, "next.bin", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)
	nextHandle, err := metadata.EncodeShareHandle(share, next.ID)
	require.NoError(t, err)
	_, err = svc.PrepareWrite(auth, nextHandle, 50<<20)
	require.NoError(t, err, "the share is empty again, so a write that fits the quota must be admitted")
}
