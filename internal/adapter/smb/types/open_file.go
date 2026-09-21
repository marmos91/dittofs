package types

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

type OpenName struct {
	Path         string
	FileName     string
	ParentHandle metadata.FileHandle
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
// fields (FileID/TreeID/SessionID) are safe to access without the mutex.
// CreateOptions is not: the SET_INFO FileModeInformation path overlays the
// mode bits under mu, so it must be read under mu as well.
//
// MetadataHandle, PayloadID and the name triple are NOT immutable: the first
// WRITE on a file created empty caches the payload the metadata store
// allocated, SET_REPARSE_POINT and COPYCHUNK replace it, SET_REPARSE_POINT
// also repoints the handle itself when a placeholder becomes a symlink, and
// SET_INFO rename rewrites the name/path/parent triple. Reach the handle
// through GetMetadataHandle, the payload through GetPayloadID / SetPayloadID
// and the triple through Name / SetName. GetMetadataHandle carries the single
// statement of when MetadataHandle may be read directly, including which reads
// are exempt; it is not restated here so the two cannot drift apart.
type OpenFile struct {
	// mu guards the mutable fields listed in the struct comment above. Held
	// across QueryDirectory enumeration R-M-W, freeze/thaw bookkeeping in
	// SET_INFO BasicInfo, the SMB delayed-write timestamp helpers and the
	// QUERY_INFO frozen/delayed-write overlay reads.
	mu sync.RWMutex

	// name is the current OpenName. Read via Name, publish via SetName.
	name atomic.Pointer[OpenName]

	FileID        [16]byte
	TreeID        uint32
	SessionID     uint64
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
	//
	// Per-op gates are INTENTIONALLY frozen to this snapshot, not re-evaluated
	// through the central metadata permission core on each request:
	//
	//   - The single access check happens once, at CREATE, through the central
	//     metadata.Service (CheckFileAccess / CheckFileAccessWithParent).
	//     Subsequent READ / WRITE / DELETE / SET_INFO / IOCTL (sparse, copychunk,
	//     fsctl) handlers gate against this frozen GrantedAccess rather than
	//     re-running the checker. This is the MS-SMB2 / MS-FSA handle model
	//     (MS-SMB2 §3.3.5.12/§3.3.5.13 gate READ and WRITE on Open.GrantedAccess
	//     — MS-FSA's own read and write algorithms never consult it — and MS-FSA
	//     §2.1.5.5 Phase 1 delete-on-close honors the authorization frozen at
	//     open), and it is
	//     deliberately spec-correct: an open's rights do NOT shrink or grow if
	//     the DACL changes after the handle is granted. Re-evaluating per-op
	//     would be a protocol bug, not a fix — a Windows client holding a valid
	//     handle would start seeing STATUS_ACCESS_DENIED mid-stream.
	//   - DELETE access verified at open (FILE_DELETE_ON_CLOSE / SET_INFO
	//     FileDispositionInformation) is propagated to the unlink path via
	//     AuthContext.HasDeleteAccess so the metadata delete check honors the
	//     same frozen authorization (see metadata.checkDeletePermission, #388).
	//
	// So for SMB the central checker is the SOLE authorizer; per-op handlers
	// only consult the mask it produced. Centralizing the per-op gates further
	// would change spec-mandated semantics and is explicitly out of scope.
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
	// MS-FSA 2.1.5.15.3 ("FileDispositionInformation") and Samba `is_delete_on_close_set` (locking.tdb).
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
	DeletePending        bool // committed shared DOC (visible to other opens)
	InitialDeleteOnClose bool // per-handle initial DOC from CREATE FILE_DELETE_ON_CLOSE

	// docLeaving marks a handle that has already run its delete-on-close
	// election (electDeleteOnClose) and can therefore no longer honour a DOC
	// propagated to it; the election's sibling scans skip such handles.
	//
	// Guarded by Handler.docElectionMu — NOT by OpenFile.mu — because it is
	// only ever read as part of a scan that must be atomic with the writes to
	// it. The handle stays in Handler.files while marked, so every other scan
	// (CREATE delete-pending gate, share modes, oplocks, rename conflict) still
	// sees it until its owner's own removal step.
	docLeaving bool

	// ShareAccess stores the sharing mode from the CREATE request.
	// Used for share mode conflict checking during rename and other operations.
	// Bit mask: 0x01 (FILE_SHARE_READ), 0x02 (FILE_SHARE_WRITE), 0x04 (FILE_SHARE_DELETE)
	ShareAccess uint32

	// CreateOptions stores the original CreateOptions from the CREATE request,
	// used to populate FileModeInformation (FILE_WRITE_THROUGH, FILE_SEQUENTIAL_ONLY, etc.)
	CreateOptions CreateOptions

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

	// Timestamp freeze/unfreeze state per MS-FSA §2.1.5.15.2 ("FileBasicInformation").
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

	// READ-driven LastAccessTime coalescing: a READ pushes the access time to
	// the metadata store at most once per smbAtimeUpdateWindow. In between the
	// newest access time lives on the handle, surfaced by QUERY_INFO and
	// persisted at CLOSE.
	SmbAtimeWrittenAt time.Time // when the last READ-driven atime reached the store
	SmbPendingAtime   time.Time // newest access time not yet written to the store (zero ⇒ none)

	// SmbParentAtimeWrittenAt bounds the parent-directory LastAccessTime bump a
	// WRITE performs, the way SmbAtimeWrittenAt bounds the file's. A suppressed
	// bump is dropped rather than deferred — see noteSmbParentAccess.
	SmbParentAtimeWrittenAt time.Time

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
	// Established per MS-SMB2 §3.3.5.9.11 ("Handling the
	// SMB2_CREATE_REQUEST_LEASE_V2 Create Context"). Used by the dir-lease
	// parent-key suppression rule: SET_INFO / WRITE / CLOSE-on-delete on this
	// handle MUST NOT break the parent dir lease whose LeaseKey matches this
	// value. That suppression is not stated in any MS-SMB2 server section —
	// §3.3.4.7 hands the break decision to the object store, and the only
	// spec text naming ParentLeaseKey outside the wire structures is the
	// client-side §3.2.4.3.8 — so Samba `dirlease_should_break` is the
	// binding reference for the rule itself. The field is
	// meaningful only when HasParentLeaseKey is true.
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
	// unlinked while this stream was still open. Per MS-FSA 2.1.5.5 ("Server Requests Closing an Open"), the
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

	// ReplayCreateGuid is the DH2Q CreateGuid carried by the originating
	// CREATE request, recorded whenever a DH2Q request context is present —
	// independent of whether V2 durability was actually granted. A replayed
	// CREATE (FLAGS_REPLAY_OPERATION) is keyed solely on the requested
	// CreateGuid per MS-SMB2 §3.3.5.9 (Samba smb2srv_open_lookup_replay_cache),
	// so a no-oplock / non-durable open (which never sets CreateGuid above)
	// must still be replay-cacheable. smbtorture
	// smb2.replay.dhv2-pending1n-vs-{oplock,lease}-sane replay io24 against an
	// open created with oplock_level=NONE and assert the same FileId comes back.
	ReplayCreateGuid [16]byte

	// IsPersistent indicates the handle was granted as a persistent durable
	// handle (DH2Q SMB2_DHANDLE_FLAG_PERSISTENT) on a continuous-availability
	// share. Persistent handles are a strict superset of durable handles
	// (IsDurable is also set); the distinction is that the DH2Q response
	// echoes the PERSISTENT flag and the grant is unconditional regardless of
	// oplock/lease level (MS-SMB2 §3.3.5.9.10). Only grantable on a CA share;
	// on a non-CA share a persistent request degrades to a plain durable grant
	// with this flag clear.
	IsPersistent bool

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
	//
	// It is the establishing GUID, so it is not what MS-SMB2 §3.3.5.9.13's
	// fourth AppInstanceId match condition names for a live open: that one is
	// the GUID of the connection the open's session is on now, which differs
	// after a reconnect from a different ClientGuid. ProcessAppInstanceId
	// resolves the current value through openClientGUID and uses this field
	// only for an open whose session is gone.
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
	// (MS-FSCC 2.4.40 (FilePositionInformation)). Servers track this per-handle so SET/GET via
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

func (f *OpenFile) Name() OpenName {
	if n := f.name.Load(); n != nil {
		return *n
	}
	return OpenName{}
}

func (f *OpenFile) SetName(n OpenName) {
	f.name.Store(&n)
}

func (f *OpenFile) WithName(n OpenName) *OpenFile {
	f.SetName(n)
	return f
}

func (f *OpenFile) OpenID() string {
	if f.cachedOpenID == "" {
		f.cachedOpenID = fmt.Sprintf("%x", f.FileID)
	}
	return f.cachedOpenID
}

func (f *OpenFile) Lock() { f.mu.Lock() }

func (f *OpenFile) Unlock() { f.mu.Unlock() }

func (f *OpenFile) RLock() { f.mu.RLock() }

func (f *OpenFile) RUnlock() { f.mu.RUnlock() }

func (f *OpenFile) IsAtimeFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.AtimeFrozen
}

func (f *OpenFile) GetRequestedAllocSize() uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.RequestedAllocSize
}

