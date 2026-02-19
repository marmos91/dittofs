package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSUIDClearingViaSetAttr tests the exact chmod/12.t scenario:
// Root creates file with mode 04777, non-owner (uid 65534) sends
// SETATTR(mode=0777) to clear SUID bits (Linux NFS client file_remove_privs).
func TestSUIDClearingViaSetAttr(t *testing.T) {
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

	// uid 65534 with write permission (mode 04777 = world-writable)
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

	// 1. Root creates file with mode 04777 (SUID + world-writable)
	_, err = svc.CreateFile(rootCtx, rootHandle, "suidfile", &metadata.FileAttr{
		Mode: 0o4777,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err)

	handle, err := store.GetChild(ctx, rootHandle, "suidfile")
	require.NoError(t, err)

	// Verify file was created with SUID bit
	file, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	t.Logf("File mode after creation: %04o (UID=%d, GID=%d)", file.Mode, file.UID, file.GID)
	assert.Equal(t, uint32(0o4777), file.Mode, "file should have SUID bit after creation by root")

	// 2. Simulate Linux NFS client file_remove_privs(): SETATTR(mode = 0777)
	// This is what the client sends before WRITE when SUID bits are set
	clearedMode := uint32(0o0777)
	err = svc.SetFileAttributes(nobodyCtx, handle, &metadata.SetAttrs{
		Mode: &clearedMode,
	})
	t.Logf("SetFileAttributes error: %v", err)

	// This MUST succeed - non-owner with write permission clearing SUID/SGID
	require.NoError(t, err, "non-owner with write permission should be allowed to clear SUID/SGID bits")

	// 3. Verify SUID bits were cleared
	file, err = store.GetFile(ctx, handle)
	require.NoError(t, err)
	t.Logf("File mode after SETATTR: %04o", file.Mode)
	assert.Equal(t, uint32(0o0777), file.Mode, "SUID bits should be cleared")

	// 4. Also test SGID (02777 -> 0777)
	_, err = svc.CreateFile(rootCtx, rootHandle, "sgidfile", &metadata.FileAttr{
		Mode: 0o2777,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err)

	handle2, err := store.GetChild(ctx, rootHandle, "sgidfile")
	require.NoError(t, err)

	file2, err := store.GetFile(ctx, handle2)
	require.NoError(t, err)
	t.Logf("SGID file mode after creation: %04o", file2.Mode)

	sgidCleared := uint32(0o0777)
	err = svc.SetFileAttributes(nobodyCtx, handle2, &metadata.SetAttrs{
		Mode: &sgidCleared,
	})
	t.Logf("SGID SetFileAttributes error: %v", err)
	require.NoError(t, err, "non-owner with write permission should be allowed to clear SGID bits")

	file2, err = store.GetFile(ctx, handle2)
	require.NoError(t, err)
	t.Logf("SGID file mode after SETATTR: %04o", file2.Mode)
	assert.Equal(t, uint32(0o0777), file2.Mode, "SGID bits should be cleared")

	// 5. Test SUID+SGID combo (06777 -> 0777)
	_, err = svc.CreateFile(rootCtx, rootHandle, "susgidfile", &metadata.FileAttr{
		Mode: 0o6777,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err)

	handle3, err := store.GetChild(ctx, rootHandle, "susgidfile")
	require.NoError(t, err)

	bothCleared := uint32(0o0777)
	err = svc.SetFileAttributes(nobodyCtx, handle3, &metadata.SetAttrs{
		Mode: &bothCleared,
	})
	require.NoError(t, err, "non-owner should be allowed to clear both SUID+SGID bits")

	file3, err := store.GetFile(ctx, handle3)
	require.NoError(t, err)
	assert.Equal(t, uint32(0o0777), file3.Mode, "both SUID+SGID should be cleared")
}
