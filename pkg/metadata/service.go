package metadata

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// DefaultLockGracePeriod is the fallback lock-manager grace period applied when
// no duration is configured. Mirrors the conventional NLM/NFSv4 grace window.
const DefaultLockGracePeriod = 90 * time.Second

// Service provides all metadata operations for the filesystem.
//
// It manages metadata stores and routes operations to the correct store
// based on share name. All protocol handlers should interact with Service
// rather than accessing stores directly.
//
// File Locking:
// Service owns one LockManager per share for byte-range locking (SMB/NLM).
// Locks are ephemeral (in-memory only) and lost on server restart.
// This is separate from metadata stores which handle persistent data.
//
// Usage:
//
//	metaSvc := metadata.New()
//	metaSvc.RegisterStoreForShare("/export", memoryStore)
//	metaSvc.RegisterStoreForShare("/archive", badgerStore)
//
//	// High-level operations (with business logic)
//	file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
//
//	// Low-level operations (direct store access)
//	file, err := metaSvc.GetFile(ctx, handle)
type Service struct {
	mu                 sync.RWMutex
	stores             map[string]Store                  // shareName -> store
	lockManagers       map[string]*LockManager           // shareName -> lock manager (ephemeral, per-share)
	unifiedViews       map[string]*UnifiedLockView       // shareName -> unified lock view (cross-protocol)
	dirChangeNotifiers map[string]lock.DirChangeNotifier // shareName -> notifier for directory changes
	pendingWrites      *PendingWritesTracker             // deferred metadata commits for performance
	deferredCommit     bool                              // if true, use deferred commits (default: true)
	cookies            *CookieManager                    // NFS/SMB cookie to store token translation
	quotas             map[string]int64                  // shareName -> quota in bytes (0 = unlimited)

	// identityQuotas holds hot-updatable per-user / per-group quota limits,
	// loaded from the control-plane DB and consulted on the write/create hot
	// path. Has its own mutex (does not contend with s.mu).
	identityQuotas *quotaLimits

	// quotaGracePersist, if set, is invoked when the enforcer starts or clears a
	// grace timer so the control-plane row's grace_started_at can be persisted.
	// A zero time clears the timer. Registered via SetQuotaGracePersister.
	quotaGracePersist QuotaGracePersister

	// removeGen counts RemoveStoreForShare calls per share. RegisterStoreForShare
	// snapshots a share's counter before recovering its lock manager outside s.mu
	// and re-checks it at publish: any removal of that share during recovery bumps
	// its counter, so the register declines to publish. This closes the
	// register/remove TOCTOU the store-pointer re-check alone cannot: a removal
	// that completes mid-flight and is followed by a same-pointer re-register
	// leaves the entry looking "still ours", which would otherwise resurrect a
	// lock manager + notifier for a removed share.
	removeGen map[string]uint64

	// graceDuration is the lock-manager grace period applied to shares whose
	// stores carry persisted locks at registration. Zero means use the default.
	graceDuration time.Duration

	// graceCoordinator, if set, is invoked when a share's lock-manager grace
	// period starts and ends. It lets the NFS adapter drive the SEPARATE NFSv4
	// StateManager grace machine in lockstep with the lock-manager grace machine
	// so both enter and exit together. Registered via SetGraceCoordinator.
	graceCoordinator GraceCoordinator

	// byteRangeReleaseHook, if set, is stamped onto every per-share lock manager
	// at creation so a byte-range UNLOCK on ANY protocol re-drives blocked
	// waiters on the OTHER protocol. The NFS adapter wires this to its
	// processNLMWaiters drain: an NLM F_SETLKW waiter blocked on an SMB lock is
	// woken when the SMB holder unlocks (NLM uses a server-driven GRANTED
	// callback, not poll-retry). Registered via SetByteRangeReleaseHook before
	// RegisterStoreForShare to affect a given share. The hook receives the
	// handle key (string-encoded FileHandle).
	byteRangeReleaseHook func(handleKey string)

	// trashPolicy, if set, supplies the per-share recycle-bin policy consulted
	// on delete. Nil (the default) disables trash entirely: deletes destroy
	// content as before. Installed via SetTrashPolicy.
	trashPolicy TrashPolicy

	// xattrStreamReader, if set, reads the content of a named-stream child File
	// so the xattr resolver can surface stream-backed xattr values (the
	// stream-entity backing). It is wired by the runtime layer, which has
	// block-store access (GetBlockStoreForHandle + engine.Store.ReadAt); the
	// metadata Service stays block-engine-agnostic. Nil (the default) leaves
	// stream NAMES enumerable via ListXattr but makes GetXattr report a
	// stream-only name as absent. Installed via SetXattrStreamReader.
	xattrStreamReader StreamContentReader
}

