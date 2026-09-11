package handlers

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	authkerberos "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	pkgidentity "github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

type Handler struct {
	Registry  smbRuntime
	StartTime time.Time

	// Server identity
	ServerGUID [16]byte

	// Session management (unified with credit tracking)
	SessionManager *session.Manager

	// Pending auth sessions (mid-handshake). Keyed by pendingAuthKey so that
	// concurrent SESSION_SETUPs on the same session from different connections
	// (e.g. multiple parallel channel binds, MS-SMB2 §3.3.5.5.2) do not clobber
	// each other's TYPE_2 challenge / ServerChallenge. See Samba bug 15346.
	pendingAuth sync.Map // pendingAuthKey -> *PendingAuth

	// Tree connections
	trees      sync.Map // treeID -> *TreeConnection
	nextTreeID atomic.Uint32

	// Open files
	files      sync.Map // string(fileID) -> *OpenFile
	nextFileID atomic.Uint64

	// renameScanMu serializes a SET_INFO rename's share-mode conflict
	// scan-and-decision against a concurrent CLOSE's authoritative handle
	// removal. The rename path reads the lock-free `files` sync.Map to decide
	// whether a conflicting handle still exists (checkShareDeleteConflict,
	// checkParentDirRenameConflict, anyOpenChild); CLOSE removes the OpenFile
	// from `files` and then releases the lease / signals the rename's
	// break-wait. Without serialization the rename's post-break re-scan can
	// observe a holder that a concurrent CLOSE is in the middle of removing —
	// the OpenFile is not-yet-deleted from `files` even though the holder has
	// already signalled the break complete — yielding a spurious
	// STATUS_SHARING_VIOLATION (intermittent smbtorture
	// rename_dir_bench / close-full-information / dirlease.rename_dst_parent).
	//
	// Lock order / deadlock-safety: the rename MUST NOT hold this mutex while
	// performing a lease break-WAIT, because that wait can only complete when a
	// holder CLOSEs — and CLOSE needs this same mutex to remove the handle and
	// signal. The rename therefore does every break-WAIT OUTSIDE the mutex and
	// takes it only for the final authoritative re-scan + decision, where it
	// reads the now-quiescent `files` map. CLOSE holds it across the handle
	// removal + lease release/signal (close.go step 10/11). The mutex never
	// nests under any LockManager/lease lock: the rename's `files.Range` scans
	// touch only the sync.Map, and CLOSE's WaitAndDeleteOpenFile drains only
	// the *closing* handle's own in-flight ops (never a rename, which registers
	// under its own distinct FileID).
	renameScanMu sync.Mutex

	// docElectionMu serializes the delete-on-close last-handle election
	// (electDeleteOnClose) across every closing handle: it makes each closer's
	// "am I the last handle on this file?" scan atomic with its own departure
	// from the set of handles that can still honour a delete-on-close
	// (OpenFile.docLeaving). See doc_election.go.
	//
	// Lock order: leaf. The election takes only per-OpenFile locks under it and
	// does no I/O, and no other lock is acquired while it is held — both close
	// paths have released it well before they take renameScanMu.
	docElectionMu sync.Mutex

	// Named pipe management (for IPC$ RPC)
	PipeManager *rpc.PipeManager

	// Lease management (thin wrapper over shared LockManager)
	LeaseManager *lease.LeaseManager

	// Change notification management
	NotifyRegistry *NotifyRegistry
	nextAsyncId    atomic.Uint64

	// Named-pipe async READ tracking
	PipeReadRegistry *PipeReadRegistry

	// PendingCreateRegistry tracks CREATE requests parked on a lease break
	// (MS-SMB2 §3.3.5.9 + §3.3.4.7). The resume goroutine waits for the break
	// to drain, then delivers the final response via AsyncCreateCompleteCallback.
	PendingCreateRegistry *PendingCreateRegistry

	// PendingLockRegistry tracks SMB2 LOCK requests parked on a byte-range
	// conflict (MS-SMB2 §3.3.5.14). The resume goroutine retries the
	// acquisition until success / timeout / cancellation, then delivers the
	// final response via AsyncLockCompleteCallback. See pending_lock_registry.go.
	PendingLockRegistry *PendingLockRegistry

	// CreateReplayCache backs SMB3 replay protection for CREATE
	// (MS-SMB2 §3.3.5.9). When a client sets SMB2_FLAGS_REPLAY_OPERATION
	// on a CREATE carrying a DH2Q with CreateGuid X, the handler
	// consults this cache first: a hit returns the original response
	// (avoiding STATUS_SHARING_VIOLATION on the legitimate retry); a
	// miss falls through to the normal CREATE path and the success
	// response is stored for the next replay window. See
	// replay_cache.go.
	CreateReplayCache *CreateReplayCache

	// LockReplayCache backs SMB3 replay protection for LOCK
	// (MS-SMB2 §3.3.5.14). Keyed by (FileID, LockSequenceIndex),
	// stores the last LockSequenceNumber + status pair so a replayed
	// LOCK with matching slot returns the cached status verbatim
	// instead of trying to re-acquire / re-release (which would trip
	// STATUS_RANGE_NOT_LOCKED or STATUS_LOCK_NOT_GRANTED). See
	// replay_cache.go.
	LockReplayCache *LockReplayCache

	// LockWaitGraph tracks "is waiting for" relationships among byte-range
	// lock owners. Consulted by the blocking-LOCK async-park path before
	// committing to wait, so a request whose grant would close a cycle is
	// rejected with STATUS_LOCK_NOT_GRANTED instead (MS-SMB2 §3.3.5.14,
	// smb2.lock.open-brlock-deadlock / ctdb-delrec-deadlock).
	LockWaitGraph *lock.WaitForGraph

	// Pending blocking lock operations (lockMsgKey -> cancel func). Legacy
	// path for inline retry inside the request goroutine — used as a
	// fallback when async parking is unavailable (no callback wired,
	// async-credit pool exhausted, or registry full).
	pendingLocks sync.Map

	// Configuration
	MaxTransactSize uint32
	MaxReadSize     uint32
	MaxWriteSize    uint32

	// Signing configuration
	SigningConfig signing.SigningConfig

	// Dialect range configuration (set by adapter from SMBAdapterSettings).
	// MinDialect is the minimum dialect the server will negotiate.
	// MaxDialect is the maximum dialect the server will negotiate.
	// Defaults: MinDialect=0x0202 (SMB 2.0.2), MaxDialect=0x0210 (SMB 2.1).
	// Configurable to 0x0311 (SMB 3.1.1) via SMBAdapterSettings when SMB3 is ready.
	MinDialect types.Dialect
	MaxDialect types.Dialect

	// Encryption configuration for enforcement decisions.
	// Propagated from the adapter's EncryptionConfig during initialization.
	EncryptionConfig EncryptionConfig

	// SigningAlgorithmPreference is the server's preference order for signing
	// algorithms, used during SIGNING_CAPABILITIES negotiate context processing.
	// The first element is the most preferred. If empty, defaults to
	// [AES-128-GMAC, AES-128-CMAC]. HMAC-SHA256 is excluded because
	// SIGNING_CAPABILITIES is a 3.1.1-only context.
	SigningAlgorithmPreference []uint16

	// EncryptionEnabled controls whether CapEncryption is advertised for SMB 3.0+.
	// When false, encryption capabilities are not offered during negotiate.
	EncryptionEnabled bool

	// DirectoryLeasingEnabled controls whether CapDirectoryLeasing is advertised for SMB 3.0+.
	// Defaults to true.
	DirectoryLeasingEnabled bool

	// Cached share list for pipe CREATE operations (IPC$).
	// Protected by sharesCacheMu. Invalidated via Runtime.OnShareChange().
	cachedShares     []rpc.ShareInfo1
	sharesCacheMu    sync.RWMutex
	sharesCacheValid bool

	// durablePurgeMu serializes the read-then-mutate windows on the
	// DurableHandleStore that would otherwise TOCTOU around each other:
	//
	//   (a) disconnect path: GetLeaseState → buildPersistedDurableHandle →
	//       PutDurableHandle (the "persist" half of close).
	//   (b) create path: purgeConflictingDisconnectedHandlesForOpen and
	//       purgeConflictingDisconnectedHandlesForDataChange
	//       (GetDurableHandlesByFileHandle → loop DeleteDurableHandle).
	//
	// Without serialization a CREATE in (b) can race against a concurrent
	// disconnect in (a): the disconnect's PutDurableHandle lands between the
	// CREATE's Get and Delete, leaving a phantom unreconnectable entry until
	// the scavenger evicts it.
	//
	// The mutex is process-wide and (b) is reached from WRITE and SET_INFO, so
	// taking it per operation would serialize every writer in the server behind
	// one lock plus a durable-store round trip. The (b) scans therefore gate on
	// disconnectedByFile below and take the mutex only for files that actually
	// have a disconnected handle.
	durablePurgeMu sync.Mutex

	// disconnectedByFile counts the durable handles currently persisted in the
	// DurableHandleStore in the disconnected state, keyed by metadata handle;
	// disconnectedTotal is their sum, so the common "no disconnected handle
	// anywhere" case costs one atomic load.
	//
	// The counts are an upper bound, never an under-count: an entry is added
	// BEFORE PutDurableHandle makes the row visible, and under durablePurgeMu,
	// so no scan can observe zero while a row exists. Not every delete path
	// decrements — a purge scan reconciles the count for the file it just read
	// from the store, so an over-count costs one slow-path scan and then
	// converges. Only the add side is load-bearing.
	disconnectedMu     sync.RWMutex
	disconnectedByFile map[string]int
	disconnectedTotal  atomic.Int64

	// KerberosProvider holds the shared Kerberos keytab/config provider.
	// Injected by the adapter layer before Serve(). When nil, Kerberos
	// auth returns STATUS_LOGON_FAILURE gracefully.
	KerberosProvider *kerberos.Provider

	// KerberosService handles AP-REQ verification, replay detection, and
	// AP-REP construction. Created from KerberosProvider.
	KerberosService *authkerberos.KerberosService

	// IdentityConfig controls Kerberos principal-to-username mapping.
	// Default: strip realm ("alice@REALM" -> "alice").
	// Deprecated: use IdentityResolver for DB-backed resolution.
	IdentityConfig *kerberos.IdentityConfig

	// identityResolver resolves Kerberos principals to DittoFS users via
	// the centralized identity provider chain. When set, takes precedence
	// over IdentityConfig. When nil, falls back to IdentityConfig.
	//
	// It is held in an atomic.Pointer because the identity-provider config can
	// be hot-reloaded over the API (which rebuilds and re-injects a fresh
	// resolver via SetIdentityResolver) while session-setup goroutines read it
	// concurrently. Access only through SetIdentityResolver / IdentityResolver.
	identityResolver atomic.Pointer[pkgidentity.Resolver]

	// SMBServicePrincipal overrides the auto-derived CIFS service principal.
	// When empty, derived from the NFS principal ("nfs/host@REALM" -> "cifs/host@REALM").
	SMBServicePrincipal string

	// NetBIOSDomain is the AD NetBIOS short domain (e.g. CONTOSO) the server is
	// joined to. It is advertised in the NTLM Type-2 TargetInfo
	// (MsvAvNbDomainName), added to the NTLMv2 domain-try list, and stamped on a
	// domain user's SMB Session. When empty the server is standalone and
	// advertises/uses "WORKGROUP" exactly as before AD-4. Set by the adapter
	// from the Kerberos provider's configured domain.
	NetBIOSDomain string

	// DNSDomain is the AD DNS domain (e.g. contoso.com) advertised in the NTLM
	// Type-2 TargetInfo (MsvAvDnsDomainName). When empty the standalone default
	// ("local") is advertised. Set by the adapter from the Kerberos provider.
	DNSDomain string

	// NetBIOSName is the server's own NetBIOS computer name advertised in the
	// NTLM Type-2 TargetInfo (MsvAvNbComputerName). When NETLOGON pass-through is
	// active this MUST be the AD machine-account name (e.g. "DITTOFS"), because a
	// domain client echoes it into its NTLMv2 response and Samba's NETLOGON
	// SamLogon rejects a response whose computer name does not match the machine
	// account the secure channel authenticated as (#1357). When empty the OS
	// hostname is used (standalone / non-domain behavior). Set by the adapter
	// from the NETLOGON machine-account workstation name.
	NetBIOSName string

	// NetlogonIdmapRID enables the idmap_rid fallback for NETLOGON pass-through:
	// when a DC-validated domain user's SID is not mapped by any configured
	// identity provider, derive a stable POSIX UID/GID algorithmically from the
	// SID's RID (Samba idmap_rid model) so the user still gets a usable session
	// without RFC2307 attributes or a directory lookup (#1357). Last resort —
	// configured LDAP/local mappings always take precedence. Enabled by the
	// adapter whenever a NETLOGON authenticator is injected.
	NetlogonIdmapRID bool

	// NtlmEnabled controls whether NTLM authentication is allowed.
	// When false, NTLM tokens in SESSION_SETUP are rejected with STATUS_LOGON_FAILURE.
	// Default: true.
	NtlmEnabled bool

	// NetlogonAuth performs NETLOGON-based NTLM pass-through authentication
	// against a domain controller. When nil, NTLM falls back to local account
	// verification only. Injected from cmd/dfs via buildNetlogonAuthenticator.
	NetlogonAuth netlogon.NetlogonAuthenticator

	// GuestEnabled controls whether guest/anonymous sessions are allowed.
	// When false, guest session requests are rejected with STATUS_LOGON_FAILURE.
	// Default: true.
	GuestEnabled bool

	// DurableStore holds the durable handle persistence layer.
	// When set, durable handles are persisted on disconnect and can be
	// reconnected from a new session. Set during adapter initialization.
	// nil when durable handles are not configured (pre-SMB3 or testing).
	DurableStore lock.DurableHandleStore

	// DurableTimeoutMs is the server's configured maximum durable handle timeout.
	// Defaults to 60000 (60 seconds). Configurable via SMBAdapterSettings.
	DurableTimeoutMs uint32

	// cleanup counts in-progress session cleanups. New SESSION_SETUP
	// requests wait for it to reach zero before proceeding, ensuring
	// that stale state from a disconnected session (open files, leases,
	// change-notify watchers) is fully removed before a new session's
	// operations can observe the shared Handler maps.
	cleanup cleanupBarrier

	// resumeKeys maps opaque 24-byte resume keys to FileIDs for FSCTL_SRV_COPYCHUNK.
	// Keys are issued via FSCTL_SRV_REQUEST_RESUME_KEY and revoked on file close.
	resumeKeys *resumeKeyStore

	// handleOps tracks in-flight operations per FileID so that CLOSE can wait
	// for concurrent operations (e.g. QueryDirectory) to snapshot the OpenFile
	// before deleting it. Without this, a CLOSE goroutine can race ahead of a
	// concurrent QueryDirectory goroutine on the same connection and delete the
	// OpenFile before QueryDirectory calls GetOpenFile, causing a spurious
	// STATUS_FILE_CLOSED (smbtorture compound_find.compound_find_close). The
	// value is *handleOpTracker; see AcquireOpenFile / ReleaseOpenFile /
	// WaitAndDeleteOpenFile.
	handleOps sync.Map // string(fileID) → *handleOpTracker
}

