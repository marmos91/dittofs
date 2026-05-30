package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// TestRegisterStoreForShare_RecoversPersistedLocks verifies the end-to-end
// production wiring: a lock acquired on one service is persisted to the backing
// store and recovered into a fresh service's lock manager on re-registration
// (simulating a server restart against the same durable store).
func TestRegisterStoreForShare_RecoversPersistedLocks(t *testing.T) {
	const shareName = "/recover"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	// First server: register and acquire a unified lock.
	svc1 := metadata.New()
	require.NoError(t, svc1.RegisterStoreForShare(shareName, store))

	lm1 := svc1.GetLockManagerForShare(shareName)
	require.NotNil(t, lm1)

	const handleKey = "/recover:file-1"
	ul := &metadata.UnifiedLock{
		ID: "lock-recover-1",
		Owner: metadata.LockOwner{
			OwnerID:   "nlm:client-z",
			ClientID:  "client-z",
			ShareName: shareName,
		},
		FileHandle: lock.FileHandle(handleKey),
		Type:       metadata.LockTypeExclusive,
	}
	require.NoError(t, lm1.AddUnifiedLock(handleKey, ul))

	// Second server: a fresh service over the same store must recover the lock
	// on registration.
	svc2 := metadata.New()
	require.NoError(t, svc2.RegisterStoreForShare(shareName, store))

	lm2 := svc2.GetLockManagerForShare(shareName)
	require.NotNil(t, lm2)

	recovered := lm2.ListUnifiedLocks(handleKey)
	require.Len(t, recovered, 1, "lock should be recovered after restart")
	require.Equal(t, "nlm:client-z", recovered[0].Owner.OwnerID)
}
