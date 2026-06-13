package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// observeDuringRecoveryStore intercepts ListLocks (called during lock-manager
// recovery, outside s.mu) and records whether the store was already observable
// via GetStoreForShare at that moment, and whether a lock manager was also
// simultaneously present via GetLockManagerForShare.
//
// Before the fix: GetStoreForShare succeeds (store was published prematurely
// in the first lock block) but GetLockManagerForShare returns nil (LM not yet
// published). After the fix: both return their zero/error value because neither
// is published until the final atomic block.
type observeDuringRecoveryStore struct {
	*memory.MemoryMetadataStore
	svc                     *metadata.Service
	shareName               string
	storeSeenDuringRecovery bool
	lmSeenDuringRecovery    bool
	observed                bool
}

func (s *observeDuringRecoveryStore) ListLocks(ctx context.Context, q lock.LockQuery) ([]*lock.PersistedLock, error) {
	if !s.observed {
		s.observed = true
		_, err := s.svc.GetStoreForShare(s.shareName)
		s.storeSeenDuringRecovery = (err == nil)
		s.lmSeenDuringRecovery = (s.svc.GetLockManagerForShare(s.shareName) != nil)
	}
	return s.MemoryMetadataStore.ListLocks(ctx, q)
}

// TestRegisterStoreForShare_LockManagerReadyWhenStoreVisible asserts that the
// lock manager is never absent while the store is visible: the two are published
// atomically so there is no window where lockManagerForHandle returns
// ErrStaleHandle for an otherwise live, registered share.
//
// Fails BEFORE fix: storeSeenDuringRecovery == true, lmSeenDuringRecovery == false.
// Passes AFTER fix: storeSeenDuringRecovery == false, lmSeenDuringRecovery == false
//
//	(neither is visible during the recovery window).
func TestRegisterStoreForShare_LockManagerReadyWhenStoreVisible(t *testing.T) {
	const shareName = "/atomic-publish"
	base := memory.NewMemoryMetadataStoreWithDefaults()

	svc := metadata.New()
	spy := &observeDuringRecoveryStore{
		MemoryMetadataStore: base,
		svc:                 svc,
		shareName:           shareName,
	}

	require.NoError(t, svc.RegisterStoreForShare(shareName, spy))
	require.True(t, spy.observed, "ListLocks hook must have been exercised during recovery")

	// Core assertion: if the store was visible during recovery, the LM must also
	// have been visible. A store-visible-but-no-LM state is the bug.
	if spy.storeSeenDuringRecovery {
		require.True(t, spy.lmSeenDuringRecovery,
			"BUG: store was observable via GetStoreForShare during lock-manager "+
				"recovery but GetLockManagerForShare returned nil — lockManagerForHandle "+
				"would have returned ErrStaleHandle for a live share")
	}

	// After registration completes, both must be present.
	gotStore, err := svc.GetStoreForShare(shareName)
	require.NoError(t, err)
	require.NotNil(t, gotStore, "store must be present after RegisterStoreForShare returns")
	require.NotNil(t, svc.GetLockManagerForShare(shareName),
		"lock manager must be present after RegisterStoreForShare returns")
}
