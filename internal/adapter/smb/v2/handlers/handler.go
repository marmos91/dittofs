package handlers

import (
	"bytes"
	"context"
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
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	pkgidentity "github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Handler manages SMB2 protocol handling including session management,
// tree connections, open file state, oplocks, leases, and named pipe RPC.
// It delegates to the Runtime registry for metadata and payload operations,
// and uses SessionManager for unified session/credit tracking.
// Thread-safe: all mutable state uses sync.Map or atomic operations.
type Handler struct {
	Registry  *runtime.Runtime
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

	// Pending blocking lock operations (messageID -> cancel func). Legacy
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
	// the scavenger evicts it. The mutex is coarse-grained (process-wide) but
	// covers only durable-store IO and the disconnect-time persist step,
	// neither of which is on the steady-state hot path.
	durablePurgeMu sync.Mutex

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

	// IdentityResolver resolves Kerberos principals to DittoFS users via
	// the centralized identity provider chain. When set, takes precedence
	// over IdentityConfig. When nil, falls back to IdentityConfig.
	IdentityResolver *pkgidentity.Resolver

	// SMBServicePrincipal overrides the auto-derived CIFS service principal.
	// When empty, derived from the NFS principal ("nfs/host@REALM" -> "cifs/host@REALM").
	SMBServicePrincipal string

	// NtlmEnabled controls whether NTLM authentication is allowed.
	// When false, NTLM tokens in SESSION_SETUP are rejected with STATUS_LOGON_FAILURE.
	// Default: true.
	NtlmEnabled bool

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

	// cleanupWg tracks in-progress session cleanups. New SESSION_SETUP
	// requests wait for this to reach zero before proceeding, ensuring
	// that stale state from a disconnected session (open files, leases,
	// change-notify watchers) is fully removed before a new session's
	// operations can observe the shared Handler maps.
	cleanupWg sync.WaitGroup

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
type PendingAuth struct {
	SessionID       uint64
	ClientAddr      string
	CreatedAt       time.Time
	ServerChallenge [8]byte // Random challenge sent in Type 2 message
	UsedSPNEGO      bool    // Whether client used SPNEGO wrapping
	IsReauth        bool    // True when re-authenticating an existing session
	// IsBinding is true when this pending auth is driving an SMB2 session
	// bind (SESSION_SETUP with SMB2_SESSION_FLAG_BINDING). In that case
	// BindingSessionID holds the existing session the client is binding to
	// and auth completion must register the connection as an additional
	// channel rather than creating a new session. MS-SMB2 §3.3.5.5.2.
	IsBinding        bool
	BindingSessionID uint64
	// ConnID is the TCP connection carrying this authentication. Pending auth
	// is keyed by (SessionID, ConnID) so concurrent binds on the same session
	// from different connections (MS-SMB2 §3.3.5.5.2) do not collide.
	ConnID uint64
	// MechListBytes: DER-encoded SEQUENCE OF OID from the NegTokenInit's
	// mechTypes field, needed to compute the SPNEGO mechListMIC in the
	// final accept-completed response (MS-NLMP 3.4.5.2 + 2.2.2.9.1).
	// Nil for clients that send raw NTLM without SPNEGO wrapping.
	MechListBytes []byte
}

// TreeConnection represents an active tree connection mapping a client
// to a DittoFS share. Created by TreeConnect and removed by TreeDisconnect.
// Stores the effective permission level for access control during file operations.
type TreeConnection struct {
	TreeID      uint32
	SessionID   uint64
	ShareName   string
	ShareType   uint8
	CreatedAt   time.Time
	Permission  models.SharePermission // User's permission level for this share
	EncryptData bool                   // Share requires all requests to be encrypted
	// AccessBasedEnumeration mirrors the share-level toggle. When true,
	// QUERY_DIRECTORY filters entries the caller cannot read (refs #532,
	// MS-SMB2 §2.2.10 SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM).
	AccessBasedEnumeration bool
	// ChangeNotifyDisabled mirrors the share-level toggle. When true,
	// CHANGE_NOTIFY requests on this tree are rejected with
	// STATUS_NOT_IMPLEMENTED — matches Samba `kernel change notify = no`
	// and the smb2.change_notify_disabled torture test.
	ChangeNotifyDisabled bool
	// StreamsDisabled mirrors the share-level toggle. When true, CREATE
	// requests that reference an Alternate Data Stream are rejected with
	// STATUS_OBJECT_NAME_INVALID — matches Samba `smbd:streams = no`
	// and the smb2.create_no_streams.no_stream torture test.
	StreamsDisabled bool
}

// OpenFile represents an open file handle created by the CREATE command.
// It links the SMB2 FileID to the underlying metadata handle and payload ID,
// tracks directory enumeration state, delete-on-close flags, and oplock level.
// Stored in a sync.Map keyed by the 16-byte FileID.
//
// Concurrency: SMB clients legitimately pipeline operations on the same handle
// (e.g. WRITE + QUERY_INFO; multi-channel sessions can also dispatch concurrent
// QUERY_DIRECTORY on the same FileID). The exported mutable fields below are
// guarded by `mu` — read-locked when surfacing state to the wire (QUERY_INFO,
// override application) and write-locked when mutating (enumeration cursor,
// freeze/thaw, delayed-write arm/flush). Hold the lock around the full
// read-modify-write region; release before any I/O to the metadata store to
// keep the critical section bounded. Atomic-typed fields
// (NotifyOverflowed/NotifyMaxBufferSize/NotifyCompletionFilter) and immutable
// fields (FileID/TreeID/SessionID/Path/MetadataHandle/PayloadID/CreateOptions)
// are safe to access without the mutex.
type OpenFile struct {
	// mu guards the mutable fields listed in the struct comment above. Held
	// across QueryDirectory enumeration R-M-W, freeze/thaw bookkeeping in
	// SET_INFO BasicInfo, the SMB delayed-write timestamp helpers and the
	// QUERY_INFO frozen/delayed-write overlay reads.
	mu sync.RWMutex

	FileID        [16]byte
	TreeID        uint32
	SessionID     uint64
	Path          string
	ShareName     string
	cachedOpenID  string // cached hex(FileID) for hot-path lock operations
	OpenTime      time.Time
	DesiredAccess uint32
	// GrantedAccess is the effective access mask the open actually holds,
	// computed at CREATE as the per-bit intersection of the requested mask
	// with the file's DACL (via metadata.CheckFileAccess). Per MS-SMB2
	// §3.3.5.9 paragraph 8 / §2.2.13.1, when MAXIMUM_ALLOWED is requested
	// this is the set of rights the requester is allowed; for explicit
	// requests it is the requested set (the open would have been rejected
	// if any non-MAXIMUM_ALLOWED bit was denied). Per MS-SMB2 §3.3.5.20.1
	// and MS-FSCC §2.4.1, FileAccessInformation and QUERY_INFO open-level
	// access gates consult this field, not DesiredAccess (smb2.acls.GENERIC
	// at acls.c:440).
	GrantedAccess       uint32
	IsDirectory         bool
	IsPipe              bool   // True if this is a named pipe (IPC$)
	PipeName            string // Named pipe name (e.g., "srvsvc")
	EnumerationComplete bool   // For directories: true if directory listing was returned

	// Store integration fields
	MetadataHandle metadata.FileHandle // Link to metadata store file handle
	PayloadID      metadata.PayloadID  // Content identifier for read/write operations

	// Directory enumeration state
	EnumerationCookie  []byte // Opaque cookie for resuming directory listing
	EnumerationIndex   int    // Current index in directory listing
	EnumerationPattern string // Last search pattern used (for detecting pattern changes)

	// EnumerationLastName is the case-folded name of the last directory entry
	// returned to the client on this handle. Subsequent QUERY_DIRECTORY calls
	// in the same enumeration sequence re-read the directory fresh and skip
	// entries with name <= EnumerationLastName (case-insensitive). This is
	// Samba's name-based cursor model (source3/smbd/dir.c) and is required
	// for smb2.dir.fixed (#728): when one handle deletes files mid-enumeration
	// on another, the second handle must see live state (deletions hidden,
	// new files added) without skipping or duplicating entries.
	//
	// EnumerationLastName == "" means "before any entry"; the first call of a
	// fresh enumeration returns "." / ".." for a wildcard search and then
	// data entries from the start. Cleared on RESTART_SCANS, REOPEN, pattern
	// change and EnumerationComplete.
	//
	// EnumerationSpecialDone tracks whether the "." and ".." entries have been
	// returned in this sequence. Without it, deletion of the first real entry
	// between calls could resurface "." on the next call (LastName="" but
	// special done).
	EnumerationLastName    string
	EnumerationSpecialDone int // count of special entries already returned (0..2)

	// Delete on close support (FileDispositionInformation).
	//
	// DeletePending tracks the SHARED, committed delete-on-close state per
	// MS-FSA 2.1.5.1.2.1 and Samba `is_delete_on_close_set` (locking.tdb).
	// It is set ONLY by:
	//   - SET_INFO FileDispositionInformation with DeleteFile=TRUE (an
	//     explicit commit by an opener), or
	//   - CLOSE-time promotion of InitialDeleteOnClose on the last handle
	//     when nobody else has committed a shared DOC yet (matches Samba
	//     close.c::close_normal_file: initial_delete_on_close
	//     && !is_delete_on_close_set => set_delete_on_close_lck).
	// Subsequent CREATEs see DeletePending and return STATUS_DELETE_PENDING
	// per MS-SMB2 3.3.5.9 — the gate consumed by isFileDeletePending and
	// isFileOrBaseDeletePending.
	//
	// InitialDeleteOnClose tracks the PER-HANDLE initial DOC flag from a
	// CREATE with FILE_DELETE_ON_CLOSE (Samba `fsp_flags.initial_delete_on_close`).
	// It is NOT visible to other handles via isFileDeletePending and does
	// NOT block subsequent opens — those still succeed and observe the
	// existing share-mode rules until the DOC is actually committed at
	// CLOSE time. Required by smbtorture smb2.dirlease.{unlink_same,
	// unlink_different}_initial_and_close which open a file with initial
	// DOC and then immediately open a SECOND handle to it (must succeed).
	DeletePending        bool                // committed shared DOC (visible to other opens)
	InitialDeleteOnClose bool                // per-handle initial DOC from CREATE FILE_DELETE_ON_CLOSE
	ParentHandle         metadata.FileHandle // Parent directory handle for deletion
	FileName             string              // File name within parent for deletion

	// ShareAccess stores the sharing mode from the CREATE request.
	// Used for share mode conflict checking during rename and other operations.
	// Bit mask: 0x01 (FILE_SHARE_READ), 0x02 (FILE_SHARE_WRITE), 0x04 (FILE_SHARE_DELETE)
	ShareAccess uint32

	// CreateOptions stores the original CreateOptions from the CREATE request,
	// used to populate FileModeInformation (FILE_WRITE_THROUGH, FILE_SEQUENTIAL_ONLY, etc.)
	CreateOptions types.CreateOptions

	// RequestedAllocSize is the client-requested initial allocation in bytes
	// from the CREATE SMB2_CREATE_ALLOCATION_SIZE ("AlSi") create context
	// [MS-SMB2] 2.2.13.2.2, or from a later SET_INFO FileAllocationInformation
	// [MS-FSCC] 2.4.4. DittoFS does not preallocate backing storage; this value
	// only raises the (cluster-aligned) AllocationSize reported in the CREATE
	// response and subsequent QUERY_INFO on this handle, keeping the two
	// consistent (smb2.create.open, smb2.durable-open.alloc-size). Always 0 for
	// directories — directories never honour the request
	// (smb2.create.dir-alloc-size). Per-handle, in-memory, lost on close.
	RequestedAllocSize uint64

	// Timestamp freeze/unfreeze state per MS-FSA 2.1.5.14.2.
	// When a client sends SET_INFO with FILETIME -1, the corresponding timestamp
	// is "frozen" and MUST NOT be auto-updated by subsequent operations (WRITE, etc.).
	// When a client sends SET_INFO with FILETIME -2, the freeze is lifted.
	// These flags are per-open-handle state and are lost on server restart,
	// which is correct per the spec (frozen state is tied to the open handle).
	BtimeFrozen bool       // CreationTime frozen (suppress explicit changes on this handle)
	MtimeFrozen bool       // LastWriteTime frozen (don't auto-update on WRITE)
	CtimeFrozen bool       // ChangeTime frozen (don't auto-update on WRITE)
	AtimeFrozen bool       // LastAccessTime frozen (don't auto-update on READ)
	FrozenBtime *time.Time // Saved CreationTime value at freeze time
	FrozenMtime *time.Time // Saved Mtime value at freeze time
	FrozenCtime *time.Time // Saved Ctime value at freeze time
	FrozenAtime *time.Time // Saved Atime value at freeze time

	// SMB delayed-write timestamp semantics, mirroring Samba
	// `source3/smbd/fileio.c::trigger_write_time_update` (2-second delay
	// before a write becomes visible via QUERY_INFO, then sticky for the
	// rest of the open) and `write_time_forced` (an explicit SetBasic
	// write_time pins the value until close).
	SmbWriteTriggered  bool       // first WRITE on this handle has occurred
	SmbWritePreMtime   *time.Time // Mtime captured before first WRITE — visible during the 2s window
	SmbWriteFlushMtime *time.Time // Mtime to surface once the 2s window expires or a flush trigger fires
	SmbWriteFlushAt    time.Time  // wall-clock when the 2s window expires (zero ⇒ already flushed)
	SmbStickyWriteTime *time.Time // explicit SetBasic write_time — wins over any pending update

	// Oplock state
	// OplockLevel is the current oplock level for this handle.
	// Thread safety: This field is written during CREATE (before storing in sync.Map)
	// and during OPLOCK_BREAK (for a specific FileID). Since file handles are session-
	// specific and OPLOCK_BREAK targets a specific FileID, concurrent access is not
	// expected. If this changes, consider using atomic operations.
	OplockLevel uint8

	// LeaseKey is the 128-bit lease key for this handle (when OplockLevel == OplockLevelLease).
	// Used to release the lease when the last handle sharing the key is closed.
	LeaseKey [16]byte

	// ParentLeaseKey is the 128-bit parent directory lease key carried in the
	// CREATE RqLs (V2) when the client set SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET.
	// Used by the dir-lease parent-key suppression rule (MS-SMB2 §3.3.4.20):
	// SET_INFO / WRITE / CLOSE-on-delete on this handle MUST NOT break the
	// parent dir lease whose LeaseKey matches this value. The field is
	// meaningful only when HasParentLeaseKey is true (#470 C2).
	ParentLeaseKey    [16]byte
	HasParentLeaseKey bool

	// DeleteOnCloseParentKey tracks the ParentLeaseKey of the handle that
	// originally set delete-on-close (via SET_INFO or CREATE option).
	// When the last handle closes and triggers the actual deletion, the
	// closer's ParentLeaseKey is compared to this: if they match, parent-key
	// suppression applies (test_unlink_same_*); if they differ, ALL parent
	// dir leases are broken without suppression (test_unlink_different_*).
	// HasDeleteOnCloseParentKey is true when the value is meaningful.
	DeleteOnCloseParentKey    [16]byte
	HasDeleteOnCloseParentKey bool

	// BaseFileDeletePending is set on a stream handle when the base file was
	// unlinked while this stream was still open. Per MS-FSA 2.1.5.4, the
	// actual base-file removal is deferred until all handles (including
	// stream handles) are closed. When the last such handle closes, the
	// CLOSE handler uses BaseFileDeleteParentHandle / BaseFileDeleteFileName
	// to perform the base file deletion.
	BaseFileDeletePending      bool
	BaseFileDeleteParentHandle metadata.FileHandle
	BaseFileDeleteFileName     string

	// Durable handle state (SMB3 durable handles)
	// IsDurable indicates this handle has been granted durability.
	// When true, the handle will be persisted to DurableHandleStore on disconnect
	// instead of being closed immediately.
	IsDurable bool

	// CreateGuid is the V2 client-generated GUID for idempotent reconnection.
	// Zero value for V1 durable handles or non-durable handles.
	CreateGuid [16]byte

	// AppInstanceId is the application instance ID for Hyper-V failover.
	// Zero value if not set.
	AppInstanceId [16]byte

	// DurableTimeoutMs is the granted durable handle timeout in milliseconds.
	// The handle expires this many milliseconds after client disconnects.
	DurableTimeoutMs uint32

	// ClientGUID is the SMB2 NEGOTIATE ClientGuid of the connection that
	// established this open. Captured at CREATE time so it can be persisted
	// with the durable handle and matched against the reconnecting
	// connection on V2 lease reconnect (smbtorture
	// smb2.durable-v2-open.reopen1a-lease — reconnect with a different
	// ClientGuid fails OBJECT_NAME_NOT_FOUND, reconnect with the original
	// ClientGuid succeeds). Non-lease V2 reconnect (reopen1a/reopen2/...)
	// does NOT consult this — those tests reconnect with a fresh ClientGuid.
	ClientGUID [16]byte

	// csMu guards the SMB3 channel-sequence tracking fields below. It is a
	// dedicated lock (not the struct mu) so the verification step in the
	// dispatch hot path never contends with QUERY_INFO/enumeration R-M-W.
	csMu sync.Mutex

	// channelSeq is the ChannelSequence number the server currently tracks
	// for this Open (MS-SMB2 §3.3.5.2.10 Open.ChannelSequence). Advanced when
	// a request arrives with a strictly newer ChannelSequence (a channel
	// failover), used to reject stale modifying replays.
	channelSeq uint16

	// channelSeqSet records whether channelSeq has been initialized from a
	// request yet. The first request on the Open seeds channelSeq with its
	// own ChannelSequence so an initial nonzero CSN is not mistaken for a
	// failover.
	channelSeqSet bool

	// PositionInfo is the FILE_POSITION_INFORMATION CurrentByteOffset
	// (MS-FSCC 2.4.32). Servers track this per-handle so SET/GET via
	// FilePositionInformation round-trips even though network filesystems
	// do not use it for I/O dispatch. Preserved across durable handle
	// disconnect/reconnect (smb2.durable-open.file-position).
	PositionInfo uint64

	// NotifyOverflowed is the sticky overflow flag for SMB2 CHANGE_NOTIFY on
	// this directory handle. Set when a notify completes with
	// STATUS_NOTIFY_ENUM_DIR because the encoded change list exceeds the
	// requested OutputBufferLength. The next CHANGE_NOTIFY on this handle
	// MUST also return STATUS_NOTIFY_ENUM_DIR regardless of the new buffer
	// size — once events are lost the directory state is considered
	// inconsistent and the client must re-enumerate (Samba notify_buffer
	// is_overflow semantics; smb2.notify.valid-req "if the first notify
	// returns NOTIFY_ENUM_DIR, all do"). Cleared after that next notify
	// consumes it. Lifetime is the handle: closing/reopening resets it.
	NotifyOverflowed atomic.Bool

	// NotifyMaxBufferSize is the OutputBufferLength captured from the FIRST
	// CHANGE_NOTIFY issued on this handle. Subsequent notifies cap their
	// effective max with MIN(req.OutputBufferLength, NotifyMaxBufferSize),
	// matching Samba `change_notify_create` / `change_notify_reply` semantics
	// (max_buffer_size is stored on notify_buffer creation and applied to
	// every reply via MIN). This is what gives the smb2.notify.valid-req
	// "if the first notify returns NOTIFY_ENUM_DIR, all do" property: a
	// tiny first buffer permanently caps later notifies on the same handle.
	//
	// Encoding: SMB2 OutputBufferLength is uint32 and 0 is a valid request
	// value (a peer may issue CHANGE_NOTIFY with OutputBufferLength=0), so
	// we cannot use 0 as the "unset" sentinel. Instead we pack into a
	// uint64: bit `notifyMaxBufferSizeSetBit` (1<<32) is set on the first
	// capture, and the low 32 bits hold the captured OutputBufferLength.
	// "Unset" is the all-zero value. Set once via CompareAndSwap(0, ...)
	// and never updated after. Use `notifyMaxBufferSizeLoad` to decode.
	NotifyMaxBufferSize atomic.Uint64

	// NotifyCompletionFilter is the CompletionFilter captured from the FIRST
	// CHANGE_NOTIFY on this handle. Subsequent requests use this stored filter
	// regardless of the filter in their request, matching Samba's
	// change_notify_create behavior where the notify buffer's filter is fixed
	// at creation. The recursive (WatchTree) flag is NOT sticky — it comes
	// from each request. Encoding mirrors NotifyMaxBufferSize: bit 32 = set,
	// low 32 bits = filter value. Zero means unset.
	NotifyCompletionFilter atomic.Uint64

	// HasByteRangeLocks is set the first time a LOCK request successfully
	// records at least one byte-range lock under this open. The flag is
	// strictly monotonic for the lifetime of the open — UNLOCK does NOT
	// clear it, mirroring the pessimistic check Samba performs in
	// `vfs_default_durable_disconnect`. The flag participates in the
	// disconnect-time decision to persist a durable handle (see
	// shouldPersistDurableOnDisconnect): an open holding any BR-lock under a
	// lease that lacks W must NOT be persisted, because its locks cannot
	// reliably survive an in-flight lease downgrade.
	// smbtorture smb2.durable-v2-open.lock-noW-lease.
	HasByteRangeLocks atomic.Bool

	// OpenerUser is a snapshot of the SMB session's authenticated DittoFS
	// user at CREATE time. After SESSION_SETUP re-authentication mutates
	// Session.User to a different principal, handle-bound operations on
	// this open (notably SET_INFO SecurityDescriptor) MUST be authorized
	// against the ORIGINAL opener — MS-SMB2 §3.3.5.5.3 freezes the open's
	// SecurityContext to the user who opened it. Re-resolving from the
	// session at op time would (a) trip the ownership gate in
	// MetadataService.SetFileAttributes when U1's file is being touched
	// via h1 while the session is currently re-authed to anon/U2, and
	// (b) misattribute authz audit records to the wrong principal.
	//
	// nil means "use the session-current user" — the legacy behaviour
	// for codepaths and tests that pre-date the snapshot. Guest/Null
	// opens set OpenerIsGuest / OpenerIsNull so handle-bound ops can
	// rebuild the same nobody/65534 identity even after the session
	// re-authenticates to a real user. smbtorture smb2.session.reauth4
	// (set_secdesc on a U1-opened handle while session is anon) and
	// reauth5 (same shape via the dir-handle dh1 SET_INFO) gate on this.
	OpenerUser    *models.User
	OpenerIsGuest bool
	OpenerIsNull  bool
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
// MS-SMB2 §3.3.4.18 durable persist gate at disconnect time — avoids the
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
func openHasLocks(metaSvc *metadata.MetadataService, openFile *OpenFile) bool {
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
func (f *OpenFile) Lock()    { f.mu.Lock() }
func (f *OpenFile) Unlock()  { f.mu.Unlock() }
func (f *OpenFile) RLock()   { f.mu.RLock() }
func (f *OpenFile) RUnlock() { f.mu.RUnlock() }

// IsAtimeFrozen returns the AtimeFrozen flag under the read lock. Used by
// READ / WRITE / QUERY_DIRECTORY / COPYCHUNK to decide whether to bump
// LastAccessTime after a successful operation.
func (f *OpenFile) IsAtimeFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.AtimeFrozen
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
func (f *OpenFile) CaptureNotifyCompletionFilter(filter uint32) (captured uint32, didCapture bool) {
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

// GetSession retrieves a session by ID.
// Delegates to SessionManager for unified session/credit management.
func (h *Handler) GetSession(sessionID uint64) (*session.Session, bool) {
	return h.SessionManager.GetSession(sessionID)
}

// DeleteSession removes a session by ID.
// This automatically cleans up credit tracking as well, and
// drops any cached SMB3 CREATE replay entries scoped to the
// session so the cache footprint is freed promptly rather
// than waiting on replayCacheTTL (MS-SMB2 §3.3.5.9).
func (h *Handler) DeleteSession(sessionID uint64) {
	h.SessionManager.DeleteSession(sessionID)
	if h.CreateReplayCache != nil {
		h.CreateReplayCache.ForgetSession(sessionID)
	}
}

// GetTree retrieves a tree connection by ID
func (h *Handler) GetTree(treeID uint32) (*TreeConnection, bool) {
	v, ok := h.trees.Load(treeID)
	if !ok {
		return nil, false
	}
	return v.(*TreeConnection), true
}

// DeleteTree removes a tree connection by ID
func (h *Handler) DeleteTree(treeID uint32) {
	h.trees.Delete(treeID)
}

// GetOpenFile retrieves an open file by FileID
func (h *Handler) GetOpenFile(fileID [16]byte) (*OpenFile, bool) {
	v, ok := h.files.Load(string(fileID[:]))
	if !ok {
		return nil, false
	}
	return v.(*OpenFile), true
}

// handleOpTracker tracks in-flight operations on a FileID via a WaitGroup.
// Created lazily by AcquireOpenFile; AcquireOpenFile adds and ReleaseOpenFile
// calls Done. WaitAndDeleteOpenFile waits for the WaitGroup to drain before
// removing the OpenFile from the map.
type handleOpTracker struct {
	wg sync.WaitGroup
}

// BeginHandleOp registers an in-flight operation on fileID without checking
// whether the OpenFile is currently present. It is intended for the
// connection dispatcher to call synchronously on the read loop BEFORE
// spawning the request goroutine, so that the handleOps counter for the
// FileID is incremented in wire order (independent of goroutine scheduling).
// Without this, a later CLOSE on the same TCP connection can race ahead of
// an earlier request's goroutine, observe handleOps empty, and delete the
// OpenFile before the prior request calls AcquireOpenFile — yielding a
// spurious STATUS_FILE_CLOSED (smbtorture compound_find.compound_find_close).
//
// The returned release func MUST be called exactly once when the request
// completes. A subsequent in-handler AcquireOpenFile/ReleaseOpenFile pair on
// the same FileID still works correctly: the tracker is shared, so the
// nested Add/Done cancel out and the dispatcher's outer Done fires when the
// request finishes.
func (h *Handler) BeginHandleOp(fileID [16]byte) func() {
	key := string(fileID[:])
	v, _ := h.handleOps.LoadOrStore(key, &handleOpTracker{})
	tracker := v.(*handleOpTracker)
	tracker.wg.Add(1)
	return func() { tracker.wg.Done() }
}

// AcquireOpenFile retrieves an open file by FileID and registers an in-flight
// operation on it. The caller MUST call ReleaseOpenFile when done. Returns
// nil,false when the handle is not found. This prevents a CLOSE on a concurrent
// goroutine from deleting the OpenFile before the caller finishes using it
// (smbtorture compound_find.compound_find_close).
func (h *Handler) AcquireOpenFile(fileID [16]byte) (*OpenFile, bool) {
	key := string(fileID[:])
	// Load-or-store the tracker; add to the WaitGroup BEFORE checking the
	// files map so that a concurrent WaitAndDeleteOpenFile sees our Add.
	v, _ := h.handleOps.LoadOrStore(key, &handleOpTracker{})
	tracker := v.(*handleOpTracker)
	tracker.wg.Add(1)

	f, ok := h.files.Load(key)
	if !ok {
		// File was already deleted; undo the Add.
		tracker.wg.Done()
		return nil, false
	}
	return f.(*OpenFile), true
}

// ReleaseOpenFile marks an in-flight operation on fileID as complete.
// Must be called exactly once for each successful AcquireOpenFile.
func (h *Handler) ReleaseOpenFile(fileID [16]byte) {
	key := string(fileID[:])
	if v, ok := h.handleOps.Load(key); ok {
		v.(*handleOpTracker).wg.Done()
	}
}

// WaitAndDeleteOpenFile waits for in-flight operations on fileID to drain,
// then deletes the OpenFile from the map and revokes resume keys. This
// replaces DeleteOpenFile for the CLOSE handler to prevent the race where
// CLOSE deletes a handle that QueryDirectory is about to look up.
func (h *Handler) WaitAndDeleteOpenFile(fileID [16]byte) {
	key := string(fileID[:])
	if v, ok := h.handleOps.Load(key); ok {
		v.(*handleOpTracker).wg.Wait()
		h.handleOps.Delete(key)
	}
	h.forgetReplayState(fileID)
	h.files.Delete(key)
	h.resumeKeys.revoke(fileID)
}

// DeleteOpenFile removes an open file by FileID and revokes any
// resume keys issued for this handle (used by FSCTL_SRV_COPYCHUNK and the
// session-cleanup path closeFilesWithFilter). Also clears the handleOps
// tracker so trackers created by BeginHandleOp on this FileID do not leak.
func (h *Handler) DeleteOpenFile(fileID [16]byte) {
	key := string(fileID[:])
	h.forgetReplayState(fileID)
	h.files.Delete(key)
	h.handleOps.Delete(key)
	h.resumeKeys.revoke(fileID)
}

// forgetReplayState drops both CREATE (by CreateGuid via the OpenFile)
// and LOCK (by FileID) replay-cache entries for a handle that is being
// closed. The cache windows are only meaningful while a retry could
// still arrive — once the handle is gone, so is any legitimate replay.
func (h *Handler) forgetReplayState(fileID [16]byte) {
	if h.CreateReplayCache != nil {
		if v, ok := h.files.Load(string(fileID[:])); ok {
			if of := v.(*OpenFile); of.CreateGuid != ([16]byte{}) {
				h.CreateReplayCache.Forget(of.CreateGuid)
			}
		}
	}
	if h.LockReplayCache != nil {
		h.LockReplayCache.ForgetFile(fileID)
	}
}

// ReleaseAllLocksForSession releases all byte-range locks held by a session.
// This is called during LOGOFF or connection cleanup to ensure locks are released
// even if CLOSE was not called for all open files.
func (h *Handler) ReleaseAllLocksForSession(ctx context.Context, sessionID uint64) {
	h.files.Range(func(key, value any) bool {
		openFile := value.(*OpenFile)
		if openFile.SessionID != sessionID {
			return true // Continue iterating
		}

		// Skip directories and pipes
		if openFile.IsDirectory || openFile.IsPipe || len(openFile.MetadataHandle) == 0 {
			return true
		}

		// Release locks for this file (per-open ownership)
		metaSvc := h.Registry.GetMetadataService()

		// UnlockAllForOpen doesn't return errors for missing locks
		if unlockErr := metaSvc.UnlockAllForOpen(ctx, openFile.MetadataHandle, openFile.OpenID()); unlockErr != nil {
			logger.Warn("ReleaseAllLocksForSession: failed to release locks",
				"share", openFile.ShareName,
				"path", openFile.Path,
				"error", unlockErr)
		}

		return true
	})
}

// CloseAllFilesForSession closes all open files for a session.
// For non-persisted opens this releases locks, flushes caches, handles delete-on-close,
// and removes file handles. When isDisconnect is true, eligible durable handles are
// instead persisted for reconnection (locks retained, caches NOT flushed, delete-on-close
// NOT executed). Both a transport drop and an explicit LOGOFF pass true: a durable handle
// is owned by the durable scope, not the session, so it survives logoff and stays
// reconnectable via DHnC/DH2C (smb2.durable-open.reopen4). Callers that pass
// isDisconnect=false — a TREE_DISCONNECT (CloseAllFilesForTree) or a session teardown
// that is not a reconnectable drop — fully close durable handles instead. Eligibility is
// further gated inside closeFilesWithFilter: delete-on-close opens and BR-lock-without-W
// opens are closed, not persisted.
// Returns the number of files closed.
func (h *Handler) CloseAllFilesForSession(ctx context.Context, sessionID uint64, isDisconnect bool) int {
	filter := func(f *OpenFile) bool {
		return f.SessionID == sessionID
	}
	return h.closeFilesWithFilter(ctx, sessionID, filter, "CloseAllFilesForSession", isDisconnect)
}

// CloseAllFilesForTree closes all open files associated with a tree connection.
// This releases locks, flushes caches, handles delete-on-close, and removes file handles.
// The sessionID parameter is used for authorization context during delete-on-close
// and lock release operations. Files are filtered by both treeID and sessionID for safety.
// Returns the number of files closed.
func (h *Handler) CloseAllFilesForTree(ctx context.Context, treeID uint32, sessionID uint64) int {
	filter := func(f *OpenFile) bool {
		return f.TreeID == treeID && f.SessionID == sessionID
	}
	// Tree disconnect is not a transport disconnect — fully close durable handles
	return h.closeFilesWithFilter(ctx, sessionID, filter, "CloseAllFilesForTree", false)
}

// closeFilesWithFilter closes files matching the filter predicate.
// This is the shared implementation for CloseAllFilesForSession and CloseAllFilesForTree.
// When isDisconnect is true, durable handles are persisted for later reconnection.
// When false (explicit LOGOFF or tree disconnect), durable handles are fully closed.
func (h *Handler) closeFilesWithFilter(
	ctx context.Context,
	sessionID uint64,
	filter func(*OpenFile) bool,
	caller string,
	isDisconnect bool,
) int {
	var closed int
	var toDelete [][16]byte
	// leaseReleases holds the opens whose per-handle lease/oplock record must be
	// released AFTER the open-file table has been shrunk (second pass). Releasing
	// in the first pass would let two opens of the SAME file with the SAME lease
	// key (still both present in h.files) each observe the other as a surviving
	// sibling and skip release, leaking the record. Pipes (no lease) and durable
	// handles persisted for reconnect (lease intentionally retained) are excluded.
	var leaseReleases []*OpenFile

	// Get session for auth context (may be nil if session already deleted)
	sess, _ := h.GetSession(sessionID)
	metaSvc := h.Registry.GetMetadataService()

	// First pass: collect files to close and release locks
	h.files.Range(func(key, value any) bool {
		openFile := value.(*OpenFile)
		if !filter(openFile) {
			return true // Continue iterating
		}

		// Handle pipe close
		if openFile.IsPipe {
			// Complete any pending async READ with STATUS_CANCELLED before closing.
			if h.PipeReadRegistry != nil {
				if pending := h.PipeReadRegistry.UnregisterByFileID(openFile.FileID); pending != nil {
					if pending.Callback != nil {
						go func(pr *PendingPipeRead) {
							if err := pr.Callback(pr.SessionID, pr.MessageID, pr.AsyncId, types.StatusCancelled, nil); err != nil {
								logger.Warn("pipe close: failed to cancel pending READ", "asyncId", pr.AsyncId, "error", err)
							}
						}(pending)
					}
				}
			}
			h.PipeManager.ClosePipe(openFile.FileID)
			toDelete = append(toDelete, openFile.FileID)
			closed++
			return true
		}

		// Durable handle persistence: when IsDurable is set AND this is a transport
		// disconnect (not an explicit LOGOFF), persist the handle to the
		// DurableHandleStore for later reconnection. On explicit LOGOFF the client
		// is intentionally closing the session, so durable handles are fully closed.
		//
		// Refuse to persist if the handle requested FILE_DELETE_ON_CLOSE at CREATE
		// time or marked DeletePending later via FileDispositionInformation. This
		// mirrors Samba `vfs_default_durable_disconnect` (source3/smbd/durable.c):
		// the disconnect path returns NT_STATUS_NOT_SUPPORTED for delete-on-close
		// opens so the caller falls back to normal close and executes the delete.
		// Required for smb2.durable-open.delete_on_close1 — without this, the
		// file persists across the disconnect and a subsequent fresh CREATE sees
		// the stale content instead of a freshly-created empty file. The matching
		// delete_on_close2 test stays in KNOWN_FAILURES (same as Samba upstream).
		hasDeleteOnClose := openFile.CreateOptions&types.FileDeleteOnClose != 0 ||
			openFile.DeletePending
		if openFile.IsDurable && h.DurableStore != nil && isDisconnect && !hasDeleteOnClose {
			username := ""
			var sessionKeyHash [32]byte
			if sess != nil {
				username = sess.Username
				sessionKeyHash = computeSessionKeyHash(sess)
			}

			// Capture current lease state + epoch from LeaseManager for reconnect
			// restoration. The epoch is the live OpLock.Lease.Epoch (lock layer);
			// persisting it lets the reconnect CREATE response restore the exact
			// epoch the client last saw (smb2.durable-v2-open.lock-lease).
			var leaseState uint32
			var leaseEpoch uint16
			if h.LeaseManager != nil && openFile.LeaseKey != ([16]byte{}) {
				if state, epoch, found := h.LeaseManager.GetLeaseState(ctx, openFile.LeaseKey); found {
					leaseState = state
					leaseEpoch = epoch
				}
			}

			// MS-SMB2 §3.3.4.18 persist gate: refuse to persist when the
			// open holds a byte-range lock under a lease lacking W. The
			// disconnected reconnect cannot reliably re-establish the lock
			// because the BR-lock is bound to the open's OpenID and a
			// non-W lease cannot promote to W on reconnect without breaking
			// other holders. Mirrors Samba's vfs_default_durable_disconnect
			// (NT_STATUS_NOT_SUPPORTED → fall through to normal close).
			// smbtorture smb2.durable-v2-open.lock-noW-lease.
			//
			// Source of truth is the lock manager — not openFile.HasByteRangeLocks
			// — to close a TOCTOU window where an async-parked LOCK goroutine
			// could set the flag after this read. The manager carries the
			// authoritative per-OpenID lock list (lock_async.go::resumePendingLock
			// adds via metaSvc.LockFile, which the manager records).
			persistGated := !shouldPersistDurableOnDisconnect(leaseState, openHasLocks(metaSvc, openFile))
			if persistGated {
				logger.Debug(caller+": durable persist refused (BR-lock without W lease)",
					"path", openFile.Path,
					"leaseState", fmt.Sprintf("0x%x", leaseState),
					"hasBRLocks", true)
			} else {
				// Serialize the persist against concurrent create-time
				// purge windows; see durablePurgeMu comment.
				h.durablePurgeMu.Lock()
				persisted := buildPersistedDurableHandle(openFile, username, sessionKeyHash, h.StartTime, leaseState, leaseEpoch)
				err := h.DurableStore.PutDurableHandle(ctx, persisted)
				h.durablePurgeMu.Unlock()
				if err != nil {
					logger.Warn(caller+": failed to persist durable handle",
						"path", openFile.Path,
						"error", err)
					// Fall through to normal close on persistence failure
				} else {
					logger.Debug(caller+": durable handle persisted for reconnect",
						"path", openFile.Path,
						"fileID", fmt.Sprintf("%x", openFile.FileID),
						"timeout", openFile.DurableTimeoutMs)
					// Do NOT release locks, flush caches, or execute delete-on-close
					// The handle lives on in the DurableHandleStore
					toDelete = append(toDelete, openFile.FileID)
					closed++
					return true
				}
			}
		}

		// Cancel any pending blocking LOCK requests for this handle and
		// release held byte-range locks. Mirrors the explicit CLOSE path
		// (close.go step 7) so callers like LOGOFF / tree-disconnect /
		// transport drop deliver STATUS_RANGE_NOT_LOCKED to parked waiters
		// per Samba `brl_close_fnum`. Without this, blocking locks parked
		// on a closing handle wait for the catch-all session/tree drain
		// which fires STATUS_CANCELLED — failing smb2.lock.cancel-logoff
		// which expects RANGE_NOT_LOCKED or OK.
		if !openFile.IsDirectory && len(openFile.MetadataHandle) > 0 {
			if h.PendingLockRegistry != nil {
				for _, parked := range h.PendingLockRegistry.UnregisterAllForOwner(openFile.OpenID()) {
					if parked.Callback != nil {
						if err := parked.Callback(parked.SessionID, parked.MessageID, parked.AsyncId, types.StatusRangeNotLocked, nil); err != nil {
							logger.Debug(caller+": failed to send RANGE_NOT_LOCKED",
								"asyncId", parked.AsyncId, "error", err)
						}
					}
					if h.LockWaitGraph != nil && parked.OwnerID != "" {
						h.LockWaitGraph.RemoveWaiter(parked.OwnerID)
					}
				}
			}
			_ = metaSvc.UnlockAllForOpen(ctx, openFile.MetadataHandle, openFile.OpenID())
		}

		// Flush cache if needed
		if !openFile.IsDirectory && openFile.PayloadID != "" {
			h.flushFileCache(ctx, openFile)
		}

		// Handle delete-on-close (FileDispositionInformation OR per-handle
		// initial DOC from a CREATE FILE_DELETE_ON_CLOSE that was not
		// promoted earlier — TDIS / LOGOFF / disconnect skip the explicit
		// CLOSE handler, so the same close.go promotion applies here).
		//
		// Promote per-handle InitialDeleteOnClose to shared committed
		// DeletePending only when no OTHER handle on the same metadata
		// file remains open: otherwise the unlink in handleDeleteOnClose
		// would fire before the last sibling closes, violating MS-FSA
		// 2.1.5.4 (delete-on-close removes the file when the LAST handle
		// closes, not on the per-handle initial flag). When siblings
		// remain, propagate the DOC + DOC-setter parent key onto them so
		// the eventual sibling close in close.go (or this same teardown
		// for sibling opens in this iteration) fires the delete instead.
		isInitialDocOnly := openFile.InitialDeleteOnClose && !openFile.DeletePending
		if isInitialDocOnly && len(openFile.MetadataHandle) > 0 {
			otherHandleExists := false
			h.files.Range(func(_, value any) bool {
				other := value.(*OpenFile)
				if other.FileID == openFile.FileID {
					return true
				}
				if bytes.Equal(other.MetadataHandle, openFile.MetadataHandle) {
					otherHandleExists = true
					return false
				}
				return true
			})
			if otherHandleExists {
				// Propagate DOC to remaining handles so their eventual
				// close triggers the delete. Matches the close.go path
				// at "DOC propagated to other handles (not last)".
				h.files.Range(func(_, value any) bool {
					other := value.(*OpenFile)
					if other.FileID == openFile.FileID {
						return true
					}
					if bytes.Equal(other.MetadataHandle, openFile.MetadataHandle) {
						other.DeletePending = true
						other.DeleteOnCloseParentKey = openFile.DeleteOnCloseParentKey
						other.HasDeleteOnCloseParentKey = openFile.HasDeleteOnCloseParentKey
						h.StoreOpenFile(other)
					}
					return true
				})
				logger.Debug(caller+": initial DOC propagated to other handles (not last)",
					"path", openFile.Path)
				isInitialDocOnly = false // delete handled by remaining sibling
			}
		}
		if (openFile.DeletePending || isInitialDocOnly) && len(openFile.ParentHandle) > 0 && openFile.FileName != "" {
			h.handleDeleteOnClose(ctx, sess, openFile, caller)
		}

		// Queue this handle's per-open lease/oplock record for release in the
		// second pass (after the open-file table is shrunk). The explicit CLOSE
		// handler releases inline in close.go step 9, but LOGOFF /
		// tree-disconnect / transport-drop bypass that handler. Relying solely
		// on the later LeaseManager.ReleaseSessionLeases (a sessionMap scan
		// keyed by lease key) leaks the record whenever a later session reused
		// the same numeric lease key on another file and overwrote the
		// sessionMap entry — the root cause of the #568 rotating cross-test
		// lease flake. See releaseHandleLeaseRecord for the full rationale.
		leaseReleases = append(leaseReleases, openFile)

		toDelete = append(toDelete, openFile.FileID)
		closed++
		return true
	})

	// Second pass: delete collected file handles and clean up associated state
	for _, fileID := range toDelete {
		// Unregister any pending CHANGE_NOTIFY watchers for this handle.
		// The CLOSE handler (close.go) does this for explicit closes, but
		// closeFilesWithFilter bypasses the CLOSE handler. Without this,
		// stale watchers persist in the NotifyRegistry after connection
		// cleanup and can fire during subsequent tests, sending async
		// responses on dead connections with partially-destroyed sessions.
		// Per MS-SMB2 3.3.4.1: when the watched handle goes away the
		// pending request MUST complete with STATUS_NOTIFY_CLEANUP so the
		// client's async recv unblocks (smb2.notify.tcon, .dir).
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.Disarm(fileID)
			if notify := h.NotifyRegistry.Unregister(fileID); notify != nil && notify.AsyncCallback != nil {
				cleanupResp := &ChangeNotifyResponse{
					SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyCleanup},
				}
				// Gate on interim PENDING — even during teardown, the
				// interim must reach the wire first or the client sees
				// out-of-order responses on its still-alive socket.
				n := notify
				go h.NotifyRegistry.QueueFinalAfterInterim(n, func() {
					if err := n.AsyncCallback(n.SessionID, n.MessageID, n.AsyncId, cleanupResp); err != nil {
						logger.Debug("closeFilesWithFilter: failed to send STATUS_NOTIFY_CLEANUP",
							"sessionID", n.SessionID,
							"messageID", n.MessageID,
							"error", err)
					}
				})
			}
		}
		h.DeleteOpenFile(fileID)
	}

	// Third pass: release per-handle lease/oplock records. Runs after every
	// DeleteOpenFile above so releaseHandleLeaseRecord's "any other open on the
	// same file shares this key" scan sees the shrunk table — otherwise sibling
	// opens of the same file/key (all still present in the first pass) would
	// each defer to the other and the record would leak.
	for _, openFile := range leaseReleases {
		h.releaseHandleLeaseRecord(ctx, openFile, caller)
	}

	if closed > 0 {
		logger.Debug(caller+": closed files", "sessionID", sessionID, "count", closed)
	}

	return closed
}

// handleDeleteOnClose performs the delete operation for files marked with
// delete-on-close during session/tree/connection teardown.
//
// Two lease breaks fire around the delete:
//
//  1. Before the unlink, strip Handle from other sessions' leases on the
//     file being deleted (RH → R, RWH → RW). The file is going away, so
//     Handle caching becomes stale. Required by
//     smb2.lease.initial_delete_tdis / logoff / disconnect.
//
//  2. After the unlink, break the parent directory's Handle and Read leases
//     (content change). Matches the explicit CLOSE path at close.go:334.
//
// Both breaks are async: the triggering SMB request (TDIS / LOGOFF / CLOSE /
// transport close) is on tree2/session2, while the lease holder is on a
// different session/tree on the same transport. Waiting for an ACK here
// would deadlock — the holder can only ack after the triggering request
// returns.
func (h *Handler) handleDeleteOnClose(ctx context.Context, sess *session.Session, openFile *OpenFile, caller string) {
	authCtx := h.buildCleanupAuthContext(ctx, sess)
	// Thread the closing handle's RqLs ParentLeaseKey so notifyDirChange can
	// apply the MS-SMB2 §3.3.4.20 / Samba `dirlease_should_break` parent-key
	// suppression rule on the parent dir lease (#470 C6/C7).
	PropagateOpenFileParentLeaseKey(authCtx, openFile)
	metaSvc := h.Registry.GetMetadataService()

	if h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
		lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
		// Exclude the closing session: its leases on this file are about to
		// be released anyway, and firing self-breaks creates spurious
		// notifications that leak into later tests (observed regressing
		// smb2.lease.v1_bug15148 to count=2).
		excludeOwner := &lock.LockOwner{ClientID: fmt.Sprintf("smb:%d", openFile.SessionID)}
		if breakErr := h.LeaseManager.BreakFileHandleLeasesOnDelete(lockFileHandle, openFile.ShareName, excludeOwner); breakErr != nil {
			logger.Debug(caller+": file Handle lease break on delete failed", "path", openFile.Path, "error", breakErr)
		}
	}

	var deleted bool
	if openFile.IsDirectory {
		if err := metaSvc.RemoveDirectory(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
			logger.Debug(caller+": failed to delete directory", "path", openFile.Path, "error", err)
		} else {
			logger.Debug(caller+": directory deleted", "path", openFile.Path)
			deleted = true
		}
	} else {
		if _, err := metaSvc.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
			logger.Debug(caller+": failed to delete file", "path", openFile.Path, "error", err)
		} else {
			logger.Debug(caller+": file deleted", "path", openFile.Path)
			deleted = true
		}
	}

	if deleted {
		// No SMBHandlerContext available on the TDIS/LOGOFF/disconnect
		// teardown path — pass nil so the helper falls back to inline
		// dispatch (those paths don't ship a triggering response on the
		// same wire, so the deferred-via-PostSend ordering is unneeded).
		h.breakParentDirLeasesForContentChange(nil, authCtx, openFile)
	}
}

// DeleteAllTreesForSession removes all tree connections for a session.
// Returns the number of trees deleted.
func (h *Handler) DeleteAllTreesForSession(sessionID uint64) int {
	var deleted int
	var toDelete []uint32

	// First pass: collect trees to delete
	h.trees.Range(func(key, value any) bool {
		tree := value.(*TreeConnection)
		if tree.SessionID == sessionID {
			toDelete = append(toDelete, tree.TreeID)
			deleted++
		}
		return true
	})

	// Second pass: delete collected trees
	for _, treeID := range toDelete {
		h.DeleteTree(treeID)
	}

	if deleted > 0 {
		logger.Debug("DeleteAllTreesForSession: deleted trees",
			"sessionID", sessionID,
			"count", deleted)
	}

	return deleted
}

// WaitForCleanup blocks until all in-progress session cleanups have finished,
// or until the timeout (3 seconds) expires. Called at the start of SESSION_SETUP
// to ensure that stale state from a prior disconnected session is fully removed
// from the shared Handler maps before a new session starts operating.
//
// The timeout prevents indefinite blocking when cleanup is slow (e.g., flushing
// many open files), which would cause smbtorture connection timeouts.
func (h *Handler) WaitForCleanup() {
	done := make(chan struct{})
	go func() {
		h.cleanupWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		logger.Warn("WaitForCleanup: timed out after 3s, proceeding with session setup")
	}
}

// SignalPendingCleanup increments the cleanup WaitGroup by count.
// This MUST be called before any async cleanup work begins, to ensure
// that WaitForCleanup() in a new session's SESSION_SETUP will block
// until the cleanup is done.
//
// The race: When a connection drops, cleanup runs in the old connection's
// goroutine. The accept loop can spawn a new connection goroutine before
// the old goroutine enters CleanupSession. If Add(1) is inside
// CleanupSession, there is a window where WaitForCleanup returns 0
// (no pending cleanup) even though cleanup has not started yet.
// By calling SignalPendingCleanup before the cleanup loop, the WaitGroup
// is guaranteed to be non-zero when the new session checks it.
func (h *Handler) SignalPendingCleanup(count int) {
	h.cleanupWg.Add(count)
}

// SignalCleanupDone decrements the cleanup WaitGroup by one.
// Used by panic recovery in the cleanup loop to release remaining slots
// when CleanupSession cannot be called (because it would call Done itself).
func (h *Handler) SignalCleanupDone() {
	h.cleanupWg.Done()
}

// ExpireSessionNotifies completes any pending CHANGE_NOTIFY requests for a
// session whose Kerberos ticket has expired, WITHOUT tearing the session down
// (it may still re-authenticate via SESSION_SETUP). An expired session rejects
// most commands with STATUS_NETWORK_SESSION_EXPIRED (MS-SMB2 §3.3.5.2.9); an
// outstanding async CHANGE_NOTIFY armed before expiry must also be completed so
// the client's smb2_notify_recv unblocks instead of hanging forever. The final
// response carries STATUS_CANCELLED: the request is being cancelled because the
// session can no longer serve it, which is exactly what smbtorture
// smb2.session.expire2s / expire2e assert (session.c:1641 expects
// NT_STATUS_CANCELLED for the cancelled notify). Unlike
// releaseSessionLeasesAndNotifies this touches ONLY the notify registry —
// leases, locks and the session itself survive so the client can
// reauthenticate and keep using its open handles. Idempotent:
// ExpirePendingForSession removes the watchers, so repeated calls on the
// subsequent expired requests of the same window are no-ops.
func (h *Handler) ExpireSessionNotifies(sessionID uint64) {
	if h.NotifyRegistry == nil {
		return
	}
	// ExpirePendingForSession (not UnregisterAllForSession): the session
	// survives the ticket expiry and may re-authenticate, so its handles stay
	// armed and buffered-event accounting carries into the re-issued NOTIFY.
	for _, notify := range h.NotifyRegistry.ExpirePendingForSession(sessionID) {
		if notify.AsyncCallback == nil {
			continue
		}
		resp := &ChangeNotifyResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusCancelled},
		}
		n := notify
		h.NotifyRegistry.QueueFinalAfterInterim(n, func() {
			if err := n.AsyncCallback(n.SessionID, n.MessageID, n.AsyncId, resp); err != nil {
				logger.Debug("expired session: failed to complete pending CHANGE_NOTIFY",
					"sessionID", n.SessionID,
					"messageID", n.MessageID,
					"error", err)
			}
		})
	}
}

