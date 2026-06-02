package metadata

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/stretchr/testify/require"
)

// removeThenRepublishStore is a white-box test store used to isolate the
// removal-generation guard. It implements lock.LockStore (so the recovery path
// type-asserts it and calls ListLocks) and embeds the MetadataStore interface
// (nil) only to satisfy the static type RegisterStoreForShare expects — the
// register/recovery path never invokes any MetadataStore method.
//
// During the unlocked recovery phase (ListLocks) it fires a RemoveStoreForShare
// for its share AND re-publishes its OWN store pointer into s.stores. Re-publishing
// the same pointer defeats the store-pointer re-check (the entry is present and
// still "ours"), so only the removal-generation bump can reveal that a removal
// landed mid-flight. This is exactly the residual area-7 M1 window: a
// RemoveStoreForShare completing while a RegisterStoreForShare for the same store
// is recovering, with the store pointer re-published in between (e.g. by a racing
// re-add resolving to the same store — the control-plane stores.Service registry
// keeps the same pointer across share remove/re-add).
type removeThenRepublishStore struct {
	MetadataStore // nil; never called during register/recovery
	svc           *MetadataService
	shareName     string
	fired         bool
}

func (s *removeThenRepublishStore) PutLock(context.Context, *lock.PersistedLock) error { return nil }
func (s *removeThenRepublishStore) GetLock(context.Context, string) (*lock.PersistedLock, error) {
	return nil, lock.NewLockNotFoundError("")
}
func (s *removeThenRepublishStore) DeleteLock(context.Context, string) error { return nil }
func (s *removeThenRepublishStore) ListLocks(context.Context, lock.LockQuery) ([]*lock.PersistedLock, error) {
	if !s.fired {
		s.fired = true
		// A concurrent RemoveStoreForShare lands while we recover outside s.mu;
		// it bumps removeGen[shareName].
		s.svc.RemoveStoreForShare(s.shareName)
		// The store pointer is then re-published (same pointer), so the register's
		// pointer re-check still sees "our" store. Only the generation bump reveals
		// that a removal happened.
		s.svc.mu.Lock()
		s.svc.stores[s.shareName] = s
		s.svc.mu.Unlock()
	}
	return nil, nil
}
func (s *removeThenRepublishStore) DeleteLocksByClient(context.Context, string) (int, error) {
	return 0, nil
}
func (s *removeThenRepublishStore) DeleteLocksByFile(context.Context, string) (int, error) {
	return 0, nil
}
func (s *removeThenRepublishStore) GetServerEpoch(context.Context) (uint64, error) { return 0, nil }
func (s *removeThenRepublishStore) IncrementServerEpoch(context.Context) (uint64, error) {
	return 1, nil
}
func (s *removeThenRepublishStore) GetCleanShutdown(context.Context) (bool, error) {
	return true, nil
}
func (s *removeThenRepublishStore) SetCleanShutdown(context.Context, bool) error { return nil }
func (s *removeThenRepublishStore) ReclaimLease(context.Context, lock.FileHandle, [16]byte, string) (*lock.UnifiedLock, error) {
	return nil, lock.NewLockNotFoundError("")
}

// TestRegisterStoreForShare_RemovalGenerationBlocksResurrection is the Finding-M1
// regression. It proves the removal-generation guard aborts the publish even when
// the store-pointer re-check would pass, so a share removed mid-flight is never
// resurrected.
func TestRegisterStoreForShare_RemovalGenerationBlocksResurrection(t *testing.T) {
	const shareName = "/raced-gen"

	svc := New()

	store := &removeThenRepublishStore{svc: svc, shareName: shareName}

	require.NoError(t, svc.RegisterStoreForShare(shareName, store))
	require.True(t, store.fired, "the mid-flight removal must have been exercised")

	// The store pointer was re-published by the decorator, so the pointer re-check
	// alone would have passed. The generation guard must still have aborted the
	// publish: no lock manager / notifier for a removed share.
	require.Nil(t, svc.GetLockManagerForShare(shareName),
		"a share removed mid-flight must NOT have its lock manager resurrected even "+
			"when the store pointer is re-published before the publish re-check")

	svc.mu.RLock()
	_, hasNotifier := svc.dirChangeNotifiers[shareName]
	svc.mu.RUnlock()
	require.False(t, hasNotifier,
		"a share removed mid-flight must NOT have its dirChangeNotifier resurrected")
}
