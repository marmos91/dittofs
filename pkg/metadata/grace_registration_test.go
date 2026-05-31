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

// TestRegisterStoreForShare_NoGraceAfterCleanDrain asserts the fast path: a
// store whose previous run recorded a clean shutdown (clean marker == true) and
// has NO persisted locks starts in normal operation (no grace). This is the
// only path that skips grace under the H7 predicate (unclean OR locks>0).
func TestRegisterStoreForShare_NoGraceAfterCleanDrain(t *testing.T) {
	const shareName = "/fresh"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	// Simulate a previous graceful Close(): the clean-shutdown marker is set.
	require.NoError(t, store.SetCleanShutdown(context.Background(), true))

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.False(t, lm.IsInGracePeriod(),
		"a cleanly-drained server with no persisted locks must NOT enter grace")

	// And the boot path must have cleared the marker for the running session,
	// so a kill -9 now (before the next graceful Close) is read as unclean next
	// boot.
	clean, err := store.GetCleanShutdown(context.Background())
	require.NoError(t, err)
	require.False(t, clean,
		"boot must clear the clean-shutdown marker so an in-session crash is detected next boot")
}

// TestRegisterStoreForShare_EntersGraceOnUncleanRestartEmptyLockSet is the core
// area-4 H7 regression test. After a kill -9 / crash the clean-shutdown marker
// is false (or absent — a fresh store defaults to unclean). The server may have
// orphaned client state that never made it into the persisted byte-range locks
// (e.g. a client holding only NFSv4 opens, or a best-effort persist that never
// landed). Grace MUST be entered on the FACT of an unclean restart even though
// the recovered lock set is EMPTY — otherwise a conflicting new lock is granted
// before the prior owner can reclaim.
//
// Before the predicate change (enter grace iff locks>0) this asserted false:
// the empty lock set skipped grace. After the change (unclean OR locks>0) it
// must enter grace with an empty expected-reclaim roster.
func TestRegisterStoreForShare_EntersGraceOnUncleanRestartEmptyLockSet(t *testing.T) {
	const shareName = "/crashed"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	// Fresh store: clean-shutdown marker is absent -> reported false (unclean).
	// No locks persisted -> empty recovered set.
	clean, err := store.GetCleanShutdown(context.Background())
	require.NoError(t, err)
	require.False(t, clean, "precondition: unclean marker (kill -9 / crash / fresh)")

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.True(t, lm.IsInGracePeriod(),
		"an unclean restart must enter grace even with an EMPTY recovered lock set (H7)")
	require.Empty(t, lm.GetExpectedClients(),
		"the expected reclaim roster is empty on an unclean restart with no recovered locks")
}

// TestRegisterStoreForShare_GraceLiftsAfterTimeoutBackstop asserts the hard
// timer backstop: an unclean restart with an empty lock set enters grace, but
// grace must always lift after the configured graceDuration so an always-on
// grace never wedges new-state creation (design §7 regression guard).
func TestRegisterStoreForShare_GraceLiftsAfterTimeoutBackstop(t *testing.T) {
	const shareName = "/crashed-backstop"
	store := memory.NewMemoryMetadataStoreWithDefaults()

	svc := metadata.New()
	// Tiny grace window so the backstop is observable without a slow test.
	svc.SetLockGracePeriod(50 * time.Millisecond)
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.True(t, lm.IsInGracePeriod(),
		"unclean restart must enter grace (empty lock set)")

	require.Eventually(t, func() bool {
		return !lm.IsInGracePeriod()
	}, 2*time.Second, 5*time.Millisecond,
		"grace must lift after the timeout backstop even with no reclaiming clients (no permanent wedge)")
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

// TestGracePeriod_EndNotifiesCoordinatorInstalledAfterRegistration pins the
// production wiring order: shares register at startup BEFORE the NFS adapter
// installs the grace coordinator (during SetRuntime). A lock manager built with
// no coordinator must still notify the coordinator when its grace window ends,
// or the NFSv4 grace machine would never be ended in lockstep. The grace-end
// callback therefore reads the coordinator live rather than capturing it at
// construction.
func TestGracePeriod_EndNotifiesCoordinatorInstalledAfterRegistration(t *testing.T) {
	const shareName = "/coord-late"
	store := memory.NewMemoryMetadataStoreWithDefaults()
	persistNLMLock(t, store, shareName, "client-1")

	// Register the store FIRST — no coordinator installed yet (startup order).
	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.True(t, lm.IsInGracePeriod(), "lock-manager grace must be active")

	// Install the coordinator AFTER the manager was built (adapter SetRuntime).
	coord := &graceSpyCoordinator{
		started: make(chan []string, 1),
		ended:   make(chan struct{}, 1),
	}
	svc.SetGraceCoordinator(coord)

	// End grace by reclaiming the sole expected client (synchronous early exit).
	lm.MarkReclaimed("client-1")

	select {
	case <-coord.ended:
	case <-time.After(time.Second):
		t.Fatal("OnLockGraceEnd was not fired for a coordinator installed after the manager was built")
	}
}
