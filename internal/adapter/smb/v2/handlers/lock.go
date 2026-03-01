package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
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

	// Length is the number of bytes to lock (0 = to EOF).
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

	// Pipes don't support locking
	if openFile.IsPipe {
		logger.Debug("LOCK: pipes don't support locking", "path", openFile.Path)
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Get metadata store
	metaSvc := h.Registry.GetMetadataService()

	// Build auth context
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("LOCK: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Track acquired locks for rollback on failure
	var acquiredLocks []LockElement

	// Process each lock element
	for i, lockElem := range req.Locks {
		isUnlock := (lockElem.Flags & SMB2LockFlagUnlock) != 0
		isShared := (lockElem.Flags & SMB2LockFlagSharedLock) != 0
		isExclusive := (lockElem.Flags & SMB2LockFlagExclusiveLock) != 0

		// Validate flag combinations per MS-SMB2 2.2.26:
		// - SharedLock and ExclusiveLock are mutually exclusive
		// - Unlock must not be combined with lock type flags
		// - Lock operations must specify either SharedLock or ExclusiveLock
		if isShared && isExclusive {
			logger.Debug("LOCK: invalid flags - shared and exclusive both set",
				"index", i,
				"flags", fmt.Sprintf("0x%08X", lockElem.Flags))
			rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, ctx.SessionID, acquiredLocks)
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
		if isUnlock && (isShared || isExclusive) {
			logger.Debug("LOCK: invalid flags - unlock combined with lock type",
				"index", i,
				"flags", fmt.Sprintf("0x%08X", lockElem.Flags))
			rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, ctx.SessionID, acquiredLocks)
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
		if !isUnlock && !isShared && !isExclusive {
			logger.Debug("LOCK: invalid flags - lock operation without lock type",
				"index", i,
				"flags", fmt.Sprintf("0x%08X", lockElem.Flags))
			rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, ctx.SessionID, acquiredLocks)
			return NewErrorResult(types.StatusInvalidParameter), nil
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
				ctx.SessionID,
				lockElem.Offset,
				lockElem.Length,
			)
			if err != nil {
				logger.Debug("LOCK: unlock failed",
					"offset", lockElem.Offset,
					"length", lockElem.Length,
					"error", err)
				status := lockErrorToStatus(err)
				// Rollback previously acquired locks (unlocks are not rolled back)
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, ctx.SessionID, acquiredLocks)
				return NewErrorResult(status), nil
			}
		} else {
			// Lock operation
			failImmediately := (lockElem.Flags & SMB2LockFlagFailImmediately) != 0

			lock := metadata.FileLock{
				ID:         0, // SMB doesn't use lock IDs in this implementation
				SessionID:  ctx.SessionID,
				Offset:     lockElem.Offset,
				Length:     lockElem.Length,
				Exclusive:  isExclusive,
				AcquiredAt: time.Now(),
				ClientAddr: ctx.ClientAddr,
			}

			err := h.acquireLockWithRetry(authCtx, metaSvc, openFile.MetadataHandle, lock, failImmediately)
			if err != nil {
				logger.Debug("LOCK: lock failed",
					"offset", lockElem.Offset,
					"length", lockElem.Length,
					"exclusive", isExclusive,
					"failImmediately", failImmediately,
					"error", err)
				status := lockErrorToStatus(err)
				// Rollback previously acquired locks
				rollbackLocks(authCtx.Context, metaSvc, openFile.MetadataHandle, ctx.SessionID, acquiredLocks)
				return NewErrorResult(status), nil
			}

			// Track for potential rollback
			acquiredLocks = append(acquiredLocks, lockElem)
		}
	}

	logger.Debug("LOCK: success",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"lockCount", req.LockCount)

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
	metaSvc *metadata.MetadataService,
	handle metadata.FileHandle,
	lock metadata.FileLock,
	failImmediately bool,
) error {
	// First attempt
	err := metaSvc.LockFile(authCtx, handle, lock)
	if err == nil {
		return nil
	}

	// Check if it's a lock conflict error
	storeErr, isStoreErr := err.(*metadata.StoreError)
	if !isStoreErr || storeErr.Code != metadata.ErrLocked {
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

			// If it's not a lock conflict anymore, return the error
			storeErr, isStoreErr := err.(*metadata.StoreError)
			if !isStoreErr || storeErr.Code != metadata.ErrLocked {
				return err
			}
			// Still locked, continue retrying
		}
	}
}

// rollbackLocks releases locks that were acquired during a failed request.
//
// LIMITATION: Lock type changes (e.g., shared → exclusive on the same range by the
// same session) are implemented as in-place updates in the lock metaSvc. When such
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
	metaSvc *metadata.MetadataService,
	handle metadata.FileHandle,
	sessionID uint64,
	locks []LockElement,
) {
	for _, lock := range locks {
		if err := metaSvc.UnlockFile(ctx, handle, sessionID, lock.Offset, lock.Length); err != nil {
			logger.Warn("LOCK: rollback failed",
				"offset", lock.Offset,
				"length", lock.Length,
				"error", err)
		}
	}
}

// lockErrorToStatus converts a metadata store error to an SMB status code.
func lockErrorToStatus(err error) types.Status {
	if storeErr, ok := err.(*metadata.StoreError); ok {
		switch storeErr.Code {
		case metadata.ErrLocked:
			return types.StatusLockNotGranted
		case metadata.ErrLockNotFound:
			return types.StatusRangeNotLocked
		case metadata.ErrNotFound:
			return types.StatusFileClosed
		case metadata.ErrPermissionDenied:
			return types.StatusAccessDenied
		case metadata.ErrIsDirectory:
			return types.StatusFileIsADirectory
		}
	}
	return types.StatusInternalError
}