// releaseSessionLeasesAndNotifies releases all leases and unregisters all
// CHANGE_NOTIFY watchers for the given session. This is factored out because
// it is needed in three places: explicit LOGOFF, re-auth failure, and
// transport disconnect (CleanupSession).
func (h *Handler) releaseSessionLeasesAndNotifies(ctx context.Context, sessionID uint64) {
	if h.LeaseManager != nil {
		if err := h.LeaseManager.ReleaseSessionLeases(ctx, sessionID); err != nil {
			logger.Warn("releaseSessionLeasesAndNotifies: failed to release leases",
				"sessionID", sessionID,
				"error", err)
		}
	}
	if h.NotifyRegistry != nil {
		// Per MS-SMB2 3.3.5.5.2 / 3.3.5.5.3: when a session is destroyed
		// (LOGOFF, transport drop, re-auth failure, or PreviousSessionID
		// supersession), pending CHANGE_NOTIFY requests MUST complete with
		// STATUS_NOTIFY_CLEANUP so the client unblocks its async recv.
		// Mirrors the per-file path in close.go.
		//
		// Delivery is SYNCHRONOUS — the response carries the OLD session's
		// SessionID and MUST be signed with that session's key. Our caller
		// (CleanupSession on the PreviousSessionID path) deletes the session
		// immediately after this returns; an async (`go func`) delivery would
		// race with DeleteSession and send the response unsigned, which the
		// client rejects. This is the missing piece behind
		// smb2.notify.session-reconnect (issue #473): the client never sees
		// the cleanup and hangs in smb2_notify_recv. The LOGOFF caller keeps
		// the session alive for response signing anyway, so sync delivery is
		// correct there as well.
		for _, notify := range h.NotifyRegistry.UnregisterAllForSession(sessionID) {
			if notify.AsyncCallback == nil {
				continue
			}
			cleanupResp := &ChangeNotifyResponse{
				SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyCleanup},
			}
			n := notify
			h.NotifyRegistry.QueueFinalAfterInterim(n, func() {
				if err := n.AsyncCallback(n.SessionID, n.MessageID, n.AsyncId, cleanupResp); err != nil {
					logger.Debug("session cleanup: failed to send STATUS_NOTIFY_CLEANUP",
						"sessionID", n.SessionID,
						"messageID", n.MessageID,
						"error", err)
				}
			})
		}
	}
	h.cancelAsyncOpsForSession(sessionID)
}