// EncryptionConfig holds encryption policy for the handler.
// This mirrors the adapter-level EncryptionConfig but lives in the handler's
// package to avoid circular imports between handlers/ and pkg/adapter/smb/.

type EncryptionConfig struct {
	// Mode controls the encryption policy.
	// Valid values: "disabled", "preferred", "required"
	Mode string

	// AllowedCiphers is an ordered list of allowed cipher IDs.
	// The order defines server preference (first = most preferred).
	AllowedCiphers []uint16
}

// PendingAuth tracks sessions in the middle of NTLM authentication.
// It stores the server's challenge for NTLMv2 response validation
// and session key derivation. Created during Type 1 (NEGOTIATE) and
// consumed during Type 3 (AUTHENTICATE) of the NTLM handshake.

type OpenName struct {
	Path         string
	FileName     string
	ParentHandle metadata.FileHandle
}

// Name returns the current name triple, zero if the handle was never named.
// Returned by value so callers cannot mutate the published name; safe to call
// while holding the handle lock.

func (f *OpenFile) Name() OpenName {
	if n := f.name.Load(); n != nil {
		return *n
	}
	return OpenName{}
}

// SetName publishes a new name triple. Callers renaming a live handle must
// hold `mu` across the read-modify-write so two renames cannot interleave.

