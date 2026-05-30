package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// persistNLMLock writes a single NLM/unified byte-range lock into the store so a
// subsequent RegisterStoreForShare recovers it and treats its client as an
// expected reclaimer.
func persistNLMLock(t *testing.T, store *memory.MemoryMetadataStore, shareName, clientID string) {
	t.Helper()
	require.NoError(t, store.PutLock(context.Background(), &lock.PersistedLock{
		ID:        "nlm-lock-1",
		ShareName: shareName,
		FileID:    shareName + ":file-1",
		OwnerID:   "nlm:" + clientID,
		ClientID:  clientID,
		LockType:  int(lock.LockTypeExclusive),
		Offset:    0,
		Length:    100,
	}))
}

// TestRegisterStoreForShare_EntersGraceWhenPersistedLocksExist asserts that a
// fresh service registering a store that carries persisted locks enters the
// grace period with the persisted clients as the expected reclaim roster.
func TestRegisterStoreForShare_EntersGraceWhenPersistedLocksExist(t *testing.T) {
	const shareName = "/graced"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	// Simulate a previous run that left an NLM lock for client-1 persisted.
	persistNLMLock(t, store, shareName, "client-1")

	// Fresh service: register the store carrying the persisted lock.
	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm, "lock manager must exist for the registered share")
	require.True(t, lm.IsInGracePeriod(),
		"a share recovering persisted locks must enter the grace period")
	require.ElementsMatch(t, []string{"client-1"}, lm.GetExpectedClients(),
		"the expected reclaim roster must be the persisted clients")
}

// TestRegisterStoreForShare_NoGraceWhenNoPersistedLocks asserts a fresh server
// with no persisted locks starts in normal operation (no grace).
func TestRegisterStoreForShare_NoGraceWhenNoPersistedLocks(t *testing.T) {
	const shareName = "/fresh"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.False(t, lm.IsInGracePeriod(),
		"a fresh server with no persisted locks must NOT enter grace")
}

// graceSpyCoordinator records the lock-manager grace start/end callbacks and,
// for the two-machine coordination test, drives a v4-like grace machine in
// lockstep.
type graceSpyCoordinator struct {
	started chan []string
	ended   chan struct{}
	onStart func(expected []string)
	onEnd   func()
}

func (c *graceSpyCoordinator) OnLockGraceStart(expected []string) {
	if c.onStart != nil {
		c.onStart(expected)
	}
	select {
	case c.started <- expected:
	default:
	}
}

func (c *graceSpyCoordinator) OnLockGraceEnd() {
	if c.onEnd != nil {
		c.onEnd()
	}
	select {
	case c.ended <- struct{}{}:
	default:
	}
}

// TestGracePeriod_BothMachinesEnterTogether asserts the GraceCoordinator fires
// OnLockGraceStart when a share's lock-manager grace begins, so the NFS adapter
// can drive the SEPARATE NFSv4 StateManager grace machine into grace in
// lockstep. A second grace machine wired through the coordinator must also be
// active once registration completes.
func TestGracePeriod_BothMachinesEnterTogether(t *testing.T) {
	const shareName = "/coord"
	store := memory.NewMemoryMetadataStoreWithDefaults()
	persistNLMLock(t, store, shareName, "client-1")

	// A stand-in for the NFSv4 grace machine, flipped on by the coordinator.
	var secondMachineActive bool
	coord := &graceSpyCoordinator{
		started: make(chan []string, 1),
		ended:   make(chan struct{}, 1),
		onStart: func(_ []string) { secondMachineActive = true },
		onEnd:   func() { secondMachineActive = false },
	}

	svc := metadata.New()
	svc.SetGraceCoordinator(coord)
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.True(t, lm.IsInGracePeriod(), "lock-manager grace must be active")

	// The coordinator must have been driven at registration time.
	select {
	case expected := <-coord.started:
		require.ElementsMatch(t, []string{"client-1"}, expected,
			"coordinator must receive the same expected reclaim roster")
	case <-time.After(time.Second):
		t.Fatal("OnLockGraceStart was not fired when lock-manager grace began")
	}
	require.True(t, secondMachineActive,
		"the second grace machine must enter grace together with the lock manager")
}