// cancelAsyncOpsForSession cancels pending pipe reads, parked CREATEs, and
// blocked LOCKs for a session. Used by both CleanupSession and
// PreviousSessionID teardown.
func (h *Handler) cancelAsyncOpsForSession(sessionID uint64) {
	if h.PipeReadRegistry != nil {
		for _, pending := range h.PipeReadRegistry.UnregisterAllForSession(sessionID) {
			if pending.Callback != nil {
				go func(pr *PendingPipeRead) {
					if err := pr.Callback(pr.SessionID, pr.MessageID, pr.AsyncId, types.StatusCancelled, nil); err != nil {
						logger.Warn("session cleanup: failed to cancel pending pipe READ", "asyncId", pr.AsyncId, "error", err)
					}
				}(pending)
			}
		}
	}
	if h.PendingCreateRegistry != nil {
		for _, parked := range h.PendingCreateRegistry.UnregisterAllForSession(sessionID) {
			if parked.Callback != nil {
				go func(p *PendingCreate) {
					if err := p.Callback(p.SessionID, p.MessageID, p.AsyncId, types.StatusCancelled, nil); err != nil {
						logger.Debug("session cleanup: failed to cancel pending CREATE",
							"asyncId", p.AsyncId, "messageID", p.MessageID, "error", err)
					}
				}(parked)
			}
		}
	}
	if h.PendingLockRegistry != nil {
		for _, parked := range h.PendingLockRegistry.UnregisterAllForSession(sessionID) {
			if parked.Callback != nil {
				go func(p *PendingLock) {
					if err := p.Callback(p.SessionID, p.MessageID, p.AsyncId, types.StatusRangeNotLocked, nil); err != nil {
						logger.Debug("session cleanup: failed to cancel pending LOCK",
							"asyncId", p.AsyncId, "messageID", p.MessageID, "error", err)
					}
				}(parked)
			}
			if h.LockWaitGraph != nil && parked.OwnerID != "" {
				h.LockWaitGraph.RemoveWaiter(parked.OwnerID)
			}
		}
	}
}

