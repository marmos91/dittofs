package metadata_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestSharedStoreQuotaBleed pins that a per-share quota is enforced against the
// share's own usage, not the totals of every share co-located in the same
// metadata store instance.
func TestSharedStoreQuotaBleed(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	svc := metadata.New()
	svc.SetDeferredCommit(false)

	mk := func(share string) metadata.FileHandle {
		root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
			Type: metadata.FileTypeDirectory, Mode: 0o777,
		})
		require.NoError(t, err)
		require.NoError(t, svc.RegisterStoreForShare(share, store))
		h, err := metadata.EncodeShareHandle(share, root.ID)
		require.NoError(t, err)
		return h
	}
	rootA := mk("/a")
	rootB := mk("/b")

	auth := &metadata.AuthContext{
		Context: ctx, AuthMethod: "unix",
		Identity: &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0), GIDs: []uint32{0}},
	}
	create := func(root metadata.FileHandle, share, name string) metadata.FileHandle {
		f, _, err := svc.CreateFile(auth, root, name, &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644})
		require.NoError(t, err)
		h, err := metadata.EncodeShareHandle(share, f.ID)
		require.NoError(t, err)
		return h
	}

	// Fill share A with 9000 bytes. Share A has no quota.
	fa := create(rootA, "/a", "big.bin")
	require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		f, err := tx.GetFile(ctx, fa)
		if err != nil {
			return err
		}
		f.Size = 9000
		return tx.UpdateAttrs(ctx, f)
	}))

	perA, err := store.GetUsedBytesForShare(ctx, "/a")
	require.NoError(t, err)
	perB, err := store.GetUsedBytesForShare(ctx, "/b")
	require.NoError(t, err)
	t.Logf("perShare(/a)=%d perShare(/b)=%d", perA, perB)

	// Share B is empty and has a 4000-byte quota. A 1000-byte write fits.
	svc.SetQuotaForShare("/b", 4000)
	fb := create(rootB, "/b", "small.bin")
	_, err = svc.PrepareWrite(auth, fb, 1000)
	require.NoError(t, err, "share /b is empty with a 4000-byte quota; a 1000-byte write must fit")
}
