package metadata

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// shareRegistry holds every piece of per-share state that RegisterStoreForShare
// publishes and RemoveStoreForShare retires: the metadata store and its
// lock-free read mirror, the ephemeral lock manager, the cross-protocol unified
// lock view, the directory-change notifier, the byte quota, and the writeback
// tier flag — plus the grace wiring stamped onto each manager at creation.
//
// One mutex covers all of it, and that is load-bearing rather than incidental.
// Two invariants depend on a single acquisition spanning several of these maps:
//
//  1. Atomic publish. register snapshots removeGen[share], drops the lock to run
//     backend IO (epoch bump, ListLocks, replay, grace entry), then re-acquires
//     and re-checks the generation under the SAME acquisition that publishes
//     stores + storeCache + lockManagers + dirChangeNotifiers. The
//     decision-to-publish and the publish are one step, so a share is never
//     observable store-visible/manager-absent, and a removal landing mid-flight
//     cannot be resurrected.
//
//  2. Grace lockstep. remove must read IsInGracePeriod BEFORE AbortGracePeriod
//     (Abort transitions to Normal and suppresses the balancing callback) while
//     holding the same lock that covers lockManagers and graceCoordinator, so
//     OnLockGraceStart/OnLockGraceEnd stay exactly-once across all four paths.
//
// Splitting these onto separate mutexes puts the generation counter and the
// state it gates behind different locks, and no ordering of two locks restores
// the atomic publish without re-serializing them. Keep them together.
type shareRegistry struct {
	mu                 sync.RWMutex
	stores             map[string]Store                  // shareName -> store
	storeCache         sync.Map                          // shareName -> Store; lock-free mirror of stores, read on every metadata op
	lockManagers       map[string]*LockManager           // shareName -> lock manager (ephemeral, per-share)
	unifiedViews       map[string]*UnifiedLockView       // shareName -> unified lock view (cross-protocol)
	dirChangeNotifiers map[string]lock.DirChangeNotifier // shareName -> notifier for directory changes
	quotas             map[string]int64                  // shareName -> quota in bytes (0 = unlimited)
	writebackShares    map[string]bool                   // shareName -> writeback tier: relax FILE_SYNC metadata flush

	// removeGen counts remove calls per share. register snapshots a share's
	// counter before recovering its lock manager outside mu and re-checks it at
	// publish: any removal of that share during recovery bumps its counter, so
	// the register declines to publish. This closes the register/remove TOCTOU a
	// store-pointer re-check alone cannot: a removal that completes mid-flight
	// and is followed by a same-pointer re-register leaves the entry looking
	// "still ours", which would otherwise resurrect a lock manager + notifier
	// for a removed share.
	removeGen map[string]uint64

	// graceDuration is the lock-manager grace period applied to shares whose
	// stores carry persisted locks at registration. Zero means use the default.
	graceDuration time.Duration

	// graceCoordinator, if set, is invoked when a share's lock-manager grace
	// period starts and ends. It lets the NFS adapter drive the SEPARATE NFSv4
	// StateManager grace machine in lockstep with the lock-manager grace machine
	// so both enter and exit together.
	graceCoordinator GraceCoordinator

	// byteRangeReleaseHook, if set, is stamped onto every per-share lock manager
	// at creation so a byte-range UNLOCK on ANY protocol re-drives blocked
	// waiters on the OTHER protocol. Must be installed before register to affect
	// a given share. The hook receives the handle key (string-encoded FileHandle).
	byteRangeReleaseHook func(handleKey string)
}

func newShareRegistry() shareRegistry {
	return shareRegistry{
		stores:             make(map[string]Store),
		lockManagers:       make(map[string]*LockManager),
		unifiedViews:       make(map[string]*UnifiedLockView),
		dirChangeNotifiers: make(map[string]lock.DirChangeNotifier),
		quotas:             make(map[string]int64),
		writebackShares:    make(map[string]bool),
		removeGen:          make(map[string]uint64),
	}
}

// dirChangeNotifier returns the registered directory-change notifier for a
// share, if any. The dispatch policy lives on Service.notifyDirChange.
func (r *shareRegistry) dirChangeNotifier(shareName string) (lock.DirChangeNotifier, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.dirChangeNotifiers[shareName]
	return n, ok
}