func (f *OpenFile) SetName(n OpenName) {
	f.name.Store(&n)
}

// WithName publishes n and returns f, so a handle can be built and named in a
// single expression.

func (f *OpenFile) WithName(n OpenName) *OpenFile {
	f.SetName(n)
	return f
}

// OpenID returns a unique identifier for this open file handle.
// This is used for per-open byte-range lock ownership per MS-SMB2.
// The identifier is derived from the SMB FileID, which is unique per open.

func (f *OpenFile) OpenID() string {
	if f.cachedOpenID == "" {
		f.cachedOpenID = fmt.Sprintf("%x", f.FileID)
	}
	return f.cachedOpenID
}

// openHasLocks reports whether any byte-range lock is currently recorded
// against the given open under the lock manager. Source of truth for the
// MS-SMB2 §3.3.7.1 ("Handling Loss of a Connection") durable persist gate
// at disconnect time — avoids the
// TOCTOU race between an async-parked LOCK goroutine's HasByteRangeLocks
// flag flip and the disconnect-time read (see lock_async.go::resumePendingLock,
// MS-SMB2 §3.3.5.14 / smb2.durable-v2-open.lock-noW-lease).
//
// Fail-closed semantics: when we cannot authoritatively confirm the open is
// lock-free — missing metadata service, missing handle, lock-manager lookup
// failure, or a stale optimistic flag — we MUST NOT permit durable
// persistence. The caller treats true as "do not persist". Returning true
// on any uncertainty preserves the lock-noW-lease gate at the cost of
// occasionally declining to persist a genuinely lock-free handle whose
// lock-manager is transiently unreachable.

