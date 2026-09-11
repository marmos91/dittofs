package state

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

const DefaultLeaseDuration = 90 * time.Second

// StateManager is the central coordinator for all NFSv4 state.
// It owns client records, open-owner tables, stateid maps, lease timers,
// and grace period state.
// All state modifications go through StateManager methods to ensure
// thread safety and consistency.
//
// Per the research anti-pattern advice, a single RWMutex protects all state
// to avoid deadlocks between interdependent lookups (client -> open-owner ->
// stateid -> lease).

type StateManager struct {
	mu sync.RWMutex

	// clientsByID maps server-assigned client IDs to client records of both
	// minor versions: v4.0 records (established by SETCLIENTID, MinorVersion 0)
	// and v4.1 records (established by EXCHANGE_ID, MinorVersion 1). The two
	// never hold the same ID because generateClientID draws both flows from one
	// sequence, and ClientRecord.MinorVersion says which flow minted each
	// entry. Version-sensitive operations read it through the v40Client /
	// v41Client helpers below; version-agnostic readers (e.g. the shared
	// recovery-key switch, delegation handback) may index the map directly.
	clientsByID map[uint64]*ClientRecord

	// clientsByName maps nfs_client_id4.id strings to confirmed client records.
	clientsByName map[string]*ClientRecord

	// unconfirmedByName maps nfs_client_id4.id strings to unconfirmed
	// client records (pending SETCLIENTID_CONFIRM).
	unconfirmedByName map[string]*ClientRecord

	// openStateByOther maps stateid "other" fields to OpenState records.
	// This is the primary lookup table for stateid validation.
	openStateByOther map[[types.NFS4_OTHER_SIZE]byte]*OpenState

	// openStateByFile indexes live OpenState records by file handle
	// (string(fileHandle)). It is a secondary index kept consistent with
	// openStateByOther at every insert/remove so share-conflict checks
	// (shareConflictLocked) and delegation grant checks (countOpensOnFile)
	// iterate only the opens on the target file instead of scanning every
	// open in the server. Maintained via addOpenStateToFileLocked /
	// removeOpenStateFromFileLocked.
	openStateByFile map[string][]*OpenState

	// openOwners maps open-owner keys to OpenOwner records.
	// Key is composite of clientID + hex(ownerData).
	openOwners map[openOwnerKey]*OpenOwner

	// closedOwnerByOther maps the "other" of a now-closed open stateid to the
	// retained open-owner. CLOSE deletes the OpenState, so a retransmitted
	// CLOSE can no longer resolve the owner via openStateByOther; this map lets
	// the CLOSE replay path find the owner (and its cached reply) to satisfy
	// the NFSv4.0 exactly-once contract (RFC 7530 §9.1.7). Entries are cleared
	// when the owner is reaped (lease expiry) or the stateid is reused.
	closedOwnerByOther map[[types.NFS4_OTHER_SIZE]byte]*OpenOwner

	// expiredStateids holds the "other" of every stateid whose state was freed
	// because the owning client's lease was cancelled. Without it the freed
	// stateid is simply absent from the tables and looks like one the server
	// never issued, so the client is told NFS4ERR_BAD_STATEID when RFC 7530
	// §9.6.3.2 requires NFS4ERR_EXPIRED ("When a lease is canceled, all locking
	// state associated with it is freed, and the use of any of the associated
	// stateids will result in NFS4ERR_EXPIRED being returned").
	//
	// ponytail: capped and dropped wholesale on overflow rather than aged out
	// per entry; losing an entry only degrades the answer to the
	// NFS4ERR_BAD_STATEID the server gave before, and both statuses send the
	// client into the same recovery. Give entries timestamps and sweep them if
	// a deployment is ever seen to overflow the cap with clients still retrying.
	expiredStateids map[[types.NFS4_OTHER_SIZE]byte]struct{}

	// lockOwners maps lock-owner keys to LockOwner records.
	// Key is composite of clientID + hex(ownerData), same pattern as openOwners.
	lockOwners map[lockOwnerKey]*LockOwner

	// lockStateByOther maps lock stateid "other" fields to LockState records.
	// This is the primary lookup table for lock stateid validation.
	lockStateByOther map[[types.NFS4_OTHER_SIZE]byte]*LockState

	// delegByOther maps delegation stateid "other" fields to DelegationState records.
	// This is the primary lookup table for delegation stateid validation.
	delegByOther map[[types.NFS4_OTHER_SIZE]byte]*DelegationState

	// delegByFile maps file handle (string key) to a list of DelegationState records.
	// Used for conflict detection: "does any client hold a delegation for this file?"
	delegByFile map[string][]*DelegationState

	// revokedDelegCount counts, per client ID, the number of delegations that
	// have been marked Revoked but are still retained in delegByOther (for
	// stale-stateid detection). It is a secondary index so GetStatusFlags can
	// answer "does this client have any revoked delegation?" in O(1) on the
	// SEQUENCE hot path instead of scanning every delegation in the server.
	// Maintained via markDelegRevokedLocked and deleteDelegByOtherLocked.
	revokedDelegCount map[uint64]int

	// delegStateidMap maps LockManager DelegationID (UUID string) to NFS Stateid4.
	// This enables NFSBreakHandler to look up the NFS wire-format stateid when
	// a delegation recall is dispatched by the shared LockManager.
	delegStateidMap map[string]types.Stateid4

	// recentlyRecalled tracks file handles that were recently involved in
	// delegation recalls. Prevents grant-recall-grant-recall storms.
	// Key: string(fileHandle), Value: time of recall.
	recentlyRecalled map[string]time.Time

	// recentlyRecalledTTL is the duration for which a file is considered
	// recently recalled. Defaults to RecentlyRecalledTTL (30s).
	recentlyRecalledTTL time.Duration

	// delegationsEnabled controls whether delegations can be granted.
	// When false, ShouldGrantDelegation always returns OPEN_DELEGATE_NONE.
	// Defaults to true; updated from live adapter settings.
	delegationsEnabled bool

	// maxDelegations is the maximum total outstanding delegations (file + directory).
	// 0 means unlimited. Updated via SetMaxDelegations.
	maxDelegations int

	// dirDelegBatchWindow is the duration of the notification batching window
	// for directory delegations. Defaults to 50ms.
	dirDelegBatchWindow time.Duration

	// lockManager is a static unified lock manager used as a fallback when no
	// resolver is set (primarily by tests). See SetLockManagerResolver.
	lockManager lock.LockManager

	// lockManagerResolver resolves the per-share unified lock manager for a file
	// handle, taking precedence over lockManager. See SetLockManagerResolver.
	lockManagerResolver func(handle []byte) lock.LockManager

	// bootEpoch identifies this incarnation of the server. It is the high 32
	// bits of every client ID and a 24-bit fragment of every stateid, and both
	// readers only ever test it for equality, never for order.
	bootEpoch uint32

	// nextClientSeq is an atomic counter for the low 32 bits of client IDs.
	nextClientSeq uint32

	// leaseDuration is the configured lease duration for all clients.
	leaseDuration time.Duration

	// gracePeriod tracks the NFSv4 grace period state for server restart recovery.
	// Created lazily when StartGracePeriod is called.
	gracePeriod *GracePeriodState

	// graceDuration is the duration of the grace period.
	// Defaults to leaseDuration if not explicitly set.
	graceDuration time.Duration

	// recoveryStore provides server-global durable persistence of confirmed
	// client identities for reboot/grace recovery. Optional: when
	// nil the StateManager behaves exactly as before (no durable recovery),
	// which keeps bare test constructions working. Set via
	// SetClientRecoveryStore, wired by the NFS adapter to the first share's
	// metadata store implementing lock.ClientRecoveryStore (mirroring the NSM
	// ClientRegistrationStore designation).
	recoveryStore lock.ClientRecoveryStore

	// serverEpoch is the epoch under which this server instance is running.
	// Stamped into every client-recovery record so stale records from prior
	// instances can be GC'd. Set alongside recoveryStore.
	serverEpoch uint64

	// bootRecoveryVerifiers snapshots, at boot-load time, the BootVerifier of
	// every prior-instance recovery record keyed by ClientIDString. CLAIM_PREVIOUS
	// is validated against THIS snapshot (RFC 7530 §9.1.4): a reclaiming client
	// whose verifier differs from its pre-restart durable verifier rebooted and
	// must not reclaim. Validating against the snapshot (not the live store) is
	// essential — a rebooting client's SETCLIENTID_CONFIRM overwrites its live
	// record with the new verifier before it reaches CLAIM_PREVIOUS. Populated
	// only by LoadClientRecovery; nil/empty otherwise (reclaim ungated).
	bootRecoveryVerifiers map[string][8]byte

	// pendingReclaimPersists tracks the at-most-one live retry chain per
	// recovery key (see pendingReclaimPersist). A re-schedule while an entry
	// exists adopts it instead of forking a second chain, so a down backend
	// cannot pile up chains and a new issuer's persist failure is not skipped
	// while an old chain lives. Entries are dropped on success or when the
	// issuer no longer holds the key.
	pendingReclaimPersists map[string]*pendingReclaimPersist

	// ============================================================================
	// Version-sensitive lookup helpers
	// ============================================================================
	// (methods on StateManager; see v40ClientLocked / v41ClientLocked below)

	// v41ClientsByOwner maps v4.1 owner ID bytes (string key) to client
	// records. EXCHANGE_ID resolves by owner, which no v4.0 flow supplies,
	// so every entry is a MinorVersion-1 record by construction.
	v41ClientsByOwner map[string]*ClientRecord

	// sessionsByID maps session IDs to session objects.
	sessionsByID map[types.SessionId4]*Session

	// sessionsByClientID maps client IDs to their sessions.
	sessionsByClientID map[uint64][]*Session

	// maxSessionsPerClient is the per-client session limit (default 16).
	maxSessionsPerClient int

	// foreMaxSlots is the server-imposed maximum fore channel slots per session.
	// Defaults to 64; updated via SetMaxSessionSlots from adapter settings.
	foreMaxSlots uint32

	// serverIdentity is the immutable server identity returned in EXCHANGE_ID responses.
	serverIdentity *ServerIdentity

	// ============================================================================
	// Connection Binding State
	// ============================================================================

	// connMu protects connection binding maps. Separate from sm.mu to avoid
	// contention between session operations (sm.mu) and connection binding
	// operations (connMu). Lock ordering: sm.mu before connMu (never reverse).
	connMu sync.RWMutex

	// connByID maps connectionID -> binding.
	connByID map[uint64]*BoundConnection

	// connBySession maps sessionID -> list of bindings.
	connBySession map[types.SessionId4][]*BoundConnection

	// maxConnsPerSession is the maximum number of connections per session (default 16).
	maxConnsPerSession int

	// ============================================================================
	// Backchannel State
	// ============================================================================

	// connWriters maps connectionID -> ConnWriter callback for writing backchannel
	// data to a TCP connection. Registered when a connection is bound for back-channel.
	// Protected by connMu.
	connWriters map[uint64]ConnWriter

	// cbRepliesByConn maps connectionID -> PendingCBReplies for routing backchannel
	// REPLY messages to the sender goroutine. Protected by connMu.
	cbRepliesByConn map[uint64]*PendingCBReplies

	// backchannelFaults tracks per-client backchannel fault state. Set when a
	// callback send fails, cleared on success. Protected by connMu.
	backchannelFaults map[uint64]bool

	// cbNullFunc verifies the callback path during SETCLIENTID_CONFIRM. It
	// defaults to SendCBNull and is only overridden by tests (set once at
	// construction, before any goroutine reads it).
	cbNullFunc func(context.Context, CallbackInfo) error
}