func (f *OpenFile) GetCreateOptions() CreateOptions {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.CreateOptions
}

func (f *OpenFile) GetPayloadID() metadata.PayloadID {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.PayloadID
}

func (f *OpenFile) GetMetadataHandle() metadata.FileHandle {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.MetadataHandle
}

func (f *OpenFile) SetPayloadID(id metadata.PayloadID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PayloadID = id
}

// SetDOCLeaving marks this handle as having run its delete-on-close election.
// IsDOCLeaving reports that mark.
//
// decision: neither takes f.mu. docLeaving is guarded by the handler's
// docElectionMu, which is held across the whole election including these
// calls; taking f.mu here would add a second lock to a field that already has
// one and invert the documented lock order (docElectionMu is a leaf). Move the
// guard only if the election stops being the sole writer.
func (f *OpenFile) SetDOCLeaving(v bool) { f.docLeaving = v }

// IsDOCLeaving reports whether this handle has already run its election.
func (f *OpenFile) IsDOCLeaving() bool { return f.docLeaving }

func (f *OpenFile) IsDeletePending() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.DeletePending
}

func (f *OpenFile) IsMtimeFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.MtimeFrozen
}

func (f *OpenFile) IsCtimeFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.CtimeFrozen
}

const notifyMaxBufferSizeSetBit uint64 = 1 << 32