// GraceCoordinator couples the lock-manager grace period with another grace
// machine (the NFSv4 StateManager). When a share recovers persisted locks at
// registration the lock manager enters grace and OnLockGraceStart fires; when
// that grace window ends (timer, early-exit, or sweep) OnLockGraceEnd fires.
// Implementations must be safe for concurrent use and must not block.
type GraceCoordinator interface {
	// OnLockGraceStart is called when a share's lock-manager grace period begins.
	// expectedClients are the client IDs recovered from persisted locks.
	OnLockGraceStart(expectedClients []string)

	// OnLockGraceEnd is called when a share's lock-manager grace period ends.
	OnLockGraceEnd()
}

// New creates a new empty MetadataService instance.
// Use RegisterStoreForShare to configure stores for each share.
// By default, deferred commits are enabled for better write performance.
func New() *Service {
	return &Service{
		stores:             make(map[string]Store),
		lockManagers:       make(map[string]*LockManager),
		unifiedViews:       make(map[string]*UnifiedLockView),
		dirChangeNotifiers: make(map[string]lock.DirChangeNotifier),
		pendingWrites:      NewPendingWritesTracker(),
		deferredCommit:     true, // Enable deferred commits by default
		cookies:            NewCookieManager(),
		quotas:             make(map[string]int64),
		identityQuotas:     newQuotaLimits(),
		removeGen:          make(map[string]uint64),
	}
}

// QuotaGracePersister persists a per-identity quota's grace timer transition
// back to the control-plane store. t is the new grace_started_at (zero clears
// it). Implementations must be safe for concurrent use and should not block the
// caller for long (the write/create hot path invokes this when grace state
// changes).
type QuotaGracePersister interface {
	PersistQuotaGrace(shareName string, scope QuotaScope, id uint32, t time.Time)

	// PersistDefaultUserGrace durably records (zero t reaps) the per-real-user
	// grace timer for a default-user quota fallback. uid is the REAL uid (not the
	// DefaultUserID sentinel). Unlike an explicit quota — whose grace lives on its
	// own row — a default-user quota is a single shared template row that cannot
	// hold per-user grace, so the timer is stored in a side table keyed by
	// (share, uid). Persisting it makes default-user soft→grace→hard enforcement
	// survive a restart. Same best-effort, non-blocking contract as
	// PersistQuotaGrace.
	PersistDefaultUserGrace(shareName string, uid uint32, t time.Time)
}

// SetQuotaGracePersister installs the grace-timer persistence hook.
func (s *Service) SetQuotaGracePersister(p QuotaGracePersister) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaGracePersist = p
}

// SetIdentityQuota installs or replaces a single per-identity quota for a share.
func (s *Service) SetIdentityQuota(shareName string, iq IdentityQuota) {
	s.identityQuotas.set(shareName, iq)
}

// RemoveIdentityQuota deletes a single per-identity quota for a share.
func (s *Service) RemoveIdentityQuota(shareName string, scope QuotaScope, id uint32) {
	s.identityQuotas.remove(shareName, scope, id)
}

// ReplaceIdentityQuotas atomically replaces all per-identity quotas for a share.
func (s *Service) ReplaceIdentityQuotas(shareName string, quotas []IdentityQuota) {
	s.identityQuotas.replaceShare(shareName, quotas)
}

// SeedDefaultUserGrace restores the durable per-real-user default-user grace
// timers for a share (keyed by real uid), replacing any existing ephemeral
// entries. Called at share load so default-user soft→grace→hard enforcement
// survives a restart.
func (s *Service) SeedDefaultUserGrace(shareName string, byUID map[uint32]time.Time) {
	s.identityQuotas.seedDynGrace(shareName, byUID)
}

// ClearDefaultUserGrace drops the in-memory per-real-user default-user grace
// timer for a single uid on a share. Called when an explicit user quota is
// removed and the uid reverts to the default-user fallback, so it does not
// inherit a stale (possibly already-expired) grace window left over from before
// the explicit quota was installed. The caller reaps the durable side-table row
// separately.
func (s *Service) ClearDefaultUserGrace(shareName string, uid uint32) {
	s.identityQuotas.setDynGrace(shareName, QuotaScopeUser, uid, time.Time{})
}

// GetIdentityQuota returns the exact-keyed quota for (scope,id) on a share.
func (s *Service) GetIdentityQuota(shareName string, scope QuotaScope, id uint32) (IdentityQuota, bool) {
	return s.identityQuotas.get(shareName, scope, id)
}

// ListIdentityQuotas returns a snapshot of every configured per-identity quota
// across all shares. Intended for observability (metrics); the result is
// bounded by the number of explicitly-configured quota principals.
func (s *Service) ListIdentityQuotas() []ConfiguredQuota {
	return s.identityQuotas.snapshot()
}

