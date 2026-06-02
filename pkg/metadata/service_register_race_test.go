package metadata_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// countingCoordinator wraps the package's graceSpyCoordinator with atomic
// start/end counters so the exactly-once grace-balance invariant can be asserted
// across the registration/removal lifecycle. It reuses graceSpyCoordinator's
// callback seams rather than reimplementing the GraceCoordinator interface.
func countingCoordinator() (*graceSpyCoordinator, *int32, *int32) {
	var starts, ends int32
	coord := &graceSpyCoordinator{
		started: make(chan []string, 1),
		ended:   make(chan struct{}, 1),
		onStart: func(_ []string) { atomic.AddInt32(&starts, 1) },
		onEnd:   func() { atomic.AddInt32(&ends, 1) },
	}
	return coord, &starts, &ends
}

// TestRemoveStoreForShare_BalancesGraceCoordinator is the Finding-1 regression.
// A share that enters grace at registration signals OnLockGraceStart to the
// coordinator (coupling the NFSv4 StateManager grace machine in lockstep).
// RemoveStoreForShare aborts the grace timer WITHOUT firing the timer's
// onGraceEnd closure, so without an explicit balancing call the coordinator
// would stay wedged in grace forever after the share is gone.
//
// Asserts: start fired once at registration; after RemoveStoreForShare the
// coordinator saw exactly one OnLockGraceEnd; and the start/end counts are
// symmetric (the coordinator is fully balanced, not left in grace).
func TestRemoveStoreForShare_BalancesGraceCoordinator(t *testing.T) {
	const shareName = "/graced-remove"
	store := memory.NewMemoryMetadataStoreWithDefaults()
	persistNLMLock(t, store, shareName, "client-1")

	coord, starts, ends := countingCoordinator()

	svc := metadata.New()
	svc.SetGraceCoordinator(coord)
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.True(t, lm.IsInGracePeriod(), "share with persisted locks must enter grace")
	require.Equal(t, int32(1), atomic.LoadInt32(starts),
		"OnLockGraceStart must fire exactly once at registration")
	require.Equal(t, int32(0), atomic.LoadInt32(ends),
		"OnLockGraceEnd must not fire while the share is still in grace")

	// Remove the share while it is still in grace.
	svc.RemoveStoreForShare(shareName)

	require.Equal(t, int32(1), atomic.LoadInt32(ends),
		"RemoveStoreForShare during grace must fire OnLockGraceEnd exactly once "+
			"(coordinator must not be left wedged in grace)")
	require.Equal(t, atomic.LoadInt32(starts), atomic.LoadInt32(ends),
		"grace start/end must be balanced after removal (symmetric invariant)")

	// The share is fully gone: no lock manager, no resurrected routing.
	require.Nil(t, svc.GetLockManagerForShare(shareName),
		"lock manager must be removed")
}

// TestRemoveStoreForShare_NoDoubleEndWhenGraceLiftedNaturally guards the
// exactly-once contract from the other side: if grace already lifted naturally
// (all expected clients reclaimed) before removal, the onGraceEnd closure
// already fired OnLockGraceEnd. RemoveStoreForShare must NOT fire a second time —
// IsInGracePeriod is read before AbortGracePeriod and is false here.
func TestRemoveStoreForShare_NoDoubleEndWhenGraceLiftedNaturally(t *testing.T) {
	const shareName = "/graced-natural"
	store := memory.NewMemoryMetadataStoreWithDefaults()
	persistNLMLock(t, store, shareName, "client-1")

	coord, starts, ends := countingCoordinator()

	svc := metadata.New()
	svc.SetGraceCoordinator(coord)
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.True(t, lm.IsInGracePeriod())

	// Reclaim the sole expected client: grace lifts naturally and the onGraceEnd
	// closure fires OnLockGraceEnd synchronously.
	lm.MarkReclaimed("client-1")
	require.False(t, lm.IsInGracePeriod(), "grace must lift after the sole client reclaims")
	require.Equal(t, int32(1), atomic.LoadInt32(ends),
		"natural grace lift fires OnLockGraceEnd once")

	// Removing the (no-longer-in-grace) share must NOT fire a second end.
	svc.RemoveStoreForShare(shareName)
	require.Equal(t, int32(1), atomic.LoadInt32(ends),
		"RemoveStoreForShare must not double-fire OnLockGraceEnd after a natural lift")
	require.Equal(t, atomic.LoadInt32(starts), atomic.LoadInt32(ends),
		"grace start/end balanced (exactly-once across natural lift + remove)")
}

// removeMidFlightStore is a test-only MetadataStore+LockStore decorator that
// fires a RemoveStoreForShare DURING the unlocked recovery phase of
// RegisterStoreForShare. Recovery calls ListLocks (initLockManagerFromStore,
// outside s.mu); intercepting that call lets us land a concurrent removal
// deterministically in the exact TOCTOU window between the initial
// store-publish and the final publish re-check — no goroutines or sleeps.
//
// It embeds *memory.MemoryMetadataStore so it satisfies both MetadataStore and
// lock.LockStore (the recovery path type-asserts the store to lock.LockStore),
// overriding only ListLocks. After firing the removal once it returns the real
// persisted locks so the manager still enters grace (signalling
// OnLockGraceStart) before the re-check observes the removal and must balance it.
type removeMidFlightStore struct {
	*memory.MemoryMetadataStore
	svc       *metadata.MetadataService
	shareName string
	fired     bool
}

func (s *removeMidFlightStore) ListLocks(ctx context.Context, q lock.LockQuery) ([]*lock.PersistedLock, error) {
	if !s.fired {
		s.fired = true
		// Simulate a concurrent RemoveStoreForShare landing while we recover
		// outside s.mu. This deletes s.stores[shareName] before our publish.
		s.svc.RemoveStoreForShare(s.shareName)
	}
	return s.MemoryMetadataStore.ListLocks(ctx, q)
}

