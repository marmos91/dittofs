package lock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGracePeriod_EarlyExitWhenAllReclaim asserts the grace window exits before
// its timer fires once every expected client has reclaimed.
func TestGracePeriod_EarlyExitWhenAllReclaim(t *testing.T) {
	done := make(chan struct{}, 1)
	gpm := NewGracePeriodManager(time.Hour, func() { done <- struct{}{} })

	gpm.EnterGracePeriod([]string{"client-1", "client-2"})
	require.Equal(t, GraceStateActive, gpm.GetState())

	gpm.MarkReclaimed("client-1")
	require.Equal(t, GraceStateActive, gpm.GetState(),
		"grace must stay active until ALL expected clients reclaim")

	gpm.MarkReclaimed("client-2")
	require.Equal(t, GraceStateNormal, gpm.GetState(),
		"grace must exit early once all expected clients have reclaimed")

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onGraceEnd was not invoked on early exit")
	}
}

// TestReclaimLease_MarksClientReclaimed pins Phase-2 item 4 (SMB side): a
// successful lease reclaim during grace must MarkReclaimed the owning client so
// grace can exit early once every expected client has recovered.
func TestReclaimLease_MarksClientReclaimed(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	gpm := NewGracePeriodManager(time.Hour, nil)
	lm := NewManagerWithGracePeriod(gpm)
	lm.SetLockStore(store)
	lm.SetShareName("share-a")

	// Persist a lease held by client-1 before the restart.
	leaseKey := [16]byte{1, 2, 3, 4}
	persisted := &PersistedLock{
		ID:         "lease-1",
		ShareName:  "share-a",
		FileID:     "share-a:dir-1",
		ClientID:   "client-1",
		LeaseKey:   leaseKey[:],
		LeaseState: LeaseStateRead | LeaseStateHandle,
	}
	require.NoError(t, store.PutLock(ctx, persisted))

	// Enter grace expecting client-1 to reclaim.
	lm.EnterGracePeriod([]string{"client-1"})
	require.True(t, lm.IsInGracePeriod(), "grace must be active after EnterGracePeriod")

	// Reclaim the lease.
	_, err := lm.ReclaimLease(ctx, leaseKey, LeaseStateRead, false)
	require.NoError(t, err, "lease reclaim during grace must succeed")

	// The reclaim must have recorded the client; with the only expected client
	// reclaimed, grace exits early.
	require.Equal(t, GraceStateNormal, gpm.GetState(),
		"reclaiming the sole expected client's lease must MarkReclaimed and end grace early")
}