// SetDeferredCommit enables or disables deferred metadata commits.
// When enabled, CommitWrite batches updates until FlushPendingWrites is called.
// This significantly improves write performance for sequential workloads.
func (s *Service) SetDeferredCommit(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferredCommit = enabled
}

// SetLockGracePeriod sets the grace period applied to per-share lock managers
// that recover persisted locks at registration. A non-positive duration falls
// back to DefaultLockGracePeriod. Must be called before RegisterStoreForShare
// to affect a given share.
func (s *Service) SetLockGracePeriod(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graceDuration = d
}

// SetGraceCoordinator registers the coordinator that couples lock-manager grace
// with the NFSv4 StateManager grace machine. It may be installed after shares
// register (the NFS adapter does so during SetRuntime): the grace-end callback
// reads the coordinator live, and the adapter catches up the start side for
// shares already in grace, so registration order does not matter.
func (s *Service) SetGraceCoordinator(c GraceCoordinator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graceCoordinator = c
}

// SetByteRangeReleaseHook registers a protocol-agnostic notification that every
// per-share lock manager fires after a byte-range UNLOCK, so a release on one
// protocol re-drives blocked waiters on another (e.g. an SMB UNLOCK waking an
// NLM F_SETLKW waiter). Must be called before RegisterStoreForShare to affect a
// given share. The hook receives the string-encoded FileHandle (handle key).
func (s *Service) SetByteRangeReleaseHook(fn func(handleKey string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byteRangeReleaseHook = fn
}

// SetTrashPolicy installs the per-share recycle-bin policy. A nil policy
// (the default) disables trash: deletes destroy content as before.
func (s *Service) SetTrashPolicy(p TrashPolicy) { s.trashPolicy = p }

// RegisterStoreForShare associates a metadata store with a share.
// Each share must have exactly one store. Calling this again for the same
// share will replace the previous store.
//
// This also creates a LockManager for the share if one doesn't exist.
// Lock managers are ephemeral and not replaced when re-registering a store.
//
// The LockManager is automatically registered as the DirChangeNotifier for the
// share, enabling unified directory change notifications across protocols.
func (s *Service) RegisterStoreForShare(shareName string, store Store) error {
	if store == nil {
		return fmt.Errorf("cannot register nil store for share %q", shareName)
	}
	if shareName == "" {
		return fmt.Errorf("cannot register store for empty share name")
	}

	s.mu.Lock()
	// Do NOT publish the store yet (in the new-share path) — it is published
	// atomically with the lock manager below so the share is never observable
	// in a partially-ready state (store visible, lock manager absent).
	_, exists := s.lockManagers[shareName]
	if exists {
		// Share already has a lock manager; replace only the store reference.
		// This is atomic and visible under the lock we already hold. The lock
		// manager is ephemeral and intentionally not replaced.
		s.stores[shareName] = store
		s.mu.Unlock()
		return nil
	}
	// Snapshot this share's removal generation under the same lock. A
	// RemoveStoreForShare for this share that lands while we recover (outside
	// s.mu) bumps it; the publish re-check below aborts when it advanced.
	startGen := s.removeGen[shareName]
	// Snapshot grace config under the same lock (read once; both fields are set
	// before any RegisterStoreForShare call per their doc contract).
	graceDuration := s.graceDuration
	graceCoordinator := s.graceCoordinator
	byteRangeReleaseHook := s.byteRangeReleaseHook
	s.mu.Unlock()
	if graceDuration <= 0 {
		graceDuration = DefaultLockGracePeriod
	}

	// Build and fully recover the lock manager on a local var BEFORE publishing
	// it into s.lockManagers. Recovery (epoch bump + ListLocks + replay) issues
	// backend IO, so it runs outside s.mu — but it must complete before the
	// manager is observable: a concurrent GetLockManagerForShare that saw an
	// empty, unrecovered manager could grant a lock conflicting with a
	// not-yet-restored one. Publishing only after recovery closes that window.
	//
	// Grace is built on this same local manager before publishing: a manager
	// must never be observable in a window where it has restored conflicting
	// locks but not yet entered grace (it would admit a stealing new lock).
	var lm *LockManager
	if ls, ok := store.(lock.LockStore); ok {
		lm = s.newGraceAwareLockManager(graceDuration)
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

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the SAME s.mu acquisition that performs the publish, so the
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
	// the first lock block. RemoveStoreForShare bumps s.removeGen[shareName]; if
	// it advanced while we recovered outside the lock, the share was removed and
	// we must abort the publish rather than resurrect it.
	_, lmExists := s.lockManagers[shareName]
	// The generation delta is the authoritative removed-mid-flight signal. (The
	// store is not yet in s.stores — it is published below alongside the lock
	// manager — so there is no store pointer to compare here.)
	removedMidFlight := !lmExists && s.removeGen[shareName] != startGen
	if lmExists || removedMidFlight {
		// Our manager may have armed a grace timer above. It was never
		// published, so abort that timer without firing onGraceEnd — letting it
		// run would sweep a surviving manager's locks from the shared store and
		// prematurely end the NFSv4 grace machine. We hold s.mu here, and
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
	// single s.mu acquisition so the share is never observable in a
	// partially-ready state (store visible, lock manager absent). A
	// lockManagerForHandle / storeForHandle that arrives during recovery sees
	// neither and consistently reports the share as not-yet-ready.
	s.stores[shareName] = store
	s.lockManagers[shareName] = lm
	// Wire LockManager as DirChangeNotifier: mutations on this share will
	// dispatch directory lease breaks via the lock manager.
	s.dirChangeNotifiers[shareName] = lm

	return nil
}

// RemoveStoreForShare deregisters a share from the MetadataService, deleting
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
func (s *Service) RemoveStoreForShare(shareName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if lm, ok := s.lockManagers[shareName]; ok && lm != nil {
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
		// false. The coordinator is read under the s.mu we already hold, and
		// OnLockGraceEnd must not block (interface contract), so this is
		// deadlock-free. Exactly-once: if grace had already lifted naturally the
		// onGraceEnd closure already fired OnLockGraceEnd and IsInGracePeriod is
		// false here, so we do not double-fire.
		if lm.IsInGracePeriod() && s.graceCoordinator != nil {
			s.graceCoordinator.OnLockGraceEnd()
		}
		// Abort the grace timer (if armed) so it never fires onGraceEnd against
		// a removed share. AbortGracePeriod stops the timer synchronously and
		// does not block, so holding s.mu across it is safe.
		lm.AbortGracePeriod()
	}

	delete(s.stores, shareName)
	delete(s.lockManagers, shareName)
	delete(s.unifiedViews, shareName)
	delete(s.dirChangeNotifiers, shareName)
	delete(s.quotas, shareName)

	// Bump this share's removal generation so any RegisterStoreForShare recovering
	// it outside s.mu declines to publish: the register snapshots removeGen before
	// recovery and re-checks it at publish (register/remove TOCTOU guard).
	s.removeGen[shareName]++
}

// initLockManagerFromStore stamps a fresh server epoch and replays any locks
// persisted by a previous run back into the lock manager. Errors are logged
// and swallowed so a recovery failure never blocks share registration.
//
// Epoch double-bump on a lost-publish race (R3-5): RegisterStoreForShare runs
// this on a local manager before publishing under s.mu, and the loser of a
// concurrent registration drops its manager. The loser still incremented the
// store epoch here, so two concurrent registrations of the same share advance
// the persisted epoch by 2 instead of 1. This is harmless: the epoch is only a
// monotonic split-brain/stale-lock marker, the surviving manager uses whatever
// epoch it observed, and every lock it restores predates that epoch regardless
// of the gap. Moving IncrementServerEpoch under s.mu would serialize backend IO
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
func (s *Service) newGraceAwareLockManager(duration time.Duration) *LockManager {
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
		s.mu.RLock()
		coordinator := s.graceCoordinator
		s.mu.RUnlock()
		if coordinator != nil {
			coordinator.OnLockGraceEnd()
		}
	})

	lm = lock.NewManagerWithGracePeriod(gpm)
	return lm
}

// GetStoreForShare returns the metadata store for a specific share.
// This is primarily for internal use and testing; protocol handlers
// should use the high-level methods instead.
func (s *Service) GetStoreForShare(shareName string) (Store, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if store, ok := s.stores[shareName]; ok {
		return store, nil
	}

	// The handle decoded but names a share that no longer exists (e.g. held
	// across a RemoveShare). Return a stale-handle StoreError so protocol
	// mappers translate to NFS *STALE / SMB STATUS_FILE_CLOSED instead of a
	// generic server fault.
	return nil, NewStaleHandleError(shareName)
}

// storeForHandle returns the appropriate store for a file handle.
// It extracts the share name from the handle and looks up the store.
//
// A malformed handle propagates DecodeFileHandle's ErrInvalidHandle
// StoreError; a well-formed handle naming an unknown share propagates
// GetStoreForShare's ErrStaleHandle StoreError. Both are *StoreError so the
// protocol error mappers classify them as BADHANDLE/STALE.
func (s *Service) storeForHandle(handle FileHandle) (Store, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}

	return s.GetStoreForShare(shareName)
}