func openHasLocks(metaSvc *metadata.Service, openFile *OpenFile) bool {
	if openFile == nil {
		// No open to gate; nothing to persist. Caller short-circuits.
		return false
	}
	// Optimistic flag is consulted first as an inexpensive positive signal.
	// A true here is authoritative ("a lock was recorded at some point").
	// A false alone is NOT authoritative — the flag can lag a concurrent
	// LOCK completion (see lock_async.go::resumePendingLock).
	if openFile.HasByteRangeLocks.Load() {
		return true
	}
	if metaSvc == nil || len(openFile.MetadataHandle) == 0 {
		// Cannot consult the lock manager — fail closed.
		return true
	}
	lm, err := metaSvc.GetLockManagerForHandle(openFile.MetadataHandle)
	if err != nil || lm == nil {
		logger.Debug("openHasLocks: lock manager lookup failed, failing closed",
			"error", err)
		return true
	}
	openID := openFile.OpenID()
	for _, fl := range lm.ListLocks(string(openFile.MetadataHandle)) {
		if fl.OpenID == openID {
			return true
		}
	}
	return false
}

// Lock / Unlock / RLock / RUnlock expose `mu` for handlers that need to hold
// the OpenFile lock across a longer R-M-W critical section (e.g. QueryDirectory
// cursor advancement, SET_INFO BasicInfo freeze/thaw bookkeeping). For simple
// boolean reads prefer IsAtimeFrozen / SnapshotFreeze.