// NewStateManager creates a new StateManager with the given lease duration.
// The boot epoch is drawn at random.
// An optional graceDuration parameter controls the grace period length;
// if omitted or zero, the lease duration is used.

func NewStateManager(leaseDuration time.Duration, graceDuration ...time.Duration) *StateManager {
	if leaseDuration <= 0 {
		leaseDuration = DefaultLeaseDuration
	}

	gd := leaseDuration
	if len(graceDuration) > 0 && graceDuration[0] > 0 {
		gd = graceDuration[0]
	}

	// The boot epoch has to differ from the last incarnation's, because
	// generateClientID restarts its sequence at 0 every boot: two runs sharing
	// an epoch hand out byte-identical client IDs, and a stale one is then
	// admitted as a live client rather than refused. A clock read at seconds
	// resolution guaranteed that collision for any restart inside one second,
	// which is well within a supervisor's restart time.
	//
	// ponytail: random and not persisted, so nothing rules out drawing the same
	// epoch twice -- the 24-bit stateid fragment puts that at roughly one
	// restart in 16 million before a stale stateid could read as current.
	// Persisting the epoch and reloading it incremented removes the chance
	// outright; the durable client-recovery store is already handed this value,
	// so the seam to persist it through exists.
	// From Go 1.24 the default crypto/rand Reader calls fatal() rather than
	// returning an error, so a partial fill that would leave this epoch zeroed
	// is not reachable and the error is dead, exactly as at the other draw in
	// generateStateidOther. If the module ever drops below Go 1.24 both must
	// become real error checks.
	var epochBytes [4]byte
	_, _ = rand.Read(epochBytes[:])
	epoch := binary.BigEndian.Uint32(epochBytes[:])

	return &StateManager{
		clientsByID:         make(map[uint64]*ClientRecord),
		clientsByName:       make(map[string]*ClientRecord),
		openStateByOther:    make(map[[types.NFS4_OTHER_SIZE]byte]*OpenState),
		openStateByFile:     make(map[string][]*OpenState),
		openOwners:          make(map[openOwnerKey]*OpenOwner),
		closedOwnerByOther:  make(map[[types.NFS4_OTHER_SIZE]byte]*OpenOwner),
		expiredStateids:     make(map[[types.NFS4_OTHER_SIZE]byte]struct{}),
		lockOwners:          make(map[lockOwnerKey]*LockOwner),
		lockStateByOther:    make(map[[types.NFS4_OTHER_SIZE]byte]*LockState),
		delegByOther:        make(map[[types.NFS4_OTHER_SIZE]byte]*DelegationState),
		delegByFile:         make(map[string][]*DelegationState),
		revokedDelegCount:   make(map[uint64]int),
		delegStateidMap:     make(map[string]types.Stateid4),
		recentlyRecalled:    make(map[string]time.Time),
		recentlyRecalledTTL: RecentlyRecalledTTL,
		delegationsEnabled:  true,
		bootEpoch:           epoch,
		leaseDuration:       leaseDuration,
		graceDuration:       gd,
		// v4.0 SETCLIENTID confirmation state
		unconfirmedByName: make(map[string]*ClientRecord),
		// v4.1 state
		v41ClientsByOwner:      make(map[string]*ClientRecord),
		sessionsByID:           make(map[types.SessionId4]*Session),
		sessionsByClientID:     make(map[uint64][]*Session),
		maxSessionsPerClient:   16,
		foreMaxSlots:           64,
		serverIdentity:         newServerIdentity(epoch),
		pendingReclaimPersists: make(map[string]*pendingReclaimPersist),
		// Connection binding state
		connByID:           make(map[uint64]*BoundConnection),
		connBySession:      make(map[types.SessionId4][]*BoundConnection),
		maxConnsPerSession: 16,
		// Backchannel state
		connWriters:       make(map[uint64]ConnWriter),
		cbRepliesByConn:   make(map[uint64]*PendingCBReplies),
		backchannelFaults: make(map[uint64]bool),
		cbNullFunc:        SendCBNull,
	}
}