// shareNameForHandle extracts the share name from a file handle.
// Returns empty string if the handle is invalid.
func shareNameForHandle(handle FileHandle) string {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return ""
	}
	return shareName
}

// lockManagerForHandle returns the lock manager for the share that owns the handle.
func (s *Service) lockManagerForHandle(handle FileHandle) (*LockManager, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}
	return s.lockManagerForShare(shareName)
}

// lockManagerForShare returns the lock manager for an already-decoded share
// name. Splitting this out of lockManagerForHandle lets callers that have
// already decoded the handle (see storeAndLockManagerForHandle) avoid a second
// UUID parse.
func (s *Service) lockManagerForShare(shareName string) (*LockManager, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if lm, ok := s.lockManagers[shareName]; ok {
		return lm, nil
	}

	// Decoded handle names a share with no lock manager (removed share):
	// stale-handle StoreError so callers map to *STALE.
	return nil, NewStaleHandleError(shareName)
}

// storeAndLockManagerForHandle resolves both the metadata store and the lock
// manager for a handle with a SINGLE DecodeFileHandle call. The store and
// lock-manager handlers (LockFile, UnlockFile, TestLock, …) need both, and
// calling storeForHandle + lockManagerForHandle separately parsed the same
// handle's UUID twice per operation. The share name is decoded once here and
// reused for both share-keyed lookups. Handle opacity is preserved: callers
// still pass an opaque FileHandle and never see the decoded components.
func (s *Service) storeAndLockManagerForHandle(handle FileHandle) (Store, *LockManager, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, nil, err
	}
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, nil, err
	}
	lm, err := s.lockManagerForShare(shareName)
	if err != nil {
		return nil, nil, err
	}
	return store, lm, nil
}