func (f *OpenFile) Lock() { f.mu.Lock() }

func (f *OpenFile) Unlock() { f.mu.Unlock() }

func (f *OpenFile) RLock() { f.mu.RLock() }

func (f *OpenFile) RUnlock() { f.mu.RUnlock() }

// IsAtimeFrozen returns the AtimeFrozen flag under the read lock. Used by
// READ / WRITE / QUERY_DIRECTORY / COPYCHUNK to decide whether to bump
// LastAccessTime after a successful operation.

func (f *OpenFile) IsAtimeFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.AtimeFrozen
}

// GetPayloadID returns the cached payload identifier under the read lock.
// WRITE, SET_REPARSE_POINT and COPYCHUNK publish it on a live handle, so
// readers on other channels must not touch the field directly.

func (f *OpenFile) GetPayloadID() metadata.PayloadID {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.PayloadID
}

// SetPayloadID publishes a new cached payload identifier under the write lock.

func (f *OpenFile) SetPayloadID(id metadata.PayloadID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PayloadID = id
}

// IsDeletePending returns the committed delete-on-close flag under the read
// lock. SET_INFO and the CLOSE delete-on-close election write it under the
// write lock, from goroutines other than the handle's own, so every read
// outside those critical sections goes through here.

func (f *OpenFile) IsDeletePending() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.DeletePending
}

// IsMtimeFrozen returns the MtimeFrozen flag under the read lock.

func (f *OpenFile) IsMtimeFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.MtimeFrozen
}

// IsCtimeFrozen returns the CtimeFrozen flag under the read lock.

func (f *OpenFile) IsCtimeFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.CtimeFrozen
}

