package handlers

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Open-file state: the OpenFile type, its accessors and frozen-timestamp
// gates, the handle-op tracker, and the share-mode/delete-conflict checks.
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

// OpenName is the name triple of an open handle: full path, name within the
// parent, and parent directory handle. SET_INFO rename replaces all three at
// once, so they are published and read as one immutable value.

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
	h.DrainHandleOps(fileID)
	h.deleteOpenFileEntry(fileID)
}

// DrainHandleOps blocks until all in-flight operations registered on fileID
// (via BeginHandleOp / AcquireOpenFile) have completed, then drops the tracker.
// Split out of WaitAndDeleteOpenFile so the CLOSE handler can drain the closing
// handle's in-flight ops OUTSIDE renameScanMu (the drain can be unbounded if a
// slow QueryDirectory is in flight) and take the mutex only for the actual
// map removal + lease release/signal — see close.go step 10/11.

func (h *Handler) DrainHandleOps(fileID [16]byte) {
	key := string(fileID[:])
	if v, ok := h.handleOps.Load(key); ok {
		v.(*handleOpTracker).wg.Wait()
		h.handleOps.Delete(key)
	}
}

// deleteOpenFileEntry removes the OpenFile from the files map and revokes its
// replay/resume state. The caller is responsible for any draining (see
// DrainHandleOps). The CLOSE path holds renameScanMu across this call so a
// concurrent rename's conflict re-scan cannot observe a half-removed handle.

