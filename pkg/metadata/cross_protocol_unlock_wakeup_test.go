package metadata_test

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// TestSetByteRangeReleaseHook_FiresOnSMBUnlock proves the cross-protocol
// blocked-waiter wakeup seam end-to-end through the MetadataService: the hook
// registered via SetByteRangeReleaseHook is stamped onto the per-share lock
// manager at registration, and an SMB-style byte-range UnlockFile fires it with
// the correct handle key. In production the NFS adapter wires this hook to
// processNLMWaiters so an NLM F_SETLKW waiter blocked on the released SMB lock
// is woken (NLM uses a server-driven NLM_GRANTED callback, not poll-retry).
//
// This is the regression guard for the cross-protocol NLM-waiter starvation
// (area #5 H-1): before the fix the SMB UNLOCK path had no hook into the NLM
// drain, so an NLM waiter blocked on an SMB lock hung indefinitely.
func TestSetByteRangeReleaseHook_FiresOnSMBUnlock(t *testing.T) {
	t.Parallel()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	const shareName = "/xproto"

	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	require.NoError(t, err)

	svc := metadata.New()

	// Register the hook BEFORE the share so it is stamped onto the manager at
	// creation (no UNLOCK can race past the manager becoming observable without
	// the hook).
	var (
		mu    sync.Mutex
		fired []string
	)
	svc.SetByteRangeReleaseHook(func(handleKey string) {
		mu.Lock()
		fired = append(fired, handleKey)
		mu.Unlock()
	})

	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: metadata.Uint32Ptr(0),
			GID: metadata.Uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	file, err := svc.CreateFile(authCtx, rootHandle, "locked.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle, err := metadata.EncodeFileHandle(file)
	require.NoError(t, err)
	handleKey := string(handle)

	// Acquire an SMB-style byte-range lock (OpenID + SessionID set).
	const sessionID = uint64(7)
	require.NoError(t, svc.LockFile(authCtx, handle, metadata.FileLock{
		ID:        1,
		OpenID:    "smb-open-1",
		SessionID: sessionID,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}))

	// The SMB UNLOCK path must fire the cross-protocol release hook so blocked
	// NLM waiters on this handle are re-driven.
	require.NoError(t, svc.UnlockFile(ctx, handle, "smb-open-1", sessionID, 0, 100))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{handleKey}, fired,
		"SMB UnlockFile must fire the byte-range release hook with the handle key")
}