// GetLockManagerForHandle returns the lock manager for the share that owns
// the given handle. Returns an error if the handle is malformed or no lock
// manager exists for the share.
//
// Used by the SMB blocking-lock async-park path (issue #430): the handler
// needs the conflicting holders' OwnerIDs to feed the Wait-For Graph for
// deadlock detection, which requires direct access to the share's
// LockManager.ListLocks. Permission checks are not needed here — this is
// pure conflict-discovery, not a lock-state mutation.
//
// Thread safety: Safe to call concurrently.
func (s *Service) GetLockManagerForHandle(handle FileHandle) (*LockManager, error) {
	return s.lockManagerForHandle(handle)
}

// GetLockManagerForShare returns the lock manager for a specific share.
//
// This is used by the NFS adapter to process NLM blocking lock waiters.
// Returns nil if no lock manager exists for the share.
//
// Thread safety: Safe to call concurrently.
func (s *Service) GetLockManagerForShare(shareName string) *LockManager {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if lm, ok := s.lockManagers[shareName]; ok {
		return lm
	}
	return nil
}

// GetUnifiedLockView returns the UnifiedLockView for a specific share.
//
// UnifiedLockView provides cross-protocol lock visibility, allowing any protocol
// handler to query all locks (NLM byte-range and SMB leases) on a file.
//
// Returns nil if no UnifiedLockView exists for the share. This can happen if:
//   - The share has not been registered
//   - No LockStore has been set for the share
//
// Thread safety: Safe to call concurrently.
func (s *Service) GetUnifiedLockView(shareName string) *UnifiedLockView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if view, ok := s.unifiedViews[shareName]; ok {
		return view
	}
	return nil
}

// SetUnifiedLockView sets the UnifiedLockView for a specific share.
//
// This is called when a LockStore becomes available for a share (e.g., when
// a store that implements LockStore is registered). Protocol handlers should
// NOT call this directly - it's for internal use by the registration process.
//
// Thread safety: Safe to call concurrently.
func (s *Service) SetUnifiedLockView(shareName string, view *UnifiedLockView) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.unifiedViews[shareName] = view
}

// GetFile retrieves file metadata by handle.
// This is a convenience method that calls GetFile from the Base interface.
// When deferred commits are enabled, it merges pending write state (size, mtime, ctime)
// with the stored file metadata.
func (s *Service) GetFile(ctx context.Context, handle FileHandle) (*File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		return nil, err
	}

	// Check for pending write state (when deferred commits are enabled)
	if pending, ok := s.pendingWrites.GetPending(handle); ok {
		// Merge pending state with stored state
		if pending.MaxSize > file.Size {
			file.Size = pending.MaxSize
		}
		// Update timestamps from pending state
		if pending.LastMtime.After(file.Mtime) {
			file.Mtime = pending.LastMtime
			file.Ctime = pending.LastMtime
		}
		// Apply setuid/setgid clearing
		if pending.ClearSetuidSetgid {
			file.Mode &= ^uint32(0o6000)
		}
	}

	return file, nil
}

