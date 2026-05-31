package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// TestRemoveStoreForShare_DeregistersAllMaps asserts that RemoveStoreForShare
// drops the share from every per-share map (store, lock manager, unified view,
// quota) so removed shares no longer route and the maps do not grow unbounded
// across add/remove churn.
func TestRemoveStoreForShare_DeregistersAllMaps(t *testing.T) {
	const shareName = "/churn"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))
	svc.SetQuotaForShare(shareName, 4096)

	// Sanity: everything is registered.
	gotStore, err := svc.GetStoreForShare(shareName)
	require.NoError(t, err)
	require.NotNil(t, gotStore)
	require.NotNil(t, svc.GetLockManagerForShare(shareName))
	require.Equal(t, int64(4096), svc.GetQuotaForShare(shareName))

	svc.RemoveStoreForShare(shareName)

	// Store routing is gone.
	_, err = svc.GetStoreForShare(shareName)
	require.Error(t, err, "removed share must not resolve to a live store")

	// Lock manager is gone.
	require.Nil(t, svc.GetLockManagerForShare(shareName),
		"removed share must not retain a lock manager")

	// Unified view is gone.
	require.Nil(t, svc.GetUnifiedLockView(shareName),
		"removed share must not retain a unified lock view")

	// Quota entry is gone (GetQuotaForShare returns the zero value for a
	// missing key, same as unlimited).
	require.Equal(t, int64(0), svc.GetQuotaForShare(shareName),
		"removed share must not retain a quota entry")
}

// TestRemoveStoreForShare_ReAddGetsFreshLockManager asserts that after a
// remove, re-adding a same-name share gets a FRESH lock manager rather than
// silently reusing the stale one (RegisterStoreForShare early-returns when a
// lock manager already exists, so a leaked entry would be reused).
//
// This is the regression that fails before the fix: without
// RemoveStoreForShare, the second RegisterStoreForShare sees the surviving
// lockManagers[name] entry and returns the SAME *LockManager.
func TestRemoveStoreForShare_ReAddGetsFreshLockManager(t *testing.T) {
	const shareName = "/reused"

	svc := metadata.New()

	store1 := memory.NewMemoryMetadataStoreWithDefaults()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store1))
	lm1 := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm1)

	svc.RemoveStoreForShare(shareName)

	store2 := memory.NewMemoryMetadataStoreWithDefaults()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store2))
	lm2 := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm2)

	require.NotSame(t, lm1, lm2,
		"re-adding a same-name share must allocate a fresh lock manager, not reuse the stale one")

	// And the re-added store must be the new one, not the stale routing target.
	gotStore, err := svc.GetStoreForShare(shareName)
	require.NoError(t, err)
	require.Same(t, store2, gotStore,
		"re-added share must route to the newly registered store")
}

// TestRemoveStoreForShare_Idempotent asserts removing an unregistered or
// already-removed share is a harmless no-op (no panic, no nil deref).
func TestRemoveStoreForShare_Idempotent(t *testing.T) {
	svc := metadata.New()

	// Never registered.
	require.NotPanics(t, func() { svc.RemoveStoreForShare("/never") })

	// Registered then removed twice.
	const shareName = "/twice"
	store := memory.NewMemoryMetadataStoreWithDefaults()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))
	require.NotPanics(t, func() { svc.RemoveStoreForShare(shareName) })
	require.NotPanics(t, func() { svc.RemoveStoreForShare(shareName) })
}