// LeaseDuration returns the configured lease duration.

func (sm *StateManager) LeaseDuration() time.Duration {
	return sm.leaseDuration
}

// BootEpoch returns the server boot epoch used for client ID generation.

func (sm *StateManager) BootEpoch() uint32 {
	return sm.bootEpoch
}

// generateClientID creates a unique 64-bit client ID by combining
// the boot epoch (high 32 bits) with a monotonic counter (low 32 bits).
// This ensures client IDs are unique across server restarts.

func (sm *StateManager) Shutdown() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, record := range sm.clientsByID {
		if record.Lease != nil {
			record.Lease.Stop()
		}
	}

	// Stop all active delegation recall timers and directory delegation
	// batch timers to prevent timer goroutines firing after shutdown.
	for _, deleg := range sm.delegByOther {
		deleg.StopRecallTimer()
		sm.cleanupDirDelegation(deleg)
	}

	// Stop all backchannel senders to prevent orphan goroutines
	for _, session := range sm.sessionsByID {
		if session.backchannelSender != nil {
			session.backchannelSender.Stop()
			session.backchannelSender = nil
		}
	}

	if sm.gracePeriod != nil {
		sm.gracePeriod.Stop()
	}

	logger.Info("StateManager: all lease, recall timers, and backchannel senders stopped")
}

// ============================================================================
// Grace Period Operations
// ============================================================================

// StartGracePeriod creates and starts a grace period for server restart recovery.
//
// The NFS adapter should call this on startup if there were previous clients
// (loaded from a saved client state file). During the grace period:
//   - OPEN with CLAIM_NULL returns NFS4ERR_GRACE
//   - OPEN with CLAIM_PREVIOUS is allowed (reclaim)
//   - RENEW, CLOSE, READ/WRITE with existing stateids work normally
//
// The grace period ends automatically after graceDuration, or early if
// all expectedClientIDs have reclaimed. If expectedClientIDs is empty,
// the grace period is skipped entirely.