// GetFileCached returns file metadata, trying the pending-writes cache first
// to avoid a BadgerDB read. Used on the COMMIT path where WRITE has already
// validated and cached the file. Falls back to the full GetFile path if there
// is no cached entry (e.g., COMMIT without prior WRITE, or cache evicted).
func (s *Service) GetFileCached(ctx context.Context, handle FileHandle) (*File, error) {
	if cached := s.pendingWrites.GetCachedFile(handle); cached != nil {
		// Merge pending state into the cached copy (same logic as GetFile)
		if pending, ok := s.pendingWrites.GetPending(handle); ok {
			if pending.MaxSize > cached.Size {
				cached.Size = pending.MaxSize
			}
			if pending.LastMtime.After(cached.Mtime) {
				cached.Mtime = pending.LastMtime
				cached.Ctime = pending.LastMtime
			}
			if pending.ClearSetuidSetgid {
				cached.Mode &= ^uint32(0o6000)
			}
		}
		return cached, nil
	}
	return s.GetFile(ctx, handle)
}

// CheckPermissions performs file-level permission checking.
// Returns granted permissions (subset of requested).
//
// This implements Unix-style permission checking:
//   - Root (UID 0): Bypass all checks except on read-only shares
//   - Owner: Check owner permission bits
//   - Group member: Check group permission bits
//   - Other: Check other permission bits
//   - Anonymous: Only world permissions
func (s *Service) CheckPermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error) {
	return s.checkFilePermissions(ctx, handle, requested)
}

// GetChild retrieves a child's handle from a directory.
func (s *Service) GetChild(ctx context.Context, dirHandle FileHandle, name string) (FileHandle, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}
	return store.GetChild(ctx, dirHandle, name)
}

// GetRootHandle returns the root handle for a share.
func (s *Service) GetRootHandle(ctx context.Context, shareName string) (FileHandle, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GetRootHandle(ctx, shareName)
}

// GenerateHandle generates a new file handle for a path.
func (s *Service) GenerateHandle(ctx context.Context, shareName, path string) (FileHandle, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GenerateHandle(ctx, shareName, path)
}

// SetQuotaForShare sets the byte quota for a share. 0 means unlimited.
func (s *Service) SetQuotaForShare(shareName string, quotaBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotas[shareName] = quotaBytes
}

// GetQuotaForShare returns the byte quota for a share. 0 means unlimited.
func (s *Service) GetQuotaForShare(shareName string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quotas[shareName]
}

// GetFilesystemStatistics returns filesystem statistics.
// When a quota is configured for the share, the returned TotalBytes and
// AvailableBytes are overlaid with quota-adjusted values. This convenience
// form has no caller identity, so per-user/per-group quotas are not reflected;
// use GetFilesystemStatisticsForIdentity from protocol FSSTAT handlers.
func (s *Service) GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error) {
	return s.GetFilesystemStatisticsForIdentity(ctx, handle, nil)
}

// GetFilesystemStatisticsForIdentity returns filesystem statistics with the
// quota overlay narrowed to the smallest applicable quota: the per-share quota
// AND (when identity is non-nil and a per-user/per-group quota applies) the
// caller's identity quota. This is what `df` / FSSTAT / SMB FS_FULL_SIZE report,
// so a quota'd user sees their own ceiling rather than the raw volume.
func (s *Service) GetFilesystemStatisticsForIdentity(ctx context.Context, handle FileHandle, identity *Identity) (*FilesystemStatistics, error) {
	// Decode once: the share name is reused for the quota overlay below.
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	stats, err := store.GetFilesystemStatistics(ctx, handle)
	if err != nil {
		return nil, err
	}

	// Apply per-share quota overlay if configured.
	if quotaBytes := s.GetQuotaForShare(shareName); quotaBytes > 0 {
		applyByteQuotaOverlay(stats, uint64(quotaBytes), stats.UsedBytes)
	}

	// Apply the tighter of the caller's user / group quota, if any.
	s.applyIdentityQuotaOverlay(shareName, store, stats, identity)

	return stats, nil
}

// applyByteQuotaOverlay narrows TotalBytes/AvailableBytes to a byte ceiling
// against the given used value. Only narrows (never widens) Total so the
// smallest applicable quota wins across successive overlays.
func applyByteQuotaOverlay(stats *FilesystemStatistics, ceiling, used uint64) {
	if ceiling == 0 {
		return
	}
	if stats.TotalBytes == 0 || ceiling < stats.TotalBytes {
		stats.TotalBytes = ceiling
	}
	var avail uint64
	if used < ceiling {
		avail = ceiling - used
	}
	if avail < stats.AvailableBytes {
		stats.AvailableBytes = avail
	}
}