// notifyMaxBufferSizeSetBit marks the NotifyMaxBufferSize uint64 as having
// been initialized by the first CHANGE_NOTIFY on the handle. The low 32 bits
// of the same uint64 hold the captured OutputBufferLength (which may legally
// be zero — see the field comment for the encoding rationale).

const notifyMaxBufferSizeSetBit uint64 = 1 << 32

// CaptureNotifyMaxBufferSize atomically records the OutputBufferLength of the
// first CHANGE_NOTIFY on this handle. Returns the captured value (low 32 bits)
// and true if this call performed the capture, or the previously-captured
// value and false if a prior CHANGE_NOTIFY already set it. Safe for
// concurrent callers; only the first wins.

func (f *OpenFile) CaptureNotifyMaxBufferSize(outputBufferLength uint32) (captured uint32, didCapture bool) {
	packed := notifyMaxBufferSizeSetBit | uint64(outputBufferLength)
	if f.NotifyMaxBufferSize.CompareAndSwap(0, packed) {
		return outputBufferLength, true
	}
	return uint32(f.NotifyMaxBufferSize.Load()), false
}

// NotifyMaxBufferSizeValue returns the captured first-CHANGE_NOTIFY
// OutputBufferLength and whether it has been set yet. Returns (0, false)
// before the first CHANGE_NOTIFY on this handle.

func (f *OpenFile) NotifyMaxBufferSizeValue() (value uint32, set bool) {
	raw := f.NotifyMaxBufferSize.Load()
	if raw&notifyMaxBufferSizeSetBit == 0 {
		return 0, false
	}
	return uint32(raw), true
}

// CaptureNotifyCompletionFilter atomically records the CompletionFilter of
// the first CHANGE_NOTIFY on this handle. Subsequent calls return the stored
// value. Thread-safe; only the first caller wins.
//
// An empty filter is never captured. It carries no mask to make sticky, and
// storing it would arm the handle with a filter that matches nothing for as
// long as the handle lives — including for the later requests that do name one.

func (f *OpenFile) CaptureNotifyCompletionFilter(filter uint32) (captured uint32, didCapture bool) {
	if filter == 0 {
		return uint32(f.NotifyCompletionFilter.Load()), false
	}
	packed := notifyMaxBufferSizeSetBit | uint64(filter)
	if f.NotifyCompletionFilter.CompareAndSwap(0, packed) {
		return filter, true
	}
	return uint32(f.NotifyCompletionFilter.Load()), false
}

// NewHandler creates a new SMB2 handler with a default session manager.
// It initializes the pipe manager, notify registry, and generates a random
// server GUID. For custom session management (e.g., shared across adapters),
// use NewHandlerWithSessionManager. LeaseManager is wired by the adapter
// layer when the runtime is available.

func NewHandler() *Handler {
	return NewHandlerWithSessionManager(session.NewDefaultManager())
}

// NewHandlerWithSessionManager creates a new SMB2 handler with an external session manager.
// This allows sharing the session manager with other components (e.g., the Adapter
// for credit tracking). Initializes pipe manager, notify registry, generates a
// random server GUID, and sets default max sizes. LeaseManager is wired by the
// adapter layer when the runtime and LockManager are available.
// The NETLOGON authenticator is injected separately via SetNetlogonAuthenticator
// after construction (see pkg/adapter/smb/adapter.go createSMBAdapter).

func NewHandlerWithSessionManager(sessionManager *session.Manager) *Handler {
	h := &Handler{
		StartTime:               time.Now(),
		SessionManager:          sessionManager,
		PipeManager:             rpc.NewPipeManager(),
		NotifyRegistry:          NewNotifyRegistry(),
		PipeReadRegistry:        NewPipeReadRegistry(),
		PendingCreateRegistry:   NewPendingCreateRegistry(),
		PendingLockRegistry:     NewPendingLockRegistry(),
		CreateReplayCache:       NewCreateReplayCache(),
		LockReplayCache:         NewLockReplayCache(),
		LockWaitGraph:           lock.NewWaitForGraph(),
		MaxTransactSize:         1048576, // 1MB (supports large directory listings; increases per-request memory)
		MaxReadSize:             1048576, // 1MB
		MaxWriteSize:            1048576, // 1MB
		SigningConfig:           signing.DefaultSigningConfig(),
		MinDialect:              types.Dialect0202,
		MaxDialect:              types.Dialect0210, // Default to 2.1 until full SMB3 session/signing is implemented
		EncryptionEnabled:       false,
		DirectoryLeasingEnabled: true,
		NtlmEnabled:             true,
		GuestEnabled:            true,

		// Default durable handle timeout: 300s (5 minutes). Matches Samba's
		// `durable_default_timeout_msec` (source3/smbd/smb2_create.c).
		// smbtorture asserts this value when the client requests
		// UINT32_MAX (clamp to server max): smb2.durable-v2-open.create-blob,
		// reopen1, reopen1a, reopen1a-lease, reopen2, app-instance, … all
		// CHECK_VAL(io.out.timeout, 300*1000).
		DurableTimeoutMs: 300000,
		resumeKeys:       newResumeKeyStore(),
	}

	// Generate random server GUID
	_, _ = rand.Read(h.ServerGUID[:])

	// Start tree/file IDs at 1 (0 is reserved)
	h.nextTreeID.Store(1)
	h.nextFileID.Store(1)

	return h
}