// CleanupSession performs full cleanup for a session.
// This closes all files, releases all locks, removes all tree connections,
// and deletes the session. Called on LOGOFF or connection close.
// When isDisconnect is true (transport drop), durable handles are preserved.
// When false (explicit LOGOFF), all handles are fully closed.
//
// IMPORTANT: The caller must call SignalPendingCleanup(1) before calling
// CleanupSession to ensure the cleanup barrier is visible to new sessions.
// The WaitGroup decrement (Done) happens via defer.
func (h *Handler) CleanupSession(ctx context.Context, sessionID uint64, isDisconnect bool) {
	defer h.cleanupWg.Done()

	logger.Debug("CleanupSession: starting cleanup", "sessionID", sessionID, "isDisconnect", isDisconnect)

	// 1. Close all open files (this also releases locks and flushes caches)
	filesClosed := h.CloseAllFilesForSession(ctx, sessionID, isDisconnect)

	// 2. Release leases and notify watchers that may not have been
	// cleaned up by per-file CLOSE (e.g. client disconnected without
	// closing all files, or re-auth failure).
	h.releaseSessionLeasesAndNotifies(ctx, sessionID)

	// 3. Delete all tree connections
	treesDeleted := h.DeleteAllTreesForSession(sessionID)

	// 4. Clean up any pending auth state (all channels)
	h.DeleteAllPendingAuthForSession(sessionID)

	// 5. Delete the session itself
	h.DeleteSession(sessionID)

	// State leak detection: audit all shared maps for any items still belonging
	// to the cleaned-up session. Any found items are logged at WARN level.
	leaked := h.AuditSessionCleanup(sessionID)

	logger.Debug("CleanupSession: completed",
		"sessionID", sessionID,
		"filesClosed", filesClosed,
		"treesDeleted", treesDeleted,
		"leaked", leaked)
}