// applyFileQuotaOverlay narrows TotalFiles/AvailableFiles to an inode ceiling.
func applyFileQuotaOverlay(stats *FilesystemStatistics, ceiling, used uint64) {
	if ceiling == 0 {
		return
	}
	if stats.TotalFiles == 0 || ceiling < stats.TotalFiles {
		stats.TotalFiles = ceiling
	}
	var avail uint64
	if used < ceiling {
		avail = ceiling - used
	}
	if avail < stats.AvailableFiles {
		stats.AvailableFiles = avail
	}
}

// applyIdentityQuotaOverlay narrows the stats to the caller's per-user and
// per-group quota (bytes + inodes), using that identity's live usage rather
// than the share-wide used total. No-op when identity is nil or has no UID, or
// when no quota applies.
func (s *Service) applyIdentityQuotaOverlay(shareName string, store Store, stats *FilesystemStatistics, identity *Identity) {
	if identity == nil || identity.UID == nil || !s.identityQuotas.hasAny(shareName) {
		return
	}
	uid := *identity.UID

	if iq, ok := s.identityQuotas.resolveUser(shareName, uid); ok {
		if usage, err := store.GetQuotaUsage(QuotaScopeUser, uid); err == nil {
			if iq.LimitBytes > 0 {
				applyByteQuotaOverlay(stats, uint64(iq.LimitBytes), uint64(max64(usage.Bytes, 0)))
			}
			if iq.LimitFiles > 0 {
				applyFileQuotaOverlay(stats, uint64(iq.LimitFiles), uint64(max64(usage.Files, 0)))
			}
		}
	}
	if identity.GID != nil {
		gid := *identity.GID
		if iq, ok := s.identityQuotas.get(shareName, QuotaScopeGroup, gid); ok {
			if usage, err := store.GetQuotaUsage(QuotaScopeGroup, gid); err == nil {
				if iq.LimitBytes > 0 {
					applyByteQuotaOverlay(stats, uint64(iq.LimitBytes), uint64(max64(usage.Bytes, 0)))
				}
				if iq.LimitFiles > 0 {
					applyFileQuotaOverlay(stats, uint64(iq.LimitFiles), uint64(max64(usage.Files, 0)))
				}
			}
		}
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// GetFilesystemCapabilities returns filesystem capabilities.
func (s *Service) GetFilesystemCapabilities(ctx context.Context, handle FileHandle) (*FilesystemCapabilities, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	return store.GetFilesystemCapabilities(ctx, handle)
}

// CheckLockForIO checks if an I/O operation is blocked by locks.
//
// This is a lightweight operation that doesn't verify file existence,
// allowing fast path for I/O operations.
// openID identifies the specific open performing the I/O (empty string falls back to sessionID).
func (s *Service) CheckLockForIO(ctx context.Context, handle FileHandle, openID string, sessionID uint64, offset, length uint64, isWrite bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	handleKey := string(handle)
	conflict := lm.CheckForIO(handleKey, openID, sessionID, offset, length, isWrite)
	if conflict != nil {
		return NewLockedError("", conflict)
	}
	return nil
}

// LockFile acquires a byte-range lock on a file.
//
// Business logic:
//   - Verifies file exists
//   - Verifies file is not a directory (directories cannot be locked)
//   - Checks user has appropriate permission (read for shared, write for exclusive)
func (s *Service) LockFile(ctx *AuthContext, handle FileHandle, lock FileLock) error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// Verify file exists and is not a directory
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	if file.Type == FileTypeDirectory {
		return NewIsDirectoryError("")
	}

	// Check permissions
	var requiredPerm Permission
	if lock.Exclusive {
		requiredPerm = PermissionWrite
	} else {
		requiredPerm = PermissionRead
	}

	// Route through the shared permission funnel rather than calling
	// calculatePermissions directly: checkFilePermissions applies the per-user
	// read-only ceiling (#1276 — a read-only user must not take an exclusive
	// write lock) and the SMB handle-based write authorization, keeping lock
	// authorization consistent with every other write path.
	granted, err := s.checkFilePermissions(ctx, handle, requiredPerm)
	if err != nil {
		return err
	}
	if granted&requiredPerm == 0 {
		return NewPermissionDeniedError("")
	}

	// Acquire the lock via LockManager
	handleKey := string(handle)
	return lm.Lock(handleKey, lock)
}

// UnlockFile releases a byte-range lock on a file.
//
// Note: Takes context.Context instead of *AuthContext because:
// - Open/Session ID identifies the lock owner (you can only unlock your own locks)
// - No permission checking needed for unlock operations
// openID identifies the specific open that owns the lock (empty string falls back to sessionID).
func (s *Service) UnlockFile(ctx context.Context, handle FileHandle, openID string, sessionID uint64, offset, length uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// Verify file exists
	_, err = store.GetFile(ctx, handle)
	if err != nil {
		return err
	}

	handleKey := string(handle)
	return lm.Unlock(handleKey, openID, sessionID, offset, length)
}

// UnlockAllForSession releases all locks held by a session on a file.
func (s *Service) UnlockAllForSession(ctx context.Context, handle FileHandle, sessionID uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// No file existence check - file may have been deleted
	handleKey := string(handle)
	lm.UnlockAllForSession(handleKey, sessionID)
	return nil
}

// UnlockAllForOpen releases all locks held by a specific open on a file.
func (s *Service) UnlockAllForOpen(ctx context.Context, handle FileHandle, openID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// No file existence check - file may have been deleted
	handleKey := string(handle)
	lm.UnlockAllForOpen(handleKey, openID)
	return nil
}

// TestLock tests if a lock would conflict with existing locks.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - bool: true if lock would succeed, false if conflict exists
//   - *LockConflict: Details of conflicting lock if bool is false
func (s *Service) TestLock(ctx *AuthContext, handle FileHandle, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict, error) {
	if err := ctx.Context.Err(); err != nil {
		return false, nil, err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return false, nil, err
	}

	// Verify file exists
	_, err = store.GetFile(ctx.Context, handle)
	if err != nil {
		return false, nil, err
	}

	handleKey := string(handle)
	ok, conflict := lm.TestLockByParams(handleKey, sessionID, offset, length, exclusive)
	return ok, conflict, nil
}

// ListLocks lists all locks on a file.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - []FileLock: All active locks on the file (empty slice if none)
func (s *Service) ListLocks(ctx *AuthContext, handle FileHandle) ([]FileLock, error) {
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	store, lm, err := s.storeAndLockManagerForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Verify file exists
	_, err = store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	handleKey := string(handle)
	locks := lm.ListLocks(handleKey)
	if locks == nil {
		return []FileLock{}, nil
	}
	return locks, nil
}

// RemoveFileLocks removes all locks for a file.
// Called when a file is deleted to clean up stale lock entries.
func (s *Service) RemoveFileLocks(handle FileHandle) {
	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return // No lock manager means no locks to remove
	}

	handleKey := string(handle)
	lm.RemoveFileLocks(handleKey)
}