// sessionDomain returns the domain to stamp on an authenticated SMB session.
// When the server is domain-joined (NetBIOSDomain configured) it returns the
// AD NetBIOS short domain (e.g. CONTOSO) so the session reflects the server's
// domain rather than the realm or whatever the client supplied. When standalone
// (NetBIOSDomain empty) it returns the caller-provided fallback (the Kerberos
// realm or the client-supplied NTLM domain), preserving pre-AD-4 behavior.

func (h *Handler) sessionDomain(fallback string) string {
	if h.NetBIOSDomain != "" {
		return h.NetBIOSDomain
	}
	return fallback
}

// SetIdentityResolver atomically installs the centralized identity resolver.
// Safe to call while session-setup goroutines read it (used for API-driven
// identity-provider config hot-reload). Pass nil to clear.

func (h *Handler) SetIdentityResolver(r *pkgidentity.Resolver) {
	h.identityResolver.Store(r)
}

// IdentityResolver returns the current centralized identity resolver, or nil
// when none is installed.

func (h *Handler) IdentityResolver() *pkgidentity.Resolver {
	return h.identityResolver.Load()
}

// GetSession retrieves a session by ID.
// Delegates to SessionManager for unified session/credit management.

func (h *Handler) GenerateSessionID() uint64 {
	return h.SessionManager.GenerateSessionID()
}

// GenerateTreeID generates a new unique tree ID

func (h *Handler) GenerateTreeID() uint32 {
	return h.nextTreeID.Add(1)
}

// generateAsyncId generates a new unique async ID for CHANGE_NOTIFY interim responses.
// AsyncIds must be unique within a connection and non-zero.

func (h *Handler) generateAsyncId() uint64 {
	return h.nextAsyncId.Add(1)
}

// notifyOpenFileModified emits a FileActionModified notification for the
// handle, taking the parent path and stream name from one name snapshot.

func (h *Handler) GenerateFileID() [16]byte {
	var fileID [16]byte
	// Use persistent part for the ID counter
	id := h.nextFileID.Add(1)
	fileID[0] = byte(id)
	fileID[1] = byte(id >> 8)
	fileID[2] = byte(id >> 16)
	fileID[3] = byte(id >> 24)
	fileID[4] = byte(id >> 32)
	fileID[5] = byte(id >> 40)
	fileID[6] = byte(id >> 48)
	fileID[7] = byte(id >> 56)
	// Use volatile part for random data
	_, _ = rand.Read(fileID[8:16])
	return fileID
}

// CreateSession creates and stores a new session.
// This replaces the old StoreSession method for unified session/credit management.

type pendingAuthKey struct {
	SessionID uint64
	ConnID    uint64
}

// StorePendingAuth stores a pending authentication. pending.SessionID and
// pending.ConnID together form the lookup key.

func adsBaseName(fileName string) string {
	colonIdx := strings.Index(fileName, ":")
	if colonIdx <= 0 {
		return ""
	}
	return fileName[:colonIdx]
}

// checkShareDeleteConflict checks if any other open handle on the same file
// lacks FILE_SHARE_DELETE in its ShareAccess. MS-FSA 2.1.5.15.12
// ("FileRenameInformation") states no share-mode check; requiring all other
// opens to permit delete sharing follows Samba `can_rename`. Returns true if a conflict
// exists (rename should be blocked with STATUS_SHARING_VIOLATION).