// flushFileCache flushes cached data for an open file.
// This is a helper used during cleanup to ensure data durability.
func (h *Handler) flushFileCache(ctx context.Context, openFile *OpenFile) {
	if openFile.PayloadID == "" {
		return
	}

	blockStore, err := h.Registry.GetBlockStoreForHandle(ctx, openFile.MetadataHandle)
	if err != nil {
		logger.Warn("flushFileCache: block store not available for handle",
			"path", openFile.Path,
			"error", err)
		return
	}

	// Use blocking Flush for immediate durability
	_, flushErr := blockStore.Flush(ctx, string(openFile.PayloadID))
	if flushErr != nil {
		logger.Warn("flushFileCache: flush failed",
			"path", openFile.Path,
			"payloadID", openFile.PayloadID,
			"error", flushErr)
	} else {
		logger.Debug("flushFileCache: flushed",
			"path", openFile.Path,
			"payloadID", openFile.PayloadID)
	}
}

// buildCleanupAuthContext creates an AuthContext for cleanup operations.
// This is used during session/tree cleanup when we need to perform file operations
// (like delete-on-close) but don't have a full SMBHandlerContext.
// If the session is available, it uses the session user's UID/GID.
// Otherwise, it falls back to root credentials for cleanup operations.
func (h *Handler) buildCleanupAuthContext(ctx context.Context, sess *session.Session) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:                ctx,
		Identity:               &metadata.Identity{},
		BypassTraverseChecking: true,
	}

	if sess != nil && sess.User != nil {
		// Use session user's UID/GID from User object
		uid, gid := getUserIdentity(sess.User)
		authCtx.Identity.UID = &uid
		authCtx.Identity.GID = &gid
		authCtx.Identity.Username = sess.User.Username
		authCtx.ClientAddr = sess.ClientAddr
	} else {
		// Fallback to root for cleanup operations when session info is unavailable.
		//
		// SECURITY NOTE: Using root credentials bypasses normal permission checks.
		// This is acceptable because:
		// 1. Delete-on-close can only be set via SET_INFO with FileDispositionInformation,
		//    which requires the file to have been opened with DELETE access.
		// 2. The cleanup is completing an operation the user was already authorized
		//    to perform when they opened the file.
		// 3. Without this fallback, files marked for deletion during ungraceful
		//    disconnect would remain orphaned in the metadata store.
		rootUID := uint32(0)
		rootGID := uint32(0)
		authCtx.Identity.UID = &rootUID
		authCtx.Identity.GID = &rootGID
	}

	return authCtx
}