// setGracePeriod sets the grace period applied to per-share lock managers
// that recover persisted locks at registration. A non-positive duration falls
// back to DefaultLockGracePeriod. Must be called before RegisterStoreForShare
// to affect a given share.
func (r *shareRegistry) setGracePeriod(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.graceDuration = d
}

// setGraceCoordinator registers the coordinator that couples lock-manager grace
// with the NFSv4 StateManager grace machine. It may be installed after shares
// register (the NFS adapter does so during SetRuntime): the grace-end callback
// reads the coordinator live, and the adapter catches up the start side for
// shares already in grace, so registration order does not matter.
func (r *shareRegistry) setGraceCoordinator(c GraceCoordinator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.graceCoordinator = c
}

// setByteRangeReleaseHook registers a protocol-agnostic notification that every
// per-share lock manager fires after a byte-range UNLOCK, so a release on one
// protocol re-drives blocked waiters on another (e.g. an SMB UNLOCK waking an
// NLM F_SETLKW waiter). Must be called before RegisterStoreForShare to affect a
// given share. The hook receives the string-encoded FileHandle (handle key).
func (r *shareRegistry) setByteRangeReleaseHook(fn func(handleKey string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byteRangeReleaseHook = fn
}

// setWriteback opts a share into (or out of) the metadata writeback tier
// (#1757). When enabled, FlushPendingWriteForFile downgrades an otherwise
// durable per-op flush (FILE_SYNC WRITE, SMB CLOSE/FLUSH) to the relaxed
// deferred-fsync path, moving the metadata db.Sync off the request hot path.
// Default (not set) is durable. Set at AddShare; cleared by RemoveStoreForShare.
func (r *shareRegistry) setWriteback(shareName string, writeback bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if writeback {
		r.writebackShares[shareName] = true
	} else {
		delete(r.writebackShares, shareName)
	}
}

// writeback reports whether a share is in the writeback tier.
func (r *shareRegistry) writeback(shareName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.writebackShares[shareName]
}

// register associates a metadata store with a share.
// Each share must have exactly one store. Calling this again for the same
// share will replace the previous store.
//
// This also creates a LockManager for the share if one doesn't exist.
// Lock managers are ephemeral and not replaced when re-registering a store.
//
// The LockManager is automatically registered as the DirChangeNotifier for the
// share, enabling unified directory change notifications across protocols.
func (r *shareRegistry) register(shareName string, store Store) error {
	if store == nil {
		return fmt.Errorf("cannot register nil store for share %q", shareName)
	}
	if shareName == "" {
		return fmt.Errorf("cannot register store for empty share name")
	}

	r.mu.Lock()
	// Do NOT publish the store yet (in the new-share path) — it is published
	// atomically with the lock manager below so the share is never observable
	// in a partially-ready state (store visible, lock manager absent).
	_, exists := r.lockManagers[shareName]
	if exists {
		// Share already has a lock manager; replace only the store reference.
		// This is atomic and visible under the lock we already hold. The lock
		// manager is ephemeral and intentionally not replaced.
		r.stores[shareName] = store
		r.storeCache.Store(shareName, store)
		r.mu.Unlock()
		return nil
	}
	// Snapshot this share's removal generation under the same lock. A
	// RemoveStoreForShare for this share that lands while we recover (outside
	// r.mu) bumps it; the publish re-check below aborts when it advanced.
	startGen := r.removeGen[shareName]
	// Snapshot grace config under the same lock (read once; both fields are set
	// before any RegisterStoreForShare call per their doc contract).
	graceDuration := r.graceDuration
	graceCoordinator := r.graceCoordinator
	byteRangeReleaseHook := r.byteRangeReleaseHook
	r.mu.Unlock()
	if graceDuration <= 0 {
		graceDuration = DefaultLockGracePeriod
	}

	// Build and fully recover the lock manager on a local var BEFORE publishing
	// it into r.lockManagers. Recovery (epoch bump + ListLocks + replay) issues
	// backend IO, so it runs outside r.mu — but it must complete before the
	// manager is observable: a concurrent GetLockManagerForShare that saw an
	// empty, unrecovered manager could grant a lock conflicting with a
	// not-yet-restored one. Publishing only after recovery closes that window.
	//
	// Grace is built on this same local manager before publishing: a manager
	// must never be observable in a window where it has restored conflicting
	// locks but not yet entered grace (it would admit a stealing new lock).
	var lm *LockManager
	if ls, ok := store.(lock.LockStore); ok {
		lm = r.newGraceAwareLockManager(graceDuration)
		lm.SetLockStore(ls)
		lm.SetShareName(shareName)
		expectedClients, enterGrace := initLockManagerFromStore(lm, ls, shareName)
		// Enter grace whenever the prior run MAY have left orphaned client state:
		// either the previous shutdown was not verified-clean (kill -9 / crash /
		// power-loss → unclean marker) OR persisted locks were recovered. A
		// genuinely fresh / cleanly-drained store with no locks skips grace and
		// starts in normal operation (the fast path). expectedClients may be
		// empty on an unclean restart with no recovered locks — that is correct:
		// grace still arms its hard timer backstop and lifts after graceDuration,
		// never wedging new-state creation.
		if enterGrace {
			lm.EnterGracePeriod(expectedClients)
			if graceCoordinator != nil {
				graceCoordinator.OnLockGraceStart(expectedClients)
			}
		}
	} else {
		lm = NewLockManager()
	}

	// Stamp the cross-protocol byte-range release notification so an UNLOCK on
	// this share wakes blocked waiters on another protocol (e.g. NLM F_SETLKW
	// blocked on an SMB lock). Set before publishing so no UNLOCK can race past
	// the manager becoming observable without the hook.
	if byteRangeReleaseHook != nil {
		lm.SetByteRangeReleaseCallback(byteRangeReleaseHook)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check under the SAME r.mu acquisition that performs the publish, so the
	// decision-to-publish and the publish itself are atomic. Two distinct races
	// can land between our initial store-publish and this point:
	//
	//  1. Another caller raced us to register this same share. First publisher
	//     wins; drop our manager.
	//  2. A concurrent RemoveStoreForShare deleted this share while we recovered
	//     outside the lock (TOCTOU). If we published our lock manager + notifier
	//     now, we would RESURRECT entries for a removed share — the store map
	//     stays deleted but lockManagers/dirChangeNotifiers come back, leaving
	//     stale routing and a lock manager that is never torn down (leak).
	//
	// We detect a removal-mid-flight via the removal generation snapshotted in
	// the first lock block. RemoveStoreForShare bumps r.removeGen[shareName]; if
	// it advanced while we recovered outside the lock, the share was removed and
	// we must abort the publish rather than resurrect it.
	_, lmExists := r.lockManagers[shareName]
	// The generation delta is the authoritative removed-mid-flight signal. (The
	// store is not yet in r.stores — it is published below alongside the lock
	// manager — so there is no store pointer to compare here.)
	removedMidFlight := !lmExists && r.removeGen[shareName] != startGen
	if lmExists || removedMidFlight {
		// Our manager may have armed a grace timer above. It was never
		// published, so abort that timer without firing onGraceEnd — letting it
		// run would sweep a surviving manager's locks from the shared store and
		// prematurely end the NFSv4 grace machine. We hold r.mu here, and
		// AbortGracePeriod (Close) does not block, so this is deadlock-free.
		//
		// Grace-coordinator balance is asymmetric between the two abort cases:
		//
		//   lmExists (lost a concurrent register for the SAME share): the WINNER
		//   published its manager and, if it entered grace, signalled
		//   OnLockGraceStart. The global NFSv4 grace machine is now coupled to
		//   the WINNER. We must NOT signal OnLockGraceEnd here — doing so would
		//   prematurely end the surviving manager's grace window. Our own
		//   (redundant) start signal was a no-op at the coordinator because v4
		//   grace was already active (first-in-wins policy).
		//
		//   removedMidFlight (a concurrent RemoveStoreForShare deleted this share
		//   while we recovered outside the lock): Remove ran BEFORE we published,
		//   so it never saw our lock manager and never fired OnLockGraceEnd for
		//   the OnLockGraceStart we signalled. If we entered grace, the
		//   coordinator is now wedged in grace for a share that no longer exists;
		//   we must balance it with exactly one OnLockGraceEnd.
		if removedMidFlight && lm.IsInGracePeriod() && graceCoordinator != nil {
			graceCoordinator.OnLockGraceEnd()
		}
		lm.AbortGracePeriod()
		return nil
	}
	// Publish store, lock manager, and dir-change notifier atomically under this
	// single r.mu acquisition so the share is never observable in a
	// partially-ready state (store visible, lock manager absent). A
	// lockManagerForHandle / storeForHandle that arrives during recovery sees
	// neither and consistently reports the share as not-yet-ready.
	r.stores[shareName] = store
	r.storeCache.Store(shareName, store)
	r.lockManagers[shareName] = lm
	// Wire LockManager as DirChangeNotifier: mutations on this share will
	// dispatch directory lease breaks via the lock manager.
	r.dirChangeNotifiers[shareName] = lm

	return nil
}

// remove deregisters a share from the MetadataService, deleting
// its entry from every per-share map populated by RegisterStoreForShare and the
// AddShare path (stores, lockManagers, unifiedViews, dirChangeNotifiers,
// quotas). Without this, those maps grow unbounded across AddShare/RemoveShare
// churn and leave stale routing: a removed-share handle would still resolve to a
// live store, and re-adding a same-name share would silently reuse the stale
// lock manager (RegisterStoreForShare early-returns when one already exists).
//
// Before dropping the lock manager its grace timer is aborted so the orphaned
// timer never fires onGraceEnd against a now-removed share. Idempotent: removing
// a share that was never registered (or already removed) is a no-op.
//
// This is the symmetric counterpart of RegisterStoreForShare and must be called
// from the control-plane RemoveShare path after the share's stores are torn
// down.
//
// Thread safety: Safe to call concurrently.
func (r *shareRegistry) remove(shareName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if lm, ok := r.lockManagers[shareName]; ok && lm != nil {
		// If the manager is still in grace, it had signalled OnLockGraceStart to
		// the grace coordinator at registration. AbortGracePeriod (below) stops
		// the timer WITHOUT firing onGraceEnd, which is exactly what suppresses
		// the coordinator's balancing OnLockGraceEnd. Left unbalanced, the
		// coordinator (NFSv4 StateManager) would stay wedged in grace
		// indefinitely after this share is removed. Fire the balancing end here,
		// mirroring how the normal timer/early-exit path ends grace.
		//
		// Capturing IsInGracePeriod before AbortGracePeriod is required: Abort
		// transitions the state to Normal, after which IsInGracePeriod would read
		// false. The coordinator is read under the r.mu we already hold, and
		// OnLockGraceEnd must not block (interface contract), so this is
		// deadlock-free. Exactly-once: if grace had already lifted naturally the
		// onGraceEnd closure already fired OnLockGraceEnd and IsInGracePeriod is
		// false here, so we do not double-fire.
		if lm.IsInGracePeriod() && r.graceCoordinator != nil {
			r.graceCoordinator.OnLockGraceEnd()
		}
		// Abort the grace timer (if armed) so it never fires onGraceEnd against
		// a removed share. AbortGracePeriod stops the timer synchronously and
		// does not block, so holding r.mu across it is safe.
		lm.AbortGracePeriod()
	}

	delete(r.stores, shareName)
	r.storeCache.Delete(shareName)
	delete(r.lockManagers, shareName)
	delete(r.unifiedViews, shareName)
	delete(r.dirChangeNotifiers, shareName)
	delete(r.quotas, shareName)
	delete(r.writebackShares, shareName)

	// Bump this share's removal generation so any RegisterStoreForShare recovering
	// it outside r.mu declines to publish: the register snapshots removeGen before
	// recovery and re-checks it at publish (register/remove TOCTOU guard).
	r.removeGen[shareName]++
}

// initLockManagerFromStore stamps a fresh server epoch and replays any locks
// persisted by a previous run back into the lock manager. Errors are logged
// and swallowed so a recovery failure never blocks share registration.
//
// Epoch double-bump on a lost-publish race (R3-5): RegisterStoreForShare runs
// this on a local manager before publishing under r.mu, and the loser of a
// concurrent registration drops its manager. The loser still incremented the
// store epoch here, so two concurrent registrations of the same share advance
// the persisted epoch by 2 instead of 1. This is harmless: the epoch is only a
// monotonic split-brain/stale-lock marker, the surviving manager uses whatever
// epoch it observed, and every lock it restores predates that epoch regardless
// of the gap. Moving IncrementServerEpoch under r.mu would serialize backend IO
// inside the service lock for no correctness gain, so the increment stays here.
//
// It returns the unique set of client IDs recovered from the persisted locks
// (the grace period's expected-reclaim roster) and a boolean reporting whether
// grace should be entered for this share.
//
// Grace-entry decision (area-4 H7): grace is entered when the previous run MAY
// have orphaned client state — i.e. the prior shutdown was NOT verified-clean
// (unclean marker: kill -9 / crash / power-loss, or a fresh store whose marker
// defaults to false) OR persisted locks were recovered. This replaces the old
// "enter grace only if persisted locks exist" predicate, which silently skipped
// grace after a crash that left no recoverable byte-range lock (e.g. a client
// holding only NFSv4 opens, or a best-effort persist that never landed),
// letting a conflicting new lock be granted before the prior owner reclaimed.
//
// The clean-shutdown marker is read first to make the decision, then
// immediately set FALSE for the running session: if this process is killed
// without a graceful Close() (which is the only writer of true), the NEXT boot
// reads false and conservatively enters grace. The flag is set false as early
// as possible — before any traffic can be served — so the crash window in which
// a kill would be misread as clean is effectively zero.
func initLockManagerFromStore(lm *LockManager, ls lock.LockStore, shareName string) (clients []string, enterGrace bool) {
	ctx := context.Background()

	// Read the clean-shutdown marker, then immediately clear it for this run.
	// A read error is treated as unclean (fail-safe): we would rather impose a
	// grace window than risk granting a stealing lock.
	clean, err := ls.GetCleanShutdown(ctx)
	if err != nil {
		logger.Error("lock recovery: failed to read clean-shutdown marker (treating as unclean)",
			"share", shareName, "error", err)
		clean = false
	}
	unclean := !clean
	if err := ls.SetCleanShutdown(ctx, false); err != nil {
		// Could not arm the unclean marker for this session. Logged but not
		// fatal: durability of the marker is best-effort, mirroring the lock
		// persistence contract.
		logger.Error("lock recovery: failed to clear clean-shutdown marker",
			"share", shareName, "error", err)
	}

	epoch, err := ls.IncrementServerEpoch(ctx)
	if err != nil {
		logger.Error("lock recovery: failed to increment server epoch", "share", shareName, "error", err)
	} else {
		lm.SetEpoch(epoch)
	}

	persisted, err := ls.ListLocks(ctx, lock.LockQuery{ShareName: shareName})
	if err != nil {
		logger.Error("lock recovery: failed to list persisted locks", "share", shareName, "error", err)
		// We could not enumerate locks; if the prior shutdown was unclean still
		// enter grace (empty roster, timer backstop) rather than risk a steal.
		return nil, unclean
	}
	if len(persisted) > 0 {
		if err := lm.RestoreLocks(persisted); err != nil {
			logger.Error("lock recovery: failed to restore persisted locks", "share", shareName, "error", err)
			return nil, unclean
		}
	}

	// Collect the unique client IDs that held locks before the restart; these
	// are the clients the grace period waits on for reclaim.
	seen := make(map[string]struct{}, len(persisted))
	for _, pl := range persisted {
		if pl.ClientID == "" {
			continue
		}
		if _, dup := seen[pl.ClientID]; dup {
			continue
		}
		seen[pl.ClientID] = struct{}{}
		clients = append(clients, pl.ClientID)
	}

	enterGrace = unclean || len(persisted) > 0
	logger.Info("lock recovery: completed",
		"share", shareName, "restored_locks", len(persisted), "epoch", epoch,
		"clients", len(clients), "prior_shutdown_clean", clean, "enter_grace", enterGrace)
	return clients, enterGrace
}

// newGraceAwareLockManager builds a lock manager whose grace period sweeps any
// locks left unreclaimed when the grace window ends and notifies the grace
// coordinator so the NFSv4 StateManager grace machine exits in lockstep.
//
// The onGraceEnd callback is best-effort: a client that did not reclaim within
// the window has its stale persisted+in-memory locks dropped (RemoveClientLocks),
// matching the X/Open NLMv4 contract that unreclaimed state is forfeited once
// grace ends.
//
// The coordinator is read LIVE from the service when the window ends, not
// captured at construction: the NFS adapter installs it (SetGraceCoordinator)
// during SetRuntime, which runs AFTER shares register at startup. A manager
// built before the adapter exists must still notify the coordinator once it is
// installed, or the v4 grace machine would never be ended in lockstep.
func (r *shareRegistry) newGraceAwareLockManager(duration time.Duration) *LockManager {
	// lm and gpm are captured by the onGraceEnd closure below. The closure only
	// runs after EnterGracePeriod arms the timer, by which point both are set.
	var lm *LockManager

	gpm := lock.NewGracePeriodManager(duration, func() {
		if lm != nil {
			reclaimed := make(map[string]struct{})
			for _, c := range lm.GetReclaimedClients() {
				reclaimed[c] = struct{}{}
			}
			for _, c := range lm.GetExpectedClients() {
				if _, ok := reclaimed[c]; ok {
					continue
				}
				logger.Info("grace period: sweeping unreclaimed locks", "client", c)
				lm.RemoveClientLocks(c)
			}
		}
		r.mu.RLock()
		coordinator := r.graceCoordinator
		r.mu.RUnlock()
		if coordinator != nil {
			coordinator.OnLockGraceEnd()
		}
	})

	lm = lock.NewManagerWithGracePeriod(gpm)
	return lm
}

// storeForShare returns the metadata store for a specific share. Every
// metadata op resolves through here, so the common case is served from the
// lock-free storeCache without touching r.mu. The cache is written under r.mu
// alongside r.stores, so a miss after RemoveStoreForShare falls through to the
// locked map and yields the stale-handle error below — never a stale store.
func (r *shareRegistry) storeForShare(shareName string) (Store, error) {
	if v, ok := r.storeCache.Load(shareName); ok {
		if store, ok := v.(Store); ok {
			return store, nil
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if store, ok := r.stores[shareName]; ok {
		return store, nil
	}

	// The handle decoded but names a share that no longer exists (e.g. held
	// across a RemoveShare). Return a stale-handle StoreError so protocol
	// mappers translate to NFS *STALE / SMB STATUS_FILE_CLOSED instead of a
	// generic server fault.
	return nil, NewStaleHandleError(shareName)
}

// lockManagerForShare returns the lock manager for an already-decoded share
// name. Splitting this out of lockManagerForHandle lets callers that have
// already decoded the handle (see storeAndLockManagerForHandle) avoid a second
// UUID parse.
func (r *shareRegistry) lockManagerForShare(shareName string) (*LockManager, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if lm, ok := r.lockManagers[shareName]; ok {
		return lm, nil
	}

	// Decoded handle names a share with no lock manager (removed share):
	// stale-handle StoreError so callers map to *STALE.
	return nil, NewStaleHandleError(shareName)
}

// lockManagerOrNil returns the lock manager for a specific share.
//
// This is used by the NFS adapter to process NLM blocking lock waiters.
// Returns nil if no lock manager exists for the share.
//
// Thread safety: Safe to call concurrently.
func (r *shareRegistry) lockManagerOrNil(shareName string) *LockManager {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if lm, ok := r.lockManagers[shareName]; ok {
		return lm
	}
	return nil
}

// unifiedView returns the UnifiedLockView for a specific share.
//
// UnifiedLockView provides cross-protocol lock visibility, allowing any protocol
// handler to query all locks (NLM byte-range and SMB leases) on a file.
//
// Returns nil if no UnifiedLockView exists for the share. This can happen if:
//   - The share has not been registered
//   - No LockStore has been set for the share
//
// Thread safety: Safe to call concurrently.
func (r *shareRegistry) unifiedView(shareName string) *UnifiedLockView {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if view, ok := r.unifiedViews[shareName]; ok {
		return view
	}
	return nil
}

// setUnifiedView sets the UnifiedLockView for a specific share.
//
// This is called when a LockStore becomes available for a share (e.g., when
// a store that implements LockStore is registered). Protocol handlers should
// NOT call this directly - it's for internal use by the registration process.
//
// Thread safety: Safe to call concurrently.
func (r *shareRegistry) setUnifiedView(shareName string, view *UnifiedLockView) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.unifiedViews[shareName] = view
}

// setQuota sets the byte quota for a share. 0 means unlimited.
func (r *shareRegistry) setQuota(shareName string, quotaBytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quotas[shareName] = quotaBytes
}

// quota returns the byte quota for a share. 0 means unlimited.
func (r *shareRegistry) quota(shareName string) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.quotas[shareName]
}

// setDirChangeNotifier registers a DirChangeNotifier for a share.
//
// When directory mutations occur on this share (create, remove, rename),
// the notifier will be called to dispatch directory lease breaks.
// Typically the LockManager is used as the notifier since it implements
// lock.DirChangeNotifier.
//
// Thread safety: Safe to call concurrently.
func (r *shareRegistry) setDirChangeNotifier(shareName string, n lock.DirChangeNotifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirChangeNotifiers[shareName] = n
}