func logRenameConflictHolder(gate string, renamer, holder *OpenFile) {
	logger.Debug("SET_INFO rename conflict holder",
		"gate", gate,
		"renamerFileID", fmt.Sprintf("%x", renamer.FileID),
		"renamerPath", renamer.Name().Path,
		"renamerSession", renamer.SessionID,
		"renamerTree", renamer.TreeID,
		"holderFileID", fmt.Sprintf("%x", holder.FileID),
		"holderPath", holder.Name().Path,
		"holderShare", holder.ShareName,
		"holderSession", holder.SessionID,
		"holderTree", holder.TreeID,
		"holderShareAccess", fmt.Sprintf("0x%x", holder.ShareAccess),
		"holderDesiredAccess", fmt.Sprintf("0x%x", holder.DesiredAccess),
		"holderIsDurable", holder.IsDurable,
		"holderDeletePending", holder.IsDeletePending())
}

// checkParentDirRenameConflict applies the destination-parent share-mode rule
// from MS-FSA 2.1.5.15.12 ("FileRenameInformation"): the rename opens the destination directory
// with DesiredAccess FILE_ADD_FILE|SYNCHRONIZE and ShareAccess
// FILE_SHARE_READ|FILE_SHARE_WRITE. Linking a new name into a directory is
// therefore a WRITE against that directory, not a delete of it, so an existing
// open conflicts only when it denies write sharing, or already holds DELETE
// access — which the rename's withheld share-delete is incompatible with. A
// holder that merely lacks FILE_SHARE_DELETE does not conflict; nothing in the
// rename asks to delete the destination parent.
//
// Only the renamer's own handle is excluded, by FileID. Another open on the
// renamer's own session still counts, because the implicit open is a fresh
// open evaluated against the whole open list.
//
// Caller passes the destination parent handle (same as source parent for a
// same-directory rename). Returns true on conflict.

func hasReadAccess(access uint32) bool {
	m := types.AccessMask(access)
	return m&types.FileReadData != 0 ||
		m&types.FileExecute != 0 ||
		m&types.GenericRead != 0 ||
		m&types.GenericAll != 0 ||
		m&types.MaximumAllowed != 0
}

// hasWriteAccess reports whether the given access mask includes write access.
// Checks FILE_WRITE_DATA, FILE_APPEND_DATA, GENERIC_WRITE, GENERIC_ALL, and MAXIMUM_ALLOWED.

func hasWriteAccess(access uint32) bool {
	m := types.AccessMask(access)
	return m&types.FileWriteData != 0 ||
		m&types.FileAppendData != 0 ||
		m&types.GenericWrite != 0 ||
		m&types.GenericAll != 0 ||
		m&types.MaximumAllowed != 0
}

// hasDeleteAccess reports whether the given access mask includes delete access.
// Checks DELETE, GENERIC_ALL, and MAXIMUM_ALLOWED.

func hasDeleteAccess(access uint32) bool {
	m := types.AccessMask(access)
	return m&types.Delete != 0 ||
		m&types.GenericAll != 0 ||
		m&types.MaximumAllowed != 0
}

// newOpenIsShareRestrictive reports whether an open, by virtue of its OWN
// access + share masks alone, would deny a co-located data-bearing holder —
// i.e. it requests a given data access but does NOT share that mode with
// others. FILE_SHARE_NONE + any data access is the maximally restrictive case.
//
// Unlike checkShareModeConflict this needs no second party: it is the
// holder-independent half of the share-mode test (the "new opener's share mask
// denies others" direction). It is stable across the brief window where a
// holder's OpenFile has been torn down but its lease record lingers, which is
// exactly why the #1331 break-reason reclassification keys on it rather than on
// the racy live-open scan. Stat-only opens impose no share constraint.

func newOpenIsShareRestrictive(desiredAccess, shareAccess uint32) bool {
	if isStatOnlyOpen(desiredAccess) {
		return false
	}
	if hasReadAccess(desiredAccess) && shareAccess&smbShareRead == 0 {
		return true
	}
	if hasWriteAccess(desiredAccess) && shareAccess&smbShareWrite == 0 {
		return true
	}
	if hasDeleteAccess(desiredAccess) && shareAccess&smbShareDelete == 0 {
		return true
	}
	return false
}

// getCachedShares returns the cached share list, rebuilding if invalidated.
// Thread-safe via RWMutex (concurrent reads allowed, exclusive write for rebuild).