// GenerateSessionID generates a new unique session ID.
// Delegates to SessionManager for ID generation.
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

// baseFileUUID returns the base file's UUID for an ADS path, or fallback for non-ADS.
// ADS streams share the base file's FileId so that all stream handles compare equal.
func (h *Handler) baseFileUUID(authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string, fallback [16]byte) [16]byte {
	if colonIdx := strings.Index(name, ":"); colonIdx > 0 && len(parentHandle) > 0 {
		metaSvc := h.Registry.GetMetadataService()
		if baseFile, _, err := metaSvc.LookupCaseInsensitive(authCtx, parentHandle, name[:colonIdx]); err == nil && baseFile != nil {
			return baseFile.ID
		}
	}
	return fallback
}

// GenerateFileID generates a new unique file ID
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
func (h *Handler) CreateSession(clientAddr string, isGuest bool, username, domain string) *session.Session {
	return h.SessionManager.CreateSession(clientAddr, isGuest, username, domain)
}

// CreateSessionWithID creates a session with a specific ID (for pending auth flows).
// The session is created in the SessionManager and returned.
func (h *Handler) CreateSessionWithID(sessionID uint64, clientAddr string, isGuest bool, username, domain string) *session.Session {
	sess := session.NewSession(sessionID, clientAddr, isGuest, username, domain)
	// Store directly - this is used for completing pending auth where we already have the ID
	h.SessionManager.StoreSession(sess)
	return sess
}

