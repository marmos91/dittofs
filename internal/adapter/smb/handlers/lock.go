package handlers

import (
	"context"
	goerrors "errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// SMB2 LOCK Constants [MS-SMB2] 2.2.26
// ============================================================================

const (
	// SMB2LockFlagSharedLock requests a shared (read) lock.
	// Multiple clients can hold shared locks on overlapping ranges.
	SMB2LockFlagSharedLock uint32 = 0x00000001

	// SMB2LockFlagExclusiveLock requests an exclusive (write) lock.
	// Only one client can hold an exclusive lock on a range.
	SMB2LockFlagExclusiveLock uint32 = 0x00000002

	// SMB2LockFlagUnlock releases a previously acquired lock.
	SMB2LockFlagUnlock uint32 = 0x00000004

	// SMB2LockFlagFailImmediately means don't wait for the lock.
	// Return immediately if the lock cannot be acquired.
	// When this flag is NOT set, the server will retry acquiring the lock
	// for up to BlockingLockTimeout before giving up.
	SMB2LockFlagFailImmediately uint32 = 0x00000010

	// BlockingLockTimeout is the maximum time to wait for a blocking lock.
	// This is used when SMB2LockFlagFailImmediately is NOT set.
	BlockingLockTimeout = 5 * time.Second

	// BlockingLockRetryInterval is how often to retry acquiring a blocking lock.
	BlockingLockRetryInterval = 50 * time.Millisecond
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// LockRequest represents an SMB2 LOCK request [MS-SMB2] 2.2.26.
// The client specifies a FileID and an array of lock elements describing
// byte ranges to lock or unlock. Each lock element is 24 bytes.
type LockRequest struct {
	// LockCount is the number of lock elements in the request.
	LockCount uint16

	// LockSequence is used for lock sequence validation (SMB 2.1+).
	// Currently ignored in this implementation.
	LockSequence uint32

	// FileID is the SMB2 file identifier returned by CREATE.
	FileID [16]byte

	// Locks is the array of lock/unlock operations.
	Locks []LockElement
}

// LockElement represents a single lock or unlock operation [MS-SMB2] 2.2.26.
type LockElement struct {
	// Offset is the starting byte offset of the lock.
	Offset uint64

	// Length is the number of bytes to lock.
	// 0 means a zero-byte lock (never conflicts, SMB2 semantics).
	Length uint64

	// Flags specifies the lock type and behavior.
	// Combination of SMB2LockFlag* constants.
	Flags uint32
}

// LockResponse represents an SMB2 LOCK response [MS-SMB2] 2.2.27.
// The 4-byte response contains only a structure size and reserved field.
type LockResponse struct {
	SMBResponseBase
	StructureSize uint16
	Reserved      uint16
}

// ============================================================================
// Decode/Encode Functions
// ============================================================================

// DecodeLockRequest decodes an SMB2 LOCK request from binary data.
//
// Returns an error if the data is malformed or too small.
func DecodeLockRequest(body []byte) (*LockRequest, error) {
	// Minimum size: 24 bytes (header without lock elements)
	if len(body) < 24 {
		return nil, fmt.Errorf("lock request too small: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	structSize := r.ReadUint16()
	if structSize != 48 {
		return nil, fmt.Errorf("invalid lock structure size: %d (expected 48)", structSize)
	}

	req := &LockRequest{
		LockCount:    r.ReadUint16(),
		LockSequence: r.ReadUint32(),
	}
	copy(req.FileID[:], r.ReadBytes(16))
	if r.Err() != nil {
		return nil, fmt.Errorf("lock decode error: %w", r.Err())
	}

	// Validate and decode lock elements
	if req.LockCount == 0 {
		return nil, fmt.Errorf("lock count cannot be zero")
	}

	expectedSize := 24 + (int(req.LockCount) * 24)
	if len(body) < expectedSize {
		return nil, fmt.Errorf("lock request too small for %d locks: %d bytes (need %d)",
			req.LockCount, len(body), expectedSize)
	}

	req.Locks = make([]LockElement, int(req.LockCount))
	for i := 0; i < int(req.LockCount); i++ {
		req.Locks[i] = LockElement{
			Offset: r.ReadUint64(),
			Length: r.ReadUint64(),
			Flags:  r.ReadUint32(),
		}
		r.Skip(4) // Reserved
	}
	if r.Err() != nil {
		return nil, fmt.Errorf("lock element decode error: %w", r.Err())
	}

	return req, nil
}

// Encode serializes the LockResponse to binary data.
func (resp *LockResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(4)
	w.WriteUint16(4) // StructureSize
	w.WriteUint16(0) // Reserved
	if w.Err() != nil {
		return nil, w.Err()
	}
	return w.Bytes(), nil
}

// ============================================================================
// Handler Implementation
// ============================================================================

// Lock handles SMB2 LOCK command [MS-SMB2] 2.2.26, 2.2.27.
//
// This implements byte-range locking for SMB clients. Locks can be:
//   - Shared (read): Multiple clients can hold shared locks on overlapping ranges
//   - Exclusive (write): Only one client can hold an exclusive lock
//   - Unlock: Release a previously acquired lock
//
// Lock requests are processed atomically - if any lock in the request fails,
// all previously acquired locks in the same request are rolled back.
//
// Blocking LOCKs (single-element, no SMB2_LOCKFLAG_FAIL_IMMEDIATELY) that
// cannot be granted immediately are parked on the PendingLockRegistry and
// emit an interim STATUS_PENDING; the resume goroutine retries acquisition
// until success / timeout / cancellation, then delivers the final response
// asynchronously (MS-SMB2 §3.3.5.14, Samba `smbd_smb2_lock_send`). This
// prevents the dispatch goroutine from being blocked by a single client.
func (h *Handler) Lock(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Decode request
	req, err := DecodeLockRequest(body)
	if err != nil {
		logger.Debug("LOCK: decode error", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("LOCK request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"lockCount", req.LockCount,
		"sessionID", ctx.SessionID)

	// Get open file
	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("LOCK: file handle not found (closed)", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// SMB3 LOCK replay protection (MS-SMB2 §3.3.5.14 step 4).
	// LockSequence packs (LockSequenceIndex << 4 | LockSequenceNumber)
	// per MS-SMB2 §2.2.26 (low 4 bits = number, upper 28 bits = bucket).
	//
	// Per spec, the cache is consulted only when Open.IsResilient,
	// Open.IsDurable or Open.IsPersistent is TRUE, or when the
	// connection negotiated SMB2_GLOBAL_CAP_MULTI_CHANNEL. For all
	// other opens the LockSequence field is ignored (mirrors Samba
	// `smb2_lock_recv` `check_lock_sequence` gate). Without this gate
	// a basic LOCK stacking client (smbtorture lock.valid-request)
	// would observe its second same-sequence LOCK short-circuit out
	// of stacking and a subsequent UNLOCK would fail RANGE_NOT_LOCKED.
	checkLockSequence := openFile.IsDurable
	if !checkLockSequence && ctx.ConnCryptoState != nil {
		if ctx.ConnCryptoState.GetServerCapabilities()&types.CapMultiChannel != 0 {
			checkLockSequence = true
		}
	}
	lockSeqIndex, lockSeqNumber, lockSeqEnabled := UnpackLockSequence(req.LockSequence)
	lockSeqEnabled = lockSeqEnabled && checkLockSequence
	if lockSeqEnabled && h.LockReplayCache != nil {
		if cachedStatus, hit := h.LockReplayCache.Lookup(req.FileID, lockSeqIndex, lockSeqNumber); hit {
			logger.Debug("LOCK: replay hit — returning cached status",
				"fileID", fmt.Sprintf("%x", req.FileID),
				"index", lockSeqIndex,
				"number", lockSeqNumber,
				"status", cachedStatus)
			if cachedStatus != types.StatusSuccess {
				return NewErrorResult(cachedStatus), nil
			}
			respBytes, encErr := (&LockResponse{
				SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
				StructureSize:   4,
			}).Encode()
			if encErr != nil {
				logger.Error("LOCK: encode error on replay-hit success", "error", encErr)
				return NewErrorResult(types.StatusInternalError), nil
			}
			return NewResult(types.StatusSuccess, respBytes), nil
		}
	}

	// Pipes don't support locking
	if openFile.IsPipe {
		logger.Debug("LOCK: pipes don't support locking", "path", openFile.Path)
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Get metadata store
	metaSvc := h.Registry.GetMetadataService()

	// Prime ctx.User / IsGuest / TreeID from the OpenFile's recorded session
	// BEFORE BuildAuthContext — otherwise ctx.User==nil falls into the
	// anonymous arm and synthesises UID-0 (root), bypassing DACL checks on
	// the downstream lock/unlock metadata operations (#619, same class as #603).
	h.primeAuthContextFromOpenFile(ctx, openFile)

	// Build auth context
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("LOCK: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// First-element flag validation. Per MS-SMB2 §3.3.5.14 / Samba
	// smbd_smb2_lock_send (source3/smbd/smb2_lock.c): only five flag
	// combinations are valid for the first element. Anything else MUST
	// produce STATUS_INVALID_PARAMETER:
	//
	//   SHARED              (blocking shared, requires LockCount==1)
	//   EXCLUSIVE           (blocking exclusive, requires LockCount==1)
	//   SHARED | FAIL_IMM
	//   EXCLUSIVE | FAIL_IMM
	//   UNLOCK              (UNLOCK alone — FAIL_IMM is illegal here)
	//
	// Notably this rejects UNLOCK|FAIL_IMM and any unknown bits set
	// (smbtorture smb2.lock.valid-request lines 247-256).
	firstFlags := req.Locks[0].Flags
	isUnlockRequest := false
	switch firstFlags {
	case SMB2LockFlagSharedLock, SMB2LockFlagExclusiveLock:
		// Blocking lock — only allowed when LockCount==1.
		if req.LockCount > 1 {
			logger.Debug("LOCK: blocking first element requires single-element request")
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
	case SMB2LockFlagSharedLock | SMB2LockFlagFailImmediately,
		SMB2LockFlagExclusiveLock | SMB2LockFlagFailImmediately:
		// Non-blocking lock — multi-element request is allowed but every
		// subsequent element MUST also be non-blocking (checked below).
	case SMB2LockFlagUnlock:
		isUnlockRequest = true
	default:
		logger.Debug("LOCK: invalid first-element flags",
			"flags", fmt.Sprintf("0x%08X", firstFlags))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Multi-element validation. When the first element is a non-blocking
	// lock, every subsequent element MUST also carry FAIL_IMM (per Samba's
	// loop at smb2_lock.c:371). The UNLOCK-first deferred-invalid path is
	// enforced per-element in the main processing loop below (Samba's
	// "invalid" defer at smb2_lock.c:425).
	if req.LockCount > 1 && !isUnlockRequest {
		for i := 1; i < len(req.Locks); i++ {
			if (req.Locks[i].Flags & SMB2LockFlagFailImmediately) == 0 {
				logger.Debug("LOCK: multi-element request without FailImmediately",
					"elem", i)
				return NewErrorResult(types.StatusInvalidParameter), nil
			}
		}
	}

	// Session-expiry gate for new lock acquisition (MS-SMB2 §3.3.5.2.9). LOCK
	// is exempt from the dispatch-level expiry gate (prepareDispatch) so an
	// expired session can still release its held locks — smbtorture
	// smb2.session.expire2s/expire2e: "1st unlock => OK". A NEW lock on an
	// expired session must still be refused with STATUS_NETWORK_SESSION_EXPIRED
	// ("lock => EXPIRED"). UNLOCK requests proceed regardless of expiry.
	if !isUnlockRequest && ctx.SessionID != 0 {
		if sess, ok := h.GetSession(ctx.SessionID); ok && sess.IsExpired() {
			logger.Debug("LOCK: new lock on expired session refused",
				"sessionID", ctx.SessionID, "fileID", fmt.Sprintf("%x", req.FileID))
			return NewErrorResult(types.StatusNetworkSessionExpired), nil
		}
	}

	// ========================================================================
	// Break read-caching leases on other clients before acquiring locks
	// ========================================================================
	//
	// Per MS-SMB2 3.3.5.14 (Receiving an SMB2 LOCK Request) and Samba
	// `source3/smbd/smb2_oplock.c::contend_level2_oplocks_begin_default`:
	// granting a byte-range lock invalidates remote read caching, so every
	// lease (other than the locker's own, by lease key) that holds Read
	// caching must be broken to None — full revocation, not "strip W".
	// The break is fire-and-forget; the lock acquisition itself does not
	// wait for ACKs.
	//
	// Skip the break when every element is an unlock: Samba's
	// contend_level2_oplocks_begin is called from brl_lock only, not
	// brl_unlock. Releasing a lock cannot invalidate any remote read cache.

	hasRangeLockElement := false
	for _, e := range req.Locks {
		if (e.Flags&SMB2LockFlagUnlock) == 0 && e.Length > 0 {
			hasRangeLockElement = true
			break
		}
	}

	if hasRangeLockElement && h.LeaseManager != nil {
		lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
		if breakErr := h.LeaseManager.BreakLeasesOnByteRangeLock(lockFileHandle, openFile.ShareName, &lock.LockOwner{
			ExcludeLeaseKey: openFile.LeaseKey,
		}); breakErr != nil {
			logger.Debug("LOCK: lease break failed (non-fatal)", "path", openFile.Path, "error", breakErr)
		}
	}

	// Track acquired locks for rollback on failure
	var acquiredLocks []LockElement

	// Per-open lock ownership: use the open's unique ID for lock ownership
	openID := openFile.OpenID()

	// Process each lock element
	for i, lockElem := range req.Locks {
		isUnlock := (lockElem.Flags & SMB2LockFlagUnlock) != 0
		isExclusive := (lockElem.Flags & SMB2LockFlagExclusiveLock) != 0

		// Per-element flag validation, mirroring Samba's per-element switch
		// (source3/smbd/smb2_lock.c:382). For an UNLOCK-first request, any
		// element with invalid flags (a lock element OR a bare-UNLOCK with
		// extra bits set, e.g. UNLOCK|FAIL_IMM) is flagged as invalid but
		// deferred: the prior unlocks executed persist and the request
		// completes with INVALID_PARAMETER without processing this element.
		// For a lock-first request, a pure UNLOCK element is rejected
		// immediately.
		deferredInvalid := false
		switch lockElem.Flags {
		case SMB2LockFlagSharedLock, SMB2LockFlagExclusiveLock,
			SMB2LockFlagSharedLock | SMB2LockFlagFailImmediately,
			SMB2LockFlagExclusiveLock | SMB2LockFlagFailImmediately:
			if isUnlockRequest {
				// UNLOCK-first chain followed by a lock element: defer.
				deferredInvalid = true
			}
		case SMB2LockFlagUnlock:
			if !isUnlockRequest {
				logger.Debug("LOCK: UNLOCK element in lock-first request",
					"elem", i,
					"flags", fmt.Sprintf("0x%08X", lockElem.Flags))
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
				return NewErrorResult(types.StatusInvalidParameter), nil
			}
		default:
			if isUnlockRequest {
				// Defer: persist prior unlocks, error after. Critically we
				// must NOT process this element (its flags are invalid);
				// otherwise UNLOCK|FAIL_IMM would slip through the
				// isUnlock=true branch and run UnlockFile.
				deferredInvalid = true
			} else {
				logger.Debug("LOCK: invalid element flags",
					"elem", i,
					"flags", fmt.Sprintf("0x%08X", lockElem.Flags))
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
				return NewErrorResult(types.StatusInvalidParameter), nil
			}
		}

		if deferredInvalid {
			// Stop processing — prior unlocks persist, return error now.
			rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
			return NewErrorResult(types.StatusInvalidParameter), nil
		}

		// Per MS-SMB2 3.3.5.14: a range is invalid only when its last byte
		// (offset+length-1) overflows uint64. {offset=~0, length=1} addresses
		// exactly the byte at 2^64-1 and is valid; {offset=~0, length=2}
		// would address byte 2^64 which overflows. Empty (length=0) ranges
		// are always valid (zero-byte lock semantics handled downstream).
		if lockElem.Length > 0 && lockElem.Offset > ^uint64(0)-(lockElem.Length-1) {
			logger.Debug("LOCK: range overflow",
				"index", i,
				"offset", lockElem.Offset,
				"length", lockElem.Length)
			rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
			return NewErrorResult(types.StatusInvalidLockRange), nil
		}

		logger.Debug("LOCK: processing element",
			"index", i,
			"offset", lockElem.Offset,
			"length", lockElem.Length,
			"flags", fmt.Sprintf("0x%08X", lockElem.Flags),
			"unlock", isUnlock,
			"exclusive", isExclusive)

		if isUnlock {
			// Unlock operation.
			//
			// NOTE: Per SMB2 spec ([MS-SMB2] 2.2.26), successful unlock operations in a
			// compound/batched LOCK request are NOT rolled back if a subsequent lock
			// operation in the same batch fails. Only newly acquired locks are subject
			// to rollback.
			//
			// As a consequence, a multi-element LOCK request that includes unlocks is
			// not fully atomic with respect to the file's lock state: an unlock may
			// succeed and permanently release a range even if the overall LOCK request
			// eventually returns an error because of a later element in the batch.
			//
			// This handler intentionally preserves that SMB2-specified behavior. Callers
			// MUST NOT assume that all prior operations (particularly unlocks) are
			// reverted when a batched LOCK request fails.
			err := metaSvc.UnlockFile(
				authCtx.Context,
				openFile.MetadataHandle,
				openID,
				ctx.SessionID,
				lockElem.Offset,
				lockElem.Length,
			)
			if err != nil {
				logger.Debug("LOCK: unlock failed",
					"offset", lockElem.Offset,
					"length", lockElem.Length,
					"error", err)
				status := common.MapLockToSMB(err)
				// Rollback previously acquired locks (unlocks are not rolled back)
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
				return NewErrorResult(status), nil
			}
		} else {
			// Lock operation
			failImmediately := (lockElem.Flags & SMB2LockFlagFailImmediately) != 0

			fileLock := metadata.FileLock{
				ID:        0, // SMB doesn't use lock IDs in this implementation
				SessionID: ctx.SessionID,
				OpenID:    openID,
				// ClientID matches the identity SMB session teardown passes to
				// RemoveClientLocks ("smb:{SessionID}") so a disconnecting
				// client's persisted byte-range locks are purged rather than
				// resurrected on the next restart.
				ClientID:   fmt.Sprintf("smb:%d", ctx.SessionID),
				Offset:     lockElem.Offset,
				Length:     lockElem.Length,
				Exclusive:  isExclusive,
				IsZeroByte: lockElem.Length == 0,
				AcquiredAt: time.Now(),
				ClientAddr: ctx.ClientAddr,
			}

			// First attempt: synchronous, fail-fast. Covers the common case
			// of an uncontended lock (and is the only path for
			// FailImmediately requests).
			err := metaSvc.LockFile(authCtx, openFile.MetadataHandle, fileLock)
			if err == nil {
				acquiredLocks = append(acquiredLocks, lockElem)
				continue
			}

			// Non-conflict errors (NotFound, AccessDenied, …) abort the
			// request immediately, even for blocking requests.
			var storeErr *metadata.StoreError
			isLockConflict := goerrors.As(err, &storeErr) && storeErr.Code == merrs.ErrLocked
			if !isLockConflict {
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
				return NewErrorResult(common.MapLockToSMB(err)), nil
			}

			// Conflict path. FailImmediately → return LOCK_NOT_GRANTED.
			// Per MS-SMB2 §3.3.5.14, SMBv2 lock denials always surface
			// LOCK_NOT_GRANTED (FILE_LOCK_CONFLICT is reserved for the
			// I/O paths — see source4/torture/smb2/lock.c:1183-1186).
			if failImmediately {
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
				return NewErrorResult(types.StatusLockNotGranted), nil
			}

			// Blocking conflict. Try async parking first; if parking is
			// unavailable (compound chain, multi-element batch, deadlock,
			// async-credit pool exhausted, registry full), fall back to
			// inline retry on the dispatch goroutine.
			//
			// Async parking is unsafe in compound chains (NextCommand != 0,
			// MS-SMB2 §3.3.4.4) and in multi-element requests. The first
			// guard is enforced by the !ctx.NextCommand check below; the
			// second is implicit because §3.3.5.14 forbids multi-element
			// requests with a blocking first element (rejected above), and
			// we additionally require LockCount == 1 below.
			if ctx.NextCommand == 0 && req.LockCount == 1 && len(acquiredLocks) == 0 &&
				h.PendingLockRegistry != nil && ctx.AsyncLockCompleteCallback != nil &&
				ctx.TryReserveAsync != nil && ctx.ReleaseAsync != nil {
				if asyncId, parked := h.parkLockOnConflict(ctx, authCtx, openFile, fileLock, req.FileID, lockSeqEnabled, lockSeqIndex, lockSeqNumber); parked {
					return &HandlerResult{
						Status:  types.StatusPending,
						AsyncId: asyncId,
					}, nil
				}
			}

			// Fallback: inline retry. Register cancel context for SMB2_CANCEL.
			cancelCtx, cancelFn := context.WithCancel(authCtx.Context)
			lockAuthCtx := &metadata.AuthContext{
				Context:  cancelCtx,
				Identity: authCtx.Identity,
			}
			h.pendingLocks.Store(ctx.MessageID, cancelFn)

			err = h.acquireLockWithRetry(lockAuthCtx, metaSvc, openFile.MetadataHandle, fileLock, false)
			ctxErr := lockAuthCtx.Context.Err()
			h.pendingLocks.LoadAndDelete(ctx.MessageID)
			cancelFn()

			if err != nil {
				if ctxErr != nil {
					rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
					return NewErrorResult(types.StatusCancelled), nil
				}
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, openID, ctx.SessionID, acquiredLocks)
				// Lock conflict on retry → LOCK_NOT_GRANTED. Non-conflict
				// errors (e.g. file deleted while parked) flow through
				// common.MapLockToSMB.
				var retryStoreErr *metadata.StoreError
				if goerrors.As(err, &retryStoreErr) && retryStoreErr.Code == merrs.ErrLocked {
					return NewErrorResult(types.StatusLockNotGranted), nil
				}
				return NewErrorResult(common.MapLockToSMB(err)), nil
			}

			acquiredLocks = append(acquiredLocks, lockElem)
		}
	}

	logger.Debug("LOCK: success",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"lockCount", req.LockCount)

	// Mark this open as having held at least one byte-range lock. The flag
	// is consumed at disconnect by shouldPersistDurableOnDisconnect to refuse
	// durable persistence for opens that hold BR-locks under a non-W lease
	// (smbtorture smb2.durable-v2-open.lock-noW-lease, MS-SMB2 §3.3.4.18).
	// Strictly monotonic — UNLOCK does NOT clear it, mirroring Samba's
	// pessimistic `vfs_default_durable_disconnect` semantics.
	for _, le := range req.Locks {
		if le.Flags&SMB2LockFlagUnlock == 0 {
			openFile.HasByteRangeLocks.Store(true)
			break
		}
	}

	// Build response
	resp := &LockResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		StructureSize:   4,
		Reserved:        0,
	}

	respBytes, err := resp.Encode()
	if err != nil {
		logger.Error("LOCK: encode error", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	// Record success in the LOCK replay cache so a subsequent
	// FLAGS_REPLAY_OPERATION retry with the same (FileID, Index,
	// Number) returns this status instead of re-running the
	// acquire/release path (MS-SMB2 §3.3.5.14 step 4).
	if lockSeqEnabled && h.LockReplayCache != nil {
		h.LockReplayCache.Store(req.FileID, lockSeqIndex, lockSeqNumber, types.StatusSuccess)
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}

// acquireLockWithRetry attempts to acquire a lock, retrying for blocking requests.
//
// For fail-immediately requests (failImmediately=true), this returns immediately
// on conflict. For blocking requests, it retries up to BlockingLockTimeout.
//
// This implements a polling-based approach which, while not as efficient as a
// true event-driven wait, provides reasonable blocking lock semantics without
// requiring changes to the metadata store interface.
func (h *Handler) acquireLockWithRetry(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.Service,
	handle metadata.FileHandle,
	lock metadata.FileLock,
	failImmediately bool,
) error {
	// First attempt
	err := metaSvc.LockFile(authCtx, handle, lock)
	if err == nil {
		return nil
	}

	// Check if it's a lock conflict error. Use errors.As to unwrap wrapped
	// StoreErrors produced by production call paths (fmt.Errorf("...: %w", storeErr)).
	var storeErr *metadata.StoreError
	if !goerrors.As(err, &storeErr) || storeErr.Code != metadata.ErrLocked {
		// Not a lock conflict - return immediately
		return err
	}

	// For fail-immediately, return the error
	if failImmediately {
		return err
	}

	// Blocking lock request - retry until timeout
	logger.Debug("LOCK: blocking lock requested, will retry",
		"offset", lock.Offset,
		"length", lock.Length,
		"exclusive", lock.Exclusive,
		"timeout", BlockingLockTimeout)

	deadline := time.Now().Add(BlockingLockTimeout)
	ticker := time.NewTicker(BlockingLockRetryInterval)
	defer ticker.Stop()

	for {
		// Check remaining time before blocking to avoid overshooting the deadline.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			logger.Debug("LOCK: blocking lock timed out",
				"offset", lock.Offset,
				"length", lock.Length)
			return err // Return the original lock conflict error
		}

		select {
		case <-authCtx.Context.Done():
			// Context cancelled (e.g., client disconnected or CANCEL request)
			return authCtx.Context.Err()

		case <-ticker.C:
			// Update AcquiredAt for fresh timestamp
			lock.AcquiredAt = time.Now()

			// Try again
			err = metaSvc.LockFile(authCtx, handle, lock)
			if err == nil {
				logger.Debug("LOCK: blocking lock acquired after retry",
					"offset", lock.Offset,
					"length", lock.Length)
				return nil
			}

			// If it's not a lock conflict anymore, return the error.
			// Use errors.As to unwrap wrapped StoreErrors.
			var storeErr *metadata.StoreError
			if !goerrors.As(err, &storeErr) || storeErr.Code != metadata.ErrLocked {
				return err
			}
			// Still locked, continue retrying
		}
	}
}

// rollbackLocks releases locks that were acquired during a failed request.
//
// LIMITATION: Lock type changes (e.g., shared → exclusive on the same range by the
// same open) are implemented as in-place updates in the lock metaSvc. When such
// an "upgraded" lock is rolled back, it is completely removed rather than reverted
// to its original type. This means lock type changes in batch requests are not
// fully atomic: if a later operation in the batch fails, the lock type change
// persists as a removal rather than reverting to the previous lock type.
//
// This is an acceptable trade-off because:
//  1. Lock type changes in batched requests are rare in practice
//  2. Tracking original lock state would add significant complexity
//  3. The client can re-acquire the lock with the desired type if needed
func rollbackLocks(
	ctx context.Context,
	metaSvc *metadata.Service,
	handle metadata.FileHandle,
	openID string,
	sessionID uint64,
	locks []LockElement,
) {
	for _, lock := range locks {
		if err := metaSvc.UnlockFile(ctx, handle, openID, sessionID, lock.Offset, lock.Length); err != nil {
			logger.Warn("LOCK: rollback failed",
				"offset", lock.Offset,
				"length", lock.Length,
				"error", err)
		}
	}
}

// encodeLockResponseBody encodes the canonical 4-byte SMB2 LOCK response
// body (StructureSize=4, Reserved=0). Shared by the synchronous and
// async-park completion paths.
func encodeLockResponseBody() []byte {
	w := smbenc.NewWriter(4)
	w.WriteUint16(4) // StructureSize
	w.WriteUint16(0) // Reserved
	return w.Bytes()
}

// Note: lockErrorToStatus was consolidated into
// internal/adapter/common/lock_errmap.go. Callers now use
// common.MapLockToSMB — lock-context and general-context mappings are now
// driven by the same three-column tables used by NFSv3/NFSv4.
