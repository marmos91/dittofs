package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSUIDClearingOnWrite tests the server-side SUID clearing during WRITE.
// This simulates the exact NFSv4 scenario where the client does NOT send
// SETATTR before WRITE, and the server must clear SUID/SGID in CommitWrite.
func TestSUIDClearingOnWrite(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName := "/test"

	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0777,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err)

	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	svc := metadata.New()
	err = svc.RegisterStoreForShare(shareName, store)
	require.NoError(t, err)

	rootCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}

	nobodyCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(65534),
			GID:  metadata.Uint32Ptr(65534),
			GIDs: []uint32{65534},
		},
		ClientAddr: "127.0.0.1",
	}

	t.Run("SUID cleared on non-root write via deferred commit", func(t *testing.T) {
		// 1. Root creates file with SUID + world-writable
		_, err := svc.CreateFile(rootCtx, rootHandle, "suidfile", &metadata.FileAttr{
			Mode: 0o4777,
			UID:  0,
			GID:  0,
		})
		require.NoError(t, err)

		handle, err := store.GetChild(ctx, rootHandle, "suidfile")
		require.NoError(t, err)

		// Verify initial mode
		file, err := store.GetFile(ctx, handle)
		require.NoError(t, err)
		assert.Equal(t, uint32(0o4777), file.Mode, "initial mode should be 04777")

		// 2. Non-root writes to the file (server-side SUID clearing)
		intent, err := svc.PrepareWrite(nobodyCtx, handle, 1024)
		require.NoError(t, err)
		t.Logf("PrepareWrite PreWriteAttr.Mode: %04o", intent.PreWriteAttr.Mode)

		resultFile, err := svc.CommitWrite(nobodyCtx, intent)
		require.NoError(t, err)
		t.Logf("CommitWrite returned Mode: %04o", resultFile.Mode)

		// 3. Verify mode is cleared in CommitWrite result
		assert.Equal(t, uint32(0o0777), resultFile.Mode, "CommitWrite should return mode with SUID cleared")

		// 4. Verify mode is cleared in GetFile (what GETATTR would return)
		file2, err := svc.GetFile(ctx, handle)
		require.NoError(t, err)
		t.Logf("GetFile Mode: %04o", file2.Mode)
		assert.Equal(t, uint32(0o0777), file2.Mode, "GetFile should return mode with SUID cleared")

		// 5. Verify mode is cleared in raw store
		file3, err := store.GetFile(ctx, handle)
		require.NoError(t, err)
		t.Logf("Raw store Mode: %04o", file3.Mode)
		assert.Equal(t, uint32(0o0777), file3.Mode, "Store should have mode with SUID cleared")
	})

	t.Run("SGID cleared on non-root write via deferred commit", func(t *testing.T) {
		_, err := svc.CreateFile(rootCtx, rootHandle, "sgidfile", &metadata.FileAttr{
			Mode: 0o2777,
			UID:  0,
			GID:  0,
		})
		require.NoError(t, err)

		handle, err := store.GetChild(ctx, rootHandle, "sgidfile")
		require.NoError(t, err)

		intent, err := svc.PrepareWrite(nobodyCtx, handle, 1024)
		require.NoError(t, err)

		resultFile, err := svc.CommitWrite(nobodyCtx, intent)
		require.NoError(t, err)
		assert.Equal(t, uint32(0o0777), resultFile.Mode, "SGID should be cleared on non-root write")

		file, err := svc.GetFile(ctx, handle)
		require.NoError(t, err)
		assert.Equal(t, uint32(0o0777), file.Mode, "GetFile should return cleared SGID")
	})

	t.Run("SUID+SGID cleared on non-root write via deferred commit", func(t *testing.T) {
		_, err := svc.CreateFile(rootCtx, rootHandle, "bothfile", &metadata.FileAttr{
			Mode: 0o6777,
			UID:  0,
			GID:  0,
		})
		require.NoError(t, err)

		handle, err := store.GetChild(ctx, rootHandle, "bothfile")
		require.NoError(t, err)

		intent, err := svc.PrepareWrite(nobodyCtx, handle, 1024)
		require.NoError(t, err)

		resultFile, err := svc.CommitWrite(nobodyCtx, intent)
		require.NoError(t, err)
		assert.Equal(t, uint32(0o0777), resultFile.Mode, "Both SUID+SGID should be cleared")

		file, err := svc.GetFile(ctx, handle)
		require.NoError(t, err)
		assert.Equal(t, uint32(0o0777), file.Mode, "GetFile should return cleared both bits")
	})

	t.Run("Root write does NOT clear SUID", func(t *testing.T) {
		_, err := svc.CreateFile(rootCtx, rootHandle, "rootwrite", &metadata.FileAttr{
			Mode: 0o4777,
			UID:  0,
			GID:  0,
		})
		require.NoError(t, err)

		handle, err := store.GetChild(ctx, rootHandle, "rootwrite")
		require.NoError(t, err)

		intent, err := svc.PrepareWrite(rootCtx, handle, 1024)
		require.NoError(t, err)

		resultFile, err := svc.CommitWrite(rootCtx, intent)
		require.NoError(t, err)
		assert.Equal(t, uint32(0o4777), resultFile.Mode, "Root write should NOT clear SUID")
	})
}