// CreateSessionWithUser creates an authenticated session with a DittoFS user.
// The session is linked to the user for permission checking during share access.
func (h *Handler) CreateSessionWithUser(sessionID uint64, clientAddr string, user *models.User, domain string) *session.Session {
	sess := session.NewSessionWithUser(sessionID, clientAddr, user, domain)
	h.SessionManager.StoreSession(sess)
	return sess
}

// CreateSessionWithUserAndExpiry creates an authenticated session with a
// bounded lifetime (e.g. a Kerberos ticket end-time). ExpiresAt is set
// before StoreSession to avoid a data race window where a concurrent reader
// could observe a zero ExpiresAt on the published session and skip the
// per-request expiry check in prepareDispatch (see #341 A1). A zero
// expiresAt is treated as "no expiry" by session.IsExpired.
func (h *Handler) CreateSessionWithUserAndExpiry(sessionID uint64, clientAddr string, user *models.User, domain string, expiresAt time.Time) *session.Session {
	sess := session.NewSessionWithUser(sessionID, clientAddr, user, domain)
	sess.ExpiresAt = expiresAt
	h.SessionManager.StoreSession(sess)
	return sess
}

// StoreTree stores a tree connection
func (h *Handler) StoreTree(tree *TreeConnection) {
	h.trees.Store(tree.TreeID, tree)
}

// StoreOpenFile stores an open file
func (h *Handler) StoreOpenFile(file *OpenFile) {
	h.files.Store(string(file.FileID[:]), file)
}

// pendingAuthKey is the composite key for pendingAuth lookups. SessionID is
// the session the handshake targets — the server-generated ID for an initial
// NTLM NEGOTIATE, the bound session for a bind, or the existing session for
// re-auth — and is the ID the client carries in the TYPE_3 header. ConnID
// disambiguates concurrent handshakes on the same SessionID so that parallel
// SESSION_SETUPs from different TCP connections do not clobber each other.
// Without per-connection keying, the regression guarded by
// smb2.multichannel.bugs.bug_15346 fails (Samba bug 15346): parallel binds
// race on a single slot and the TYPE_3 of one channel picks up the
// ServerChallenge of another.
type pendingAuthKey struct {
	SessionID uint64
	ConnID    uint64
}

// StorePendingAuth stores a pending authentication. pending.SessionID and
// pending.ConnID together form the lookup key.
func (h *Handler) StorePendingAuth(pending *PendingAuth) {
	h.pendingAuth.Store(pendingAuthKey{pending.SessionID, pending.ConnID}, pending)
}

// GetPendingAuth retrieves a pending authentication by (sessionID, connID).
func (h *Handler) GetPendingAuth(sessionID, connID uint64) (*PendingAuth, bool) {
	v, ok := h.pendingAuth.Load(pendingAuthKey{sessionID, connID})
	if !ok {
		return nil, false
	}
	return v.(*PendingAuth), true
}

// DeletePendingAuth removes a pending authentication for a specific connection.
func (h *Handler) DeletePendingAuth(sessionID, connID uint64) {
	h.pendingAuth.Delete(pendingAuthKey{sessionID, connID})
}

// DeleteAllPendingAuthForSession removes every pending-auth record associated
// with sessionID, regardless of connection. Used on session teardown (LOGOFF,
// connection cleanup) to invalidate any in-flight binds for the session.
func (h *Handler) DeleteAllPendingAuthForSession(sessionID uint64) {
	h.pendingAuth.Range(func(k, _ any) bool {
		if key, ok := k.(pendingAuthKey); ok && key.SessionID == sessionID {
			h.pendingAuth.Delete(key)
		}
		return true
	})
}