// TestRegisterStoreForShare_RemovedMidFlightDoesNotResurrect is the Finding-2
// regression: a RemoveStoreForShare that lands between the initial store-publish
// and the final publish re-check must NOT resurrect the share. Before the fix,
// RegisterStoreForShare would unconditionally publish its lock manager +
// dirChangeNotifier under the final s.mu, re-adding routing for a removed share
// (stale routing + a lock manager that is never torn down = leak), and the
// OnLockGraceStart it signalled would be left unbalanced (coordinator wedged in
// grace for a share that no longer exists).
//
// The decorator fires the removal during recovery (ListLocks), then returns a
// persisted lock so the manager enters grace and signals OnLockGraceStart. The
// re-check must then detect removedMidFlight, fire exactly one balancing
// OnLockGraceEnd, abort the (unpublished) manager's grace timer, and decline to
// publish.
func TestRegisterStoreForShare_RemovedMidFlightDoesNotResurrect(t *testing.T) {
	const shareName = "/raced-remove"
	base := memory.NewMemoryMetadataStoreWithDefaults()
	persistNLMLock(t, base, shareName, "client-1")

	coord, starts, ends := countingCoordinator()

	svc := metadata.New()
	svc.SetGraceCoordinator(coord)

	store := &removeMidFlightStore{
		MemoryMetadataStore: base,
		svc:                 svc,
		shareName:           shareName,
	}

	// Registration must succeed (nil) — the removal is absorbed silently.
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))
	require.True(t, store.fired, "the mid-flight removal must have been exercised")

	// The share must NOT be resurrected: no lock manager re-published.
	require.Nil(t, svc.GetLockManagerForShare(shareName),
		"a share removed mid-flight must NOT have its lock manager resurrected")

	// The manager DID enter grace during recovery, so OnLockGraceStart fired once.
	require.Equal(t, int32(1), atomic.LoadInt32(starts),
		"recovery entered grace and signalled OnLockGraceStart once")
	// removedMidFlight must balance that start with exactly one OnLockGraceEnd —
	// the prior RemoveStoreForShare ran before publish and never saw our manager,
	// so the register re-check owns the balancing end.
	require.Equal(t, int32(1), atomic.LoadInt32(ends),
		"removed-mid-flight register must fire exactly one balancing OnLockGraceEnd")
	require.Equal(t, atomic.LoadInt32(starts), atomic.LoadInt32(ends),
		"coordinator must not be left wedged in grace for a removed share")
}

// TestRegisterStoreForShare_LostRaceDoesNotEndWinnerGrace pins the asymmetric
// branch: when a register loses a concurrent register for the SAME share
// (lmExists), the WINNER owns the coordinator's grace. The loser must abort its
// own unpublished timer WITHOUT firing OnLockGraceEnd — firing would prematurely
// end the surviving winner's grace window.
//
// We synthesize the lost-race deterministically: register a first store (the
// winner) so a lock manager is published and grace begins, then register a
// SECOND, different store for the same share. The second call sees lmExists on
// the re-check and must drop its manager without touching the coordinator's end.
func TestRegisterStoreForShare_LostRaceDoesNotEndWinnerGrace(t *testing.T) {
	const shareName = "/raced-same"
	winner := memory.NewMemoryMetadataStoreWithDefaults()
	persistNLMLock(t, winner, shareName, "client-1")

	coord, starts, ends := countingCoordinator()

	svc := metadata.New()
	svc.SetGraceCoordinator(coord)

	// Winner registers first: manager published, grace begins, start fires once.
	require.NoError(t, svc.RegisterStoreForShare(shareName, winner))
	lm := svc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	require.True(t, lm.IsInGracePeriod())
	require.Equal(t, int32(1), atomic.LoadInt32(starts))
	require.Equal(t, int32(0), atomic.LoadInt32(ends))

	// A second register for the SAME share with a DIFFERENT store loses the race:
	// lmExists is true on the re-check, so it drops its manager and must NOT fire
	// OnLockGraceEnd (the winner still owns grace).
	loser := memory.NewMemoryMetadataStoreWithDefaults()
	persistNLMLock(t, loser, shareName, "client-2")
	require.NoError(t, svc.RegisterStoreForShare(shareName, loser))

	require.Equal(t, int32(0), atomic.LoadInt32(ends),
		"losing a same-share register must NOT end the winner's grace")
	require.True(t, lm.IsInGracePeriod(),
		"the winner's grace window must survive a lost concurrent register")
	require.Same(t, lm, svc.GetLockManagerForShare(shareName),
		"the published lock manager must remain the winner's (no replacement)")
}

// TestRemoveStoreForShare_ConcurrentRegisterDoesNotResurrect is the concurrent
// -race reproducer the area-7 REVIEW calls for. It drives the add/remove churn
// the static finding describes: many concurrent RemoveStoreForShare vs
// RegisterStoreForShare for the same share, with GetLockManagerForShare reading
// throughout. Run under -race it surfaces any unsynchronised access to the
// per-share maps across the register/remove TOCTOU window.
func TestRemoveStoreForShare_ConcurrentRegisterDoesNotResurrect(t *testing.T) {
	const (
		shareName = "/churn"
		rounds    = 200
	)

	svc := metadata.New()
	store := memory.NewMemoryMetadataStoreWithDefaults()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds*4; i++ {
			_ = svc.GetLockManagerForShare(shareName)
			_, _ = svc.GetStoreForShare(shareName)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = svc.RegisterStoreForShare(shareName, store)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			svc.RemoveStoreForShare(shareName)
		}
	}()

	wg.Wait()
}