func (f *OpenFile) CaptureNotifyMaxBufferSize(outputBufferLength uint32) (captured uint32, didCapture bool) {
	packed := notifyMaxBufferSizeSetBit | uint64(outputBufferLength)
	if f.NotifyMaxBufferSize.CompareAndSwap(0, packed) {
		return outputBufferLength, true
	}
	return uint32(f.NotifyMaxBufferSize.Load()), false
}

func (f *OpenFile) NotifyMaxBufferSizeValue() (value uint32, set bool) {
	raw := f.NotifyMaxBufferSize.Load()
	if raw&notifyMaxBufferSizeSetBit == 0 {
		return 0, false
	}
	return uint32(raw), true
}

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

// ChannelSequence returns the ChannelSequence the Open currently tracks. It
// exists for diagnostics: a channel-sequence test that fails needs the stored
// value in its message to be debuggable at all.
func (f *OpenFile) ChannelSequence() uint16 { return f.channelSeq }

// VerifyChannelSequence applies the MS-SMB2 §3.3.5.2.10 channel-sequence check
// to a request targeting this Open and advances the tracked sequence.
//
//   - reqCSN:   the ChannelSequence from the request header (low 16 bits of the
//     SMB2 ChannelSequence/Reserved field).
//   - isModify: whether the command is a modifying op (WRITE, SET_INFO, IOCTL).
//
// It returns false when the request must be rejected with
// STATUS_FILE_NOT_AVAILABLE; true when it may proceed.
//
// The SMB2_FLAGS_REPLAY_OPERATION flag does not change the decision: a replay
// of a stale modifying op is exactly the out-of-order resend the check exists
// to suppress, and a replay on the current/newer sequence is accepted on the
// same terms as a fresh request (see the package note on request_count).
func (f *OpenFile) VerifyChannelSequence(reqCSN uint16, isModify bool) bool {
	f.csMu.Lock()
	defer f.csMu.Unlock()

	// Seed the tracked sequence from the first request seen on this Open so an
	// initial nonzero ChannelSequence is not misread as a failover.
	if !f.channelSeqSet {
		f.channelSeq = reqCSN
		f.channelSeqSet = true
		return true
	}

	// Full-width difference between the request's ChannelSequence and the one
	// tracked for this Open, matching Samba: compute as plain integers (range
	// -65535..65535), then treat a magnitude greater than 0x7FFF as a 16-bit
	// wraparound of the client counter (an actual forward step) by flipping the
	// sign. cmp == 0 → same channel; cmp > 0 → newer channel (failover);
	// cmp < 0 → older/stale channel.
	cmp := int32(reqCSN) - int32(f.channelSeq)
	if cmp > 0x7FFF || cmp < -0x7FFF {
		cmp = -cmp
	}

	switch {
	case cmp == 0:
		return true
	case cmp > 0:
		f.channelSeq = reqCSN
		return true
	case isModify:
		// Stale ChannelSequence on a modifying op: reject.
		return false
	default:
		return true
	}
}