// isFileDeletePending reports whether any existing open on the same file
// (identified by its metadata handle) has DeletePending set. Per MS-FSA
// 2.1.5.1.2 and MS-SMB2 3.3.5.9: a subsequent open on a delete-pending
// file MUST fail with STATUS_DELETE_PENDING. The check runs BEFORE oplock
// break dispatch so the holder's oplock remains intact.
//
// Required by smbtorture smb2.oplock.doc: tree1 opens with Batch oplock,
// sets delete-on-close; tree2's open must return STATUS_DELETE_PENDING
// without triggering a break.
func (h *Handler) isFileDeletePending(fileHandle metadata.FileHandle) bool {
	pending := false
	h.files.Range(func(_, value any) bool {
		existing := value.(*OpenFile)
		if existing.IsPipe || len(existing.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(existing.MetadataHandle, fileHandle) {
			return true
		}
		if existing.DeletePending {
			pending = true
			return false
		}
		return true
	})
	return pending
}

// isFileOrBaseDeletePending extends isFileDeletePending to also check whether
// a deferred base-file delete is pending across stream/base handles.
//
// Per Samba semantics (also matches WPTS expectations): a stream open does
// NOT inherit the base file's DOC pending state. Streams are tracked as
// independent fsps; the base's mark-for-delete only fails subsequent stream
// opens once the base has actually been unlinked and the delete is being
// deferred for outstanding stream handles (BaseFileDeletePending).
//
// Cases handled here:
//   - Opening a base file: reject if any stream handle on the same base
//     carries BaseFileDeletePending (base was unlinked, delete deferred).
//   - Opening a stream:   reject if any handle on the base file or sibling
//     stream carries BaseFileDeletePending.
//
// filePath is the normalized path being opened (e.g., "file" or "file:Stream One").
func (h *Handler) isFileOrBaseDeletePending(fileHandle metadata.FileHandle, filePath string) bool {
	// Fast path: direct metadata-handle match against an existing handle
	// whose own DeletePending is set. Covers the same-file re-open case
	// (smbtorture smb2.oplock.doc, smb2.streams.delete).
	if h.isFileDeletePending(fileHandle) {
		return true
	}

	openBase := adsBasePath(filePath) // non-empty if filePath is a stream
	pending := false
	h.files.Range(func(_, value any) bool {
		existing := value.(*OpenFile)
		if existing.IsPipe || len(existing.MetadataHandle) == 0 {
			return true
		}
		if !existing.BaseFileDeletePending {
			return true
		}
		existingBase := adsBasePath(existing.Path)
		if openBase == "" {
			// Opening a base file: match against any stream of this base.
			if strings.EqualFold(existingBase, filePath) {
				pending = true
				return false
			}
		} else {
			// Opening a stream: match against a sibling stream sharing the
			// same base path, or against a base-file handle of that base.
			if strings.EqualFold(existingBase, openBase) ||
				strings.EqualFold(existing.Path, openBase) {
				pending = true
				return false
			}
		}
		return true
	})
	return pending
}

// checkShareModeConflict checks if opening a file with the given access and sharing
// modes would conflict with any existing opens on the same file or related
// streams. Per MS-FSA 2.1.5.1.2 + Samba semantics, share mode enforcement is:
//   - Same stream (same metadata handle) → always checked
//   - Base file vs its stream (or vice versa) → checked
//   - Stream A vs Stream B (different streams, same base) → NOT checked
//
// Returns true if a conflict exists (CREATE should fail with STATUS_SHARING_VIOLATION).
func (h *Handler) checkShareModeConflict(fileHandle metadata.FileHandle, newDesiredAccess, newShareAccess uint32, filePath string) bool {
	const (
		fileShareRead   = uint32(0x01)
		fileShareWrite  = uint32(0x02)
		fileShareDelete = uint32(0x04)

		// Access mask bits per MS-SMB2
		fileReadData   = uint32(0x00000001)
		fileWriteData  = uint32(0x00000002)
		fileAppendData = uint32(0x00000004)
		fileExecute    = uint32(0x00000020)
		deleteAccess   = uint32(0x00010000)
		genericRead    = uint32(0x80000000)
		genericWrite   = uint32(0x40000000)
		genericAll     = uint32(0x10000000)
		maxAllowed     = uint32(0x02000000)
	)

	// Stat-only opens (FILE_READ_ATTRIBUTES / FILE_WRITE_ATTRIBUTES /
	// READ_CONTROL / SYNCHRONIZE only) impose no share-mode constraint per
	// MS-SMB2 §3.3.5.9 + Samba `share_conflict` (source3/locking/share_mode_lock.c)
	// + `is_stat_open` (source3/smbd/open.c). smbtorture smb2.oplock.batch8 /
	// exclusive4 expect a stat-only second open on a BATCH/EXCLUSIVE holder
	// with ShareAccess=NONE to succeed with NT_STATUS_OK (no break, no
	// sharing violation).
	if isStatOnlyOpen(newDesiredAccess) {
		return false
	}

	// Helper: does access mask imply read?
	hasRead := func(access uint32) bool {
		return access&(fileReadData|fileExecute|genericRead|genericAll|maxAllowed) != 0
	}
	// Helper: does access mask imply write?
	hasWrite := func(access uint32) bool {
		return access&(fileWriteData|fileAppendData|genericWrite|genericAll) != 0
	}
	// Helper: does access mask imply delete?
	hasDelete := func(access uint32) bool {
		return access&(deleteAccess|genericAll) != 0
	}

	newBase := adsBasePath(filePath)

	conflict := false
	h.files.Range(func(key, value any) bool {
		existing := value.(*OpenFile)
		if existing.IsPipe {
			return true
		}
		if len(existing.MetadataHandle) == 0 {
			return true
		}

		// Same stream (same metadata handle) → full share mode check.
		// Base file vs its stream (or vice versa) → DELETE-only check.
		// Stream A vs stream B (different streams) → skip.
		sameFile := bytes.Equal(existing.MetadataHandle, fileHandle)
		crossStream := false
		if !sameFile {
			existingBase := adsBasePath(existing.Path)
			baseVsStream := false
			if newBase == "" && existingBase != "" {
				baseVsStream = strings.EqualFold(existingBase, filePath)
			} else if newBase != "" && existingBase == "" {
				baseVsStream = strings.EqualFold(newBase, existing.Path)
			}
			if !baseVsStream {
				return true
			}
			crossStream = true
		}

		// Cross-stream: only DELETE sharing enforced per Samba.
		if crossStream {
			if hasDelete(existing.DesiredAccess) && newShareAccess&fileShareDelete == 0 {
				conflict = true
				return false
			}
			if hasDelete(newDesiredAccess) && existing.ShareAccess&fileShareDelete == 0 {
				conflict = true
				return false
			}
			return true
		}

		// Same-stream: full share mode check.
		if !hasRead(existing.DesiredAccess) &&
			!hasWrite(existing.DesiredAccess) &&
			!hasDelete(existing.DesiredAccess) &&
			existing.DesiredAccess&fileAppendData == 0 {
			return true
		}

		if hasRead(existing.DesiredAccess) && newShareAccess&fileShareRead == 0 {
			conflict = true
			return false
		}
		if hasWrite(existing.DesiredAccess) && newShareAccess&fileShareWrite == 0 {
			conflict = true
			return false
		}
		if hasDelete(existing.DesiredAccess) && newShareAccess&fileShareDelete == 0 {
			conflict = true
			return false
		}

		if hasRead(newDesiredAccess) && existing.ShareAccess&fileShareRead == 0 {
			conflict = true
			return false
		}
		if hasWrite(newDesiredAccess) && existing.ShareAccess&fileShareWrite == 0 {
			conflict = true
			return false
		}
		if hasDelete(newDesiredAccess) && existing.ShareAccess&fileShareDelete == 0 {
			conflict = true
			return false
		}

		return true
	})
	return conflict
}

// lookupCaseInsensitive is a thin shim around
// MetadataService.LookupCaseInsensitive that keeps the existing
// (handler, metaSvc, parent, name) call signature used across the SMB
// handlers. NTFS-style paths are case-insensitive; DittoFS preserves the
// original on-disk casing and returns it via the second result.
func (h *Handler) lookupCaseInsensitive(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.MetadataService,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.File, string, error) {
	return metaSvc.LookupCaseInsensitive(authCtx, parentHandle, name)
}

// adsBasePath extracts the base file path from a potentially ADS-qualified path.
// For "dir/file.txt:stream" returns "dir/file.txt".
// For "dir/file.txt" (no stream) returns "" (not an ADS).
func adsBasePath(filePath string) string {
	lastSep := strings.LastIndex(filePath, "/")
	var fileName string
	if lastSep >= 0 {
		fileName = filePath[lastSep+1:]
	} else {
		fileName = filePath
	}
	colonIdx := strings.Index(fileName, ":")
	if colonIdx <= 0 {
		return ""
	}
	if lastSep >= 0 {
		return filePath[:lastSep+1] + fileName[:colonIdx]
	}
	return fileName[:colonIdx]
}

// checkShareDeleteConflict checks if any other open handle on the same file
// lacks FILE_SHARE_DELETE in its ShareAccess. Per MS-FSA 2.1.5.14.10, a rename
// requires all other opens to permit delete sharing. Returns true if a conflict
// exists (rename should be blocked with STATUS_SHARING_VIOLATION).
func (h *Handler) checkShareDeleteConflict(renameFile *OpenFile) bool {
	const fileShareDelete = uint32(0x04) // FILE_SHARE_DELETE

	conflict := false
	h.files.Range(func(key, value any) bool {
		other := value.(*OpenFile)
		// Skip the handle being renamed
		if other.FileID == renameFile.FileID {
			return true
		}
		// Only check handles to the same file (same metadata handle)
		if len(other.MetadataHandle) == 0 || len(renameFile.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(other.MetadataHandle, renameFile.MetadataHandle) {
			return true
		}
		// If this other handle does not allow delete sharing, conflict
		if other.ShareAccess&fileShareDelete == 0 {
			conflict = true
			return false // Stop iterating
		}
		return true
	})
	return conflict
}

// checkParentDirRenameConflict applies the destination-parent share-mode
// rule from MS-FSA 2.1.5.14.11.3 / Samba smbd_smb2_setinfo_rename_dst_parent_check.
// Rename takes an implicit DELETE-bearing access on the destination parent
// without granting share-delete; only the DELETE vs FILE_SHARE_DELETE pair
// matters for the conflict (the ADD_FILE bit doesn't interact with the
// share-mode word, so we don't probe write/share-write). An existing
// destination-parent open conflicts when it (a) lacks FILE_SHARE_DELETE —
// denying the rename's DELETE access — or (b) already holds DELETE access —
// incompatible with the rename's ShareAccess=0. The renamer's own handle
// is excluded by FileID. Caller passes the destination parent handle (same
// as source parent for same-directory rename). Returns true on conflict.
func (h *Handler) checkParentDirRenameConflict(renamerFileID [16]byte, dstParent metadata.FileHandle) bool {
	const fileShareDelete = uint32(0x04) // FILE_SHARE_DELETE
	if len(dstParent) == 0 {
		return false
	}
	conflict := false
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == renamerFileID {
			return true
		}
		if len(other.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(other.MetadataHandle, dstParent) {
			return true
		}
		// Stat-only opens (READ_ATTRIBUTES / WRITE_ATTRIBUTES / SYNCHRONIZE /
		// READ_CONTROL only) impose no share-mode constraint per MS-SMB2
		// §3.3.5.9.8 + Samba `is_lease_stat_open`. smbtorture rename.msword
		// opens the parent dir stat-only with ShareAccess=0 and expects the
		// rename to succeed; without this filter the lack of FILE_SHARE_DELETE
		// would falsely trip the conflict.
		if isStatOnlyOpen(other.DesiredAccess) {
			return true
		}
		if other.ShareAccess&fileShareDelete == 0 || hasDeleteAccess(other.DesiredAccess) {
			conflict = true
			return false
		}
		return true
	})
	return conflict
}

// snapshotOpenChildren returns the metadata handles of every open file whose
// ParentHandle equals dirHandle. Caller must read h.files only once; iterating
// twice could observe inconsistent open state across a concurrent CLOSE.
func (h *Handler) snapshotOpenChildren(dirHandle metadata.FileHandle) []metadata.FileHandle {
	var children []metadata.FileHandle
	h.files.Range(func(_, value any) bool {
		of := value.(*OpenFile)
		if len(of.ParentHandle) == 0 || len(of.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(of.ParentHandle, dirHandle) {
			return true
		}
		children = append(children, of.MetadataHandle)
		return true
	})
	return children
}

// anyOpenChild reports whether any open file currently has ParentHandle ==
// dirHandle. Cheaper than snapshotOpenChildren when only the boolean is
// needed (post-break recheck in the directory-rename path).
func (h *Handler) anyOpenChild(dirHandle metadata.FileHandle) bool {
	open := false
	h.files.Range(func(_, value any) bool {
		of := value.(*OpenFile)
		if len(of.ParentHandle) == 0 {
			return true
		}
		if !bytes.Equal(of.ParentHandle, dirHandle) {
			return true
		}
		open = true
		return false
	})
	return open
}

// hasOpenHandleOnFile reports whether any open file handle (other than the
// renamer's own handle) currently references targetMeta. Used by the
// SET_INFO FileRenameInformation handler to enforce MS-FSA §2.1.5.14.10
// "rename overwrite onto an open file" — once any H-lease on the destination
// has been broken to RW, the destination's open handle still blocks the
// overwrite and must surface as STATUS_ACCESS_DENIED.
//
// excludeFileID is the rename's own SMB FileID (the source handle). It is
// excluded from the conflict check so a self-rename via the only handle on
// targetMeta is allowed (degenerate case; matches Samba behavior).
func (h *Handler) hasOpenHandleOnFile(targetMeta metadata.FileHandle, excludeFileID [16]byte) bool {
	if len(targetMeta) == 0 {
		return false
	}
	conflict := false
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == excludeFileID {
			return true
		}
		if len(other.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(other.MetadataHandle, targetMeta) {
			return true
		}
		conflict = true
		return false
	})
	return conflict
}

// hasReadAccess reports whether the given access mask includes read access.
// Checks FILE_READ_DATA, FILE_EXECUTE, GENERIC_READ, GENERIC_ALL, and
// MAXIMUM_ALLOWED. FILE_EXECUTE is treated as read access because the
// canonical SMB clients (Samba, Windows) allow READ on a handle opened with
// only FILE_EXECUTE — execution implies read, and the smb2.read.access
// torture test exercises that path.
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

// getCachedShares returns the cached share list, rebuilding if invalidated.
// Thread-safe via RWMutex (concurrent reads allowed, exclusive write for rebuild).
func (h *Handler) getCachedShares() []rpc.ShareInfo1 {
	h.sharesCacheMu.RLock()
	if h.sharesCacheValid {
		shares := h.cachedShares
		h.sharesCacheMu.RUnlock()
		return shares
	}
	h.sharesCacheMu.RUnlock()

	// Rebuild cache under write lock
	h.sharesCacheMu.Lock()
	defer h.sharesCacheMu.Unlock()

	// Double-check after acquiring write lock (another goroutine may have rebuilt)
	if h.sharesCacheValid {
		return h.cachedShares
	}

	if h.Registry == nil {
		return nil
	}

	shareNames := h.Registry.ListShares()
	shares := make([]rpc.ShareInfo1, 0, len(shareNames))
	for _, name := range shareNames {
		if strings.EqualFold(name, "/ipc$") {
			continue
		}
		displayName := strings.TrimPrefix(name, "/")
		shares = append(shares, rpc.ShareInfo1{
			Name:    displayName,
			Type:    rpc.STYPE_DISKTREE,
			Comment: "DittoFS share",
		})
	}

	h.cachedShares = shares
	h.sharesCacheValid = true

	return shares
}

// invalidateShareCache marks the share list cache as stale.
// Called by the Runtime share change callback.
func (h *Handler) invalidateShareCache() {
	h.sharesCacheMu.Lock()
	h.sharesCacheValid = false
	h.sharesCacheMu.Unlock()
}

// RegisterShareChangeCallback subscribes to share change events from the Runtime
// to invalidate the cached share list used by pipe CREATE operations.
func (h *Handler) RegisterShareChangeCallback() {
	if h.Registry == nil {
		return
	}
	h.Registry.OnShareChange(func(_ []string) {
		h.invalidateShareCache()
	})
}
