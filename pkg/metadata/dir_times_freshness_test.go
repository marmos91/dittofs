package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// TestDirTimesOverlayFreshness guards #1573: because a create's parent-directory
// mtime bump is coalesced (not written into the create transaction), every path
// that reports directory attributes must overlay the pending bump so they agree
// with a direct GETATTR. The overlay lives in the Service and is store-agnostic,
// so a memory store exercises it. Assertions compare each path against the
// direct GetFile value (consistency) rather than wall-clock deltas, so they are
// not timing-sensitive.
func TestDirTimesOverlayFreshness(t *testing.T) {
	ctx := context.Background()
	store := memory.NewMemoryMetadataStoreWithDefaults()
	const share = "/test"
	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(share, store))
	auth := &metadata.AuthContext{
		Context: ctx, AuthMethod: "unix",
		Identity: &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
	}

	// A subdirectory "sub", then a file created INSIDE it — the create coalesces
	// sub's mtime bump (unflushed for the 2s interval).
	sub, _, err := svc.CreateDirectory(auth, rootHandle, "sub", &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777})
	require.NoError(t, err)
	subHandle, err := metadata.EncodeShareHandle(share, sub.ID)
	require.NoError(t, err)

	_, _, err = svc.CreateFile(auth, subHandle, "f.txt", &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644})
	require.NoError(t, err)

	// Ground truth: a direct GETATTR of sub (overlay applied).
	fresh, err := svc.GetFile(ctx, subHandle)
	require.NoError(t, err)

	// Issue 1: READDIR of the parent must report sub's fresh mtime in its entry
	// attrs, matching the direct GETATTR (was stale — pre-bump — before the fix).
	page, err := svc.ReadDirectory(auth, rootHandle, 0, 64*1024)
	require.NoError(t, err)
	var subEntry *metadata.DirEntry
	for i := range page.Entries {
		if page.Entries[i].Name == "sub" {
			subEntry = &page.Entries[i]
			break
		}
	}
	require.NotNil(t, subEntry, "sub not found in readdir")
	require.NotNil(t, subEntry.Attr, "sub entry missing attrs")
	require.True(t, subEntry.Attr.Mtime.Equal(fresh.Mtime),
		"readdir entry mtime %v stale vs GETATTR %v (#1573 entry overlay)", subEntry.Attr.Mtime, fresh.Mtime)

	// Issue 2: a mode-only SetAttr on sub must report the fresh (overlaid) mtime
	// in its post-op WCC, matching the direct GETATTR (was stale before the fix).
	mode := uint32(0o750)
	wcc, err := svc.SetFileAttributes(auth, subHandle, &metadata.SetAttrs{Mode: &mode})
	require.NoError(t, err)
	require.NotNil(t, wcc.After)
	require.True(t, wcc.After.Mtime.Equal(fresh.Mtime),
		"setattr wcc.After mtime %v stale vs GETATTR %v (#1573 setattr overlay)", wcc.After.Mtime, fresh.Mtime)
}