// CreateShare creates a new share with its root directory.
func (s *Service) CreateShare(ctx context.Context, shareName string, share *Share) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}
	return store.CreateShare(ctx, share)
}

// GetShareOptions returns the options for a share.
func (s *Service) GetShareOptions(ctx context.Context, shareName string) (*ShareOptions, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GetShareOptions(ctx, shareName)
}

// SetDirChangeNotifier registers a DirChangeNotifier for a share.
//
// When directory mutations occur on this share (create, remove, rename),
// the notifier will be called to dispatch directory lease breaks.
// Typically the LockManager is used as the notifier since it implements
// lock.DirChangeNotifier.
//
// Thread safety: Safe to call concurrently.
func (s *Service) SetDirChangeNotifier(shareName string, n lock.DirChangeNotifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirChangeNotifiers[shareName] = n
}

// notifyDirChange dispatches a directory change notification for a share.
//
// This is fire-and-forget: notifications do NOT affect the success/failure
// of the mutation that triggered them. If the notifier is nil or not
// registered for the share, the call is silently ignored.
//
// The originClientID is extracted from the AuthContext's LockClientID field
// (falling back to ClientAddr) to identify the originating client so their
// own leases aren't broken.
func (s *Service) notifyDirChange(shareName string, parentHandle FileHandle, changeType lock.DirChangeType, ctx *AuthContext) {
	s.mu.RLock()
	notifier, ok := s.dirChangeNotifiers[shareName]
	s.mu.RUnlock()

	if !ok || notifier == nil {
		return
	}

	originClient := ""
	var excludeParentKey [16]byte
	var hasExcludeKey bool
	if ctx != nil {
		originClient = ctx.LockClientID
		if originClient == "" {
			originClient = ctx.ClientAddr
		}
		// Thread the originating handle's RqLs ParentLeaseKey into the
		// notifier so the dir-lease parent-key suppression rule (MS-SMB2
		// §3.3.4.20, #470 C2) can skip the matching parent dir lease.
		// NFS callers leave HasParentLeaseKey=false.
		if ctx.HasParentLeaseKey {
			excludeParentKey = ctx.ParentLeaseKey
			hasExcludeKey = true
		}
	}

	// Fire-and-forget: notifier handles dispatch; recover from panics
	defer func() {
		if r := recover(); r != nil {
			logger.Error("notifyDirChange: panic in notifier", "share", shareName, "error", r)
		}
	}()
	notifier.OnDirChange(lock.FileHandle(parentHandle), changeType, originClient, excludeParentKey, hasExcludeKey)
}
