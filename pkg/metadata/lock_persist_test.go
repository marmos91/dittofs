package metadata_test

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// blockingRecoveryStore embeds a memory store but blocks ListLocks until a
// signal is received, widening the recovery window deterministically. It
// satisfies both MetadataStore and lock.LockStore via embedding.
type blockingRecoveryStore struct {
	*memory.MemoryMetadataStore
	listEntered chan struct{} // closed when ListLocks is entered
	release     chan struct{} // ListLocks returns once this is closed
}

func (s *blockingRecoveryStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	close(s.listEntered)
	<-s.release
	return s.MemoryMetadataStore.ListLocks(ctx, query)
}

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

// TestRegisterStoreForShare_RecoversEmptyShareNameLocks pins R2 end-to-end:
// NFSv4/NLM byte-range producers build LockOwner with ShareName="" (the
// byte-range path never carries it). The manager must stamp its own share
// name at persist time so the per-share recovery query finds the lock on
// restart. Without the stamp, NFSv4 byte-range locks are silently dropped.
func TestRegisterStoreForShare_RecoversEmptyShareNameLocks(t *testing.T) {
	const shareName = "/recover-empty"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	svc1 := metadata.New()
	require.NoError(t, svc1.RegisterStoreForShare(shareName, store))
	lm1 := svc1.GetLockManagerForShare(shareName)
	require.NotNil(t, lm1)

	const handleKey = "/recover-empty:file-nfs4"
	ul := &metadata.UnifiedLock{
		ID: "nfs4-empty-share",
		Owner: metadata.LockOwner{
			OwnerID:   "nfs4:1:deadbeef",
			ClientID:  "nfs4:1",
			ShareName: "", // NFSv4 producer leaves this empty.
		},
		FileHandle: lock.FileHandle(handleKey),
		Type:       metadata.LockTypeExclusive,
	}
	require.NoError(t, lm1.AddUnifiedLock(handleKey, ul))

	svc2 := metadata.New()
	require.NoError(t, svc2.RegisterStoreForShare(shareName, store))
	lm2 := svc2.GetLockManagerForShare(shareName)
	require.NotNil(t, lm2)

	recovered := lm2.ListUnifiedLocks(handleKey)
	require.Len(t, recovered, 1, "empty-ShareName lock must recover via per-share query")
}

// TestRegisterStoreForShare_ManagerObservableOnlyAfterRecovery pins R5: the
// lock manager must not become observable via GetLockManagerForShare until
// recovery (ListLocks + RestoreLocks) has completed. Otherwise a concurrent
// reader could see an empty, unrecovered manager and grant a lock that
// conflicts with a not-yet-restored one. We persist a lock on a first server,
// then race GetLockManagerForShare against RegisterStoreForShare on a fresh
// server: every non-nil manager observed must already carry the recovered lock.
func TestRegisterStoreForShare_ManagerObservableOnlyAfterRecovery(t *testing.T) {
	const shareName = "/recover-race"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	svc1 := metadata.New()
	require.NoError(t, svc1.RegisterStoreForShare(shareName, store))
	lm1 := svc1.GetLockManagerForShare(shareName)
	require.NotNil(t, lm1)

	const handleKey = "/recover-race:file-1"
	ul := &metadata.UnifiedLock{
		ID: "race-lock-1",
		Owner: metadata.LockOwner{
			OwnerID:   "nlm:client-r",
			ClientID:  "client-r",
			ShareName: shareName,
		},
		FileHandle: lock.FileHandle(handleKey),
		Type:       metadata.LockTypeExclusive,
	}
	require.NoError(t, lm1.AddUnifiedLock(handleKey, ul))

	// Fresh server over a store whose ListLocks blocks mid-recovery, so the
	// recovery window is wide and deterministic.
	blocking := &blockingRecoveryStore{
		MemoryMetadataStore: store,
		listEntered:         make(chan struct{}),
		release:             make(chan struct{}),
	}
	svc2 := metadata.New()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NoError(t, svc2.RegisterStoreForShare(shareName, blocking))
	}()

	// Wait until recovery is mid-flight (inside ListLocks). At this point a
	// correctly-ordered implementation must NOT yet expose the manager.
	<-blocking.listEntered
	require.Nil(t, svc2.GetLockManagerForShare(shareName),
		"lock manager must not be observable until recovery completes")

	// Let recovery finish, then the manager appears fully recovered.
	close(blocking.release)
	wg.Wait()

	lm2 := svc2.GetLockManagerForShare(shareName)
	require.NotNil(t, lm2)
	require.Len(t, lm2.ListUnifiedLocks(handleKey), 1)
}