func (h *Handler) deleteOpenFileEntry(fileID [16]byte) {
	key := string(fileID[:])
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
			// Forget by the replay-cache key (the requested CreateGuid),
			// which is set even for non-durable opens that never populate
			// CreateGuid. Falls back to CreateGuid for older code paths.
			of := v.(*OpenFile)
			guid := of.ReplayCreateGuid
			if guid == ([16]byte{}) {
				guid = of.CreateGuid
			}
			if guid != ([16]byte{}) {
				h.CreateReplayCache.Forget(guid)
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
		// DeletePending is concurrently written by CLOSE DOC propagation under
		// existing.mu — read it under the read lock.
		existing.mu.RLock()
		dp := existing.DeletePending
		existing.mu.RUnlock()
		if dp {
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
// The open is identified by its resolved parent directory handle and its
// parent-relative name (e.g. "file" or "file:Stream One"), not by its full
// path. A base file and its streams are siblings in one directory, so that
// pair relates them by identity; the full path cannot, because it reproduces
// whatever spelling the client sent.

func (h *Handler) isFileOrBaseDeletePending(
	fileHandle metadata.FileHandle,
	parentHandle metadata.FileHandle,
	fileName string,
) bool {
	// Fast path: direct metadata-handle match against an existing handle
	// whose own DeletePending is set. Covers the same-file re-open case
	// (smbtorture smb2.oplock.doc, smb2.streams.delete).
	if h.isFileDeletePending(fileHandle) {
		return true
	}

	openBase := adsBaseName(fileName) // non-empty if fileName is a stream
	pending := false
	h.files.Range(func(_, value any) bool {
		existing := value.(*OpenFile)
		if existing.IsPipe || len(existing.MetadataHandle) == 0 {
			return true
		}
		// BaseFileDeletePending is concurrently written by CLOSE deferred-delete
		// propagation under existing.mu — read it under the read lock.
		existing.mu.RLock()
		bdp := existing.BaseFileDeletePending
		existing.mu.RUnlock()
		if !bdp {
			return true
		}
		existingName := existing.Name()
		if !bytes.Equal(existingName.ParentHandle, parentHandle) {
			return true
		}
		existingBase := adsBaseName(existingName.FileName)
		if openBase == "" {
			// Opening a base file: match against any stream of this base.
			if strings.EqualFold(existingBase, fileName) {
				pending = true
				return false
			}
		} else {
			// Opening a stream: match against a sibling stream sharing the
			// same base name, or against a base-file handle of that base.
			if strings.EqualFold(existingBase, openBase) ||
				strings.EqualFold(existingName.FileName, openBase) {
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
// streams. Per MS-FSA 2.1.5.1.2.2 ("Algorithm to Check Sharing Access to an Existing Stream or Directory") + Samba semantics, share mode enforcement is:
//   - Same stream (same metadata handle) → always checked
//   - Base file vs its stream (or vice versa) → checked
//   - Stream A vs Stream B (different streams, same base) → NOT checked
//
// Returns true if a conflict exists (CREATE should fail with STATUS_SHARING_VIOLATION).
// The open is identified by its resolved parent directory handle and its
// parent-relative name, for the reason given on isFileOrBaseDeletePending.

func (h *Handler) checkShareModeConflict(
	fileHandle metadata.FileHandle,
	newDesiredAccess, newShareAccess uint32,
	parentHandle metadata.FileHandle,
	fileName string,
) bool {
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
	// MAXIMUM_ALLOWED resolves to the maximal granted rights, which include
	// write+delete on a writable handle. Samba's share_conflict evaluates the
	// resolved effective mask; DittoFS keeps the raw 0x02000000 bit in
	// OpenFile.DesiredAccess (ExpandGenericMask strips GENERIC_* but not
	// MAXIMUM_ALLOWED), so it must be treated as write/delete here too —
	// otherwise a MAXIMUM_ALLOWED opener is wrongly treated as read-only and
	// bypasses SHARE_WRITE / SHARE_DELETE enforcement (matches hasRead above and
	// the module-level hasWriteAccess/hasDeleteAccess).
	hasWrite := func(access uint32) bool {
		return access&(fileWriteData|fileAppendData|genericWrite|genericAll|maxAllowed) != 0
	}
	// Helper: does access mask imply delete?
	hasDelete := func(access uint32) bool {
		return access&(deleteAccess|genericAll|maxAllowed) != 0
	}

	newBase := adsBaseName(fileName)

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
			existingName := existing.Name()
			// A base file and its streams live in one directory; a handle
			// anywhere else cannot be related to this open.
			if !bytes.Equal(existingName.ParentHandle, parentHandle) {
				return true
			}
			existingBase := adsBaseName(existingName.FileName)
			baseVsStream := false
			if newBase == "" && existingBase != "" {
				baseVsStream = strings.EqualFold(existingBase, fileName)
			} else if newBase != "" && existingBase == "" {
				baseVsStream = strings.EqualFold(newBase, existingName.FileName)
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
	metaSvc *metadata.Service,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.File, string, error) {
	return metaSvc.LookupCaseInsensitive(authCtx, parentHandle, name)
}

// adsBaseName extracts the base file name from a potentially ADS-qualified
// parent-relative name. For "file.txt:stream" it returns "file.txt"; for
// "file.txt" (not a stream) it returns "".
//
// Stream names cannot contain a path separator (rejected at CREATE), so this
// operates on a single name component, never a path.

func (h *Handler) checkShareDeleteConflict(renameFile *OpenFile) bool {
	const fileShareDelete = uint32(0x04) // FILE_SHARE_DELETE

	var culprit *OpenFile
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
			culprit = other
			return false // Stop iterating
		}
		return true
	})
	if culprit != nil {
		// #1652: dump the offending holder so a spurious/intermittent
		// SHARING_VIOLATION on rename is diagnosable — the call-site log only
		// records the renamer. The conflict is expected only when a live
		// sibling open lacks FILE_SHARE_DELETE; a holder on a different
		// session/tree, a durable handle, or a delete-pending stub pointing
		// here is the fingerprint of a leaked/stale open.
		logRenameConflictHolder("source-file share-delete gate", renameFile, culprit)
		return true
	}
	return false
}

// logRenameConflictHolder emits (at Debug) the full identity of the open handle
// that tripped a rename share-mode gate, alongside the renamer. Fields chosen to
// answer "is this a legitimate live sibling, or a stale/cross-connection leak?":
// session/tree locate the owning connection, ShareAccess/DesiredAccess show why
// it conflicted, IsDurable/DeletePending flag reconnect/teardown stubs. #1652.

func (h *Handler) checkParentDirRenameConflict(renamer *OpenFile, dstParent metadata.FileHandle) bool {
	if len(dstParent) == 0 {
		return false
	}
	var culprit *OpenFile
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == renamer.FileID {
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
		// rename to succeed; without this filter its lack of FILE_SHARE_WRITE
		// would falsely trip the conflict.
		if isStatOnlyOpen(other.DesiredAccess) {
			return true
		}
		if other.ShareAccess&smbShareWrite == 0 || hasDeleteAccess(other.DesiredAccess) {
			culprit = other
			return false
		}
		return true
	})
	if culprit != nil {
		logRenameConflictHolder("dst-parent share-mode gate", renamer, culprit)
		return true
	}
	return false
}

// snapshotOpenChildren returns the metadata handles of every open file whose
// ParentHandle equals dirHandle. Caller must read h.files only once; iterating
// twice could observe inconsistent open state across a concurrent CLOSE.

func (h *Handler) snapshotOpenChildren(dirHandle metadata.FileHandle) []metadata.FileHandle {
	var children []metadata.FileHandle
	h.files.Range(func(_, value any) bool {
		of := value.(*OpenFile)
		parent := of.Name().ParentHandle
		if len(parent) == 0 || len(of.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(parent, dirHandle) {
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
		parent := of.Name().ParentHandle
		if len(parent) == 0 {
			return true
		}
		if !bytes.Equal(parent, dirHandle) {
			return true
		}
		open = true
		return false
	})
	return open
}

// hasOpenHandleOnFile reports whether any open file handle (other than the
// renamer's own handle) currently references targetMeta. Used by the
// SET_INFO FileRenameInformation handler to enforce MS-FSA §2.1.5.15.12 ("FileRenameInformation")
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
