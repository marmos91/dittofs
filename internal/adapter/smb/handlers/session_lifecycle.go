package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Session and tree lifecycle: session/tree lookup and creation, pending
// auth tracking, and the session/tree cleanup paths.
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
	// NegotiateMessage holds the client's Type-1 NEGOTIATE message bytes from
	// the first SESSION_SETUP of this handshake, and ChallengeMessage the
	// server's Type-2 CHALLENGE reply. Together with the Type-3 AUTHENTICATE
	// (MIC zeroed) they are the exact input to the AUTHENTICATE MIC check
	// (MS-NLMP 3.2.5.2.1). Nil when that message was not seen on this
	// pending-auth flow.
	NegotiateMessage []byte
	ChallengeMessage []byte
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
	// ContinuousAvailability mirrors the share-level toggle. When true, the
	// TREE_CONNECT response advertises SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY
	// (MS-SMB2 §2.2.10) and a DH2Q SMB2_DHANDLE_FLAG_PERSISTENT request is
	// granted as a persistent durable handle (#739, smbtorture
	// smb2.durable-v2-open.persistent-open-{oplock,lease}).
	ContinuousAvailability bool
	// AllowMFsymlink mirrors the share-level toggle. When false (default),
	// 1067-byte XSym files written by macOS/Windows clients are stored as
	// regular files. When true, they are converted to real symlinks on CLOSE.
	// The conversion target is client-controlled, so promotion is opt-in.
	AllowMFsymlink bool
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
// fields (FileID/TreeID/SessionID/MetadataHandle/CreateOptions) are safe to
// access without the mutex.
//
// PayloadID and the name triple are NOT immutable: the first WRITE on a file
// created empty caches the payload the metadata store allocated,
// SET_REPARSE_POINT and COPYCHUNK replace it, and SET_INFO rename rewrites the
// name/path/parent triple. Reach the payload through GetPayloadID /
// SetPayloadID and the triple through Name / SetName.

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
				"path", openFile.Name().Path,
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
	// Directory handles only. CHANGE_NOTIFY is rejected on anything else
	// (stub_handlers.go returns STATUS_INVALID_PARAMETER for a non-directory),
	// so a file or pipe handle can never carry a watch — and running the
	// notify completion for one would record a close tombstone nothing will
	// ever consume, making bulk teardown pay an O(n) tombstone sweep per
	// handle.
	var notifyDirs [][16]byte
	// docDirs holds the (share, path) of every directory handle this teardown
	// found carrying a delete-on-close. Marking a directory for deletion has to
	// complete the CHANGE_NOTIFY watches on it, and those normally live on
	// handles this teardown is not touching — see close.go step 9.
	var docDirs [][2]string
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

		// Delete-on-close decision. It runs here, ahead of everything that can
		// make this handle leave the open-file table — the durable persist
		// below removes it from h.files just as a full close does — because
		// the decision and this handle's departure from the set of handles
		// that can still honour a delete-on-close have to be one step. This
		// pass decides and the third pass below removes; a concurrent closer
		// scanning in between must not see a handle already written off, or
		// it defers the unlink to one that will never perform it. Shared with
		// close.go step 8; see doc_election.go. The unlink itself runs further
		// down, after the lock release and the cache flush.
		decision, docDelete := h.electDeleteOnClose(openFile)
		if decision != docDecisionNone && openFile.IsDirectory {
			docDirs = append(docDirs, [2]string{openFile.ShareName, openFile.Name().Path})
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
		// A non-none decision is exactly "this handle carries a delete-on-close",
		// read inside the election — so a DOC a concurrent closer propagated
		// onto this handle a moment ago is honoured rather than persisted away.
		hasDeleteOnClose := decision != docDecisionNone
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
				if state, epoch, found := h.LeaseManager.GetLeaseState(ctx, lock.FileHandle(openFile.MetadataHandle), openFile.ShareName, openFile.LeaseKey); found {
					leaseState = state
					leaseEpoch = epoch
				}
			}

			// MS-SMB2 §3.3.7.1 ("Handling Loss of a Connection") persist gate:
			// refuse to persist when the
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
					"path", openFile.Name().Path,
					"leaseState", fmt.Sprintf("0x%x", leaseState),
					"hasBRLocks", true)
			} else {
				// Serialize the persist against concurrent create-time
				// purge windows; see durablePurgeMu comment.
				h.durablePurgeMu.Lock()
				persisted := buildPersistedDurableHandle(openFile, username, sessionKeyHash, h.StartTime, leaseState, leaseEpoch)
				// Count the handle before the row becomes visible, so a
				// concurrent WRITE/SET_INFO cannot take its fast path over a
				// file that already has a disconnected handle. A failed Put is
				// deliberately not un-counted: it may still have written the
				// row, and the next scan of this file reconciles the count.
				h.noteDisconnectedHandle(persisted.MetadataHandle)
				err := h.DurableStore.PutDurableHandle(ctx, persisted)
				h.durablePurgeMu.Unlock()
				if err != nil {
					logger.Warn(caller+": failed to persist durable handle",
						"path", openFile.Name().Path,
						"error", err)
					// Fall through to normal close on persistence failure
				} else {
					logger.Debug(caller+": durable handle persisted for reconnect",
						"path", openFile.Name().Path,
						"fileID", fmt.Sprintf("%x", openFile.FileID),
						"timeout", openFile.DurableTimeoutMs)
					// Do NOT release locks, flush caches, or execute delete-on-close
					// The handle lives on in the DurableHandleStore
					toDelete = append(toDelete, openFile.FileID)
					if openFile.IsDirectory {
						notifyDirs = append(notifyDirs, openFile.FileID)
					}
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
		if !openFile.IsDirectory && openFile.GetPayloadID() != "" {
			h.flushFileCache(ctx, openFile)
		}

		// Execute the delete-on-close decided above. TDIS / LOGOFF / disconnect
		// skip the explicit CLOSE handler, so this is where the unlink happens
		// for them. The target was snapshotted by the election, so a rename
		// landing since cannot redirect it.
		if decision == docDecisionDelete && len(docDelete.ParentHandle) > 0 && docDelete.FileName != "" {
			h.handleDeleteOnClose(ctx, sess, openFile, docDelete, caller)
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
		if openFile.IsDirectory {
			notifyDirs = append(notifyDirs, openFile.FileID)
		}
		closed++
		return true
	})

	// Second pass: unregister pending CHANGE_NOTIFY watchers for the collected
	// handles. The CLOSE handler (close.go) does this for explicit closes, but
	// closeFilesWithFilter bypasses the CLOSE handler. Without this, stale
	// watchers persist in the NotifyRegistry after connection cleanup and can
	// fire during subsequent tests, sending async responses on dead connections
	// with partially-destroyed sessions. Per MS-SMB2 3.3.4.1: when the watched
	// handle goes away the pending request MUST complete with
	// STATUS_NOTIFY_CLEANUP so the client's async recv unblocks
	// (smb2.notify.tcon, .dir).
	//
	// This runs OUTSIDE renameScanMu — it touches only the NotifyRegistry, not
	// the `files` map a concurrent rename scan inspects — and never blocks on a
	// wait a concurrent CLOSE must satisfy, preserving the same deadlock-safety
	// argument as close.go's DrainHandleOps placement.
	if h.NotifyRegistry != nil {
		for _, fileID := range notifyDirs {
			for _, notify := range h.NotifyRegistry.CloseByFileID(fileID) {
				if notify.AsyncCallback == nil {
					continue
				}
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

		// Per [MS-FSA] 2.1.5.15.3 step 3.2.3.2: a directory marked for deletion completes
		// every pending CHANGE_NOTIFY on it with STATUS_DELETE_PENDING. Runs
		// after the loop above so a watch on a handle this teardown is closing
		// still gets the STATUS_NOTIFY_CLEANUP its own close owes it; what is
		// left are watches held by handles outside this teardown.
		for _, d := range docDirs {
			h.NotifyRegistry.CompleteWatchersForDeletePending(d[0], d[1])
		}
	}

	// Third pass: remove the collected handles from the `files` map and release
	// their per-handle lease/oplock records, all under renameScanMu.
	//
	// Lock rationale (mirrors close.go step 10/11): a concurrent SET_INFO
	// rename's post-break conflict re-scan reads the lock-free `files` map to
	// decide whether a conflicting holder still exists. If a teardown removed a
	// handle and then released its lease (which signals the rename's break-wait)
	// WITHOUT this mutex, the woken rename could observe the holder
	// half-removed — gone from `files` per the signal yet still mid-removal — or
	// the reverse, yielding a spurious STATUS_SHARING_VIOLATION. Holding
	// renameScanMu across the map removal + lease release makes the rename's
	// authoritative scan run entirely before or entirely after this teardown,
	// never interleaved. closeFilesWithFilter does no DrainHandleOps wait, so
	// nothing inside this section blocks on a wait a concurrent CLOSE must
	// satisfy; the mutex never nests under any LockManager/lease lock (the scan
	// touches only the sync.Map), so lock ordering stays cycle-free.
	//
	// The handleOps tracker is cleared outside the mutex: it is an independent
	// sync.Map the rename scan never reads, and the in-flight ops for a
	// teardown handle are not waited on here.
	//
	// releaseHandleLeaseRecord runs after every map removal so its "any other
	// open on the same file shares this key" scan sees the shrunk table —
	// otherwise sibling opens of the same file/key (all still present in the
	// first pass) would each defer to the other and the record would leak.
	h.renameScanMu.Lock()
	for _, fileID := range toDelete {
		h.deleteOpenFileEntry(fileID)
	}
	for _, openFile := range leaseReleases {
		h.releaseHandleLeaseRecord(ctx, openFile, caller)
	}
	h.renameScanMu.Unlock()

	// Clear the handleOps trackers for the removed handles. deleteOpenFileEntry
	// (used inside renameScanMu above, consistent with close.go) does not touch
	// handleOps, so do it here to avoid leaking trackers created by
	// BeginHandleOp on these FileIDs.
	for _, fileID := range toDelete {
		h.handleOps.Delete(string(fileID[:]))
	}

	if closed > 0 {
		logger.Debug(caller+": closed files", "sessionID", sessionID, "count", closed)
	}

	return closed
}

// purgeBlockStorePayload best-effort deletes a deleted file's block-store
// payload using that file's own stored PayloadID. Files created after #1166
// PR-3 get a UUID-based PayloadID (metadata.buildPayloadID), so a recreate at
// the same path now gets a fresh content_id and cannot collide with this
// file's bytes. This purge is still required to reclaim the deleted file's
// append-log/CAS state (otherwise its append log and tracked size would leak;
// historically a path-derived recreate could even read its stale bytes —
// e.g. a sparse hole surfacing non-zero bytes, smb2.ioctl.copy_chunk_sparse_dest).
// Callers invoke this only AFTER the metadata removal succeeds, so a block-store
// miss is harmless and GC reclaims any straggler CAS chunks; all errors are
// logged at Debug and swallowed.

func (h *Handler) purgeBlockStorePayload(ctx context.Context, handle metadata.FileHandle, payloadID metadata.PayloadID, path, caller string) {
	if payloadID == "" || len(handle) == 0 {
		return
	}
	blockStore, err := common.ResolveForWrite(ctx, h.Registry, handle)
	if err != nil {
		return
	}
	// Pass nil blocks: the block store resolves this payload's manifest and
	// reaps each row's refcount so the chunks become GC-eligible (#1433), in
	// addition to purging the append log + tracked size.
	if delErr := blockStore.Delete(ctx, string(payloadID), nil); delErr != nil {
		logger.Debug(caller+": block-store payload delete failed (non-fatal)",
			"path", path, "payloadID", payloadID, "error", delErr)
	}
}

// handleDeleteOnClose performs the delete operation for files marked with
// delete-on-close during session/tree/connection teardown.
//
// Two lease breaks fire after the removal, and only when it happened:
//
//  1. Strip Handle from other sessions' leases on the file that was
//     deleted (RH → R, RWH → RW). Handle caching is only stale once the
//     entry is actually gone — a delete-on-close that meets a non-empty
//     directory declines the removal (MS-FSA 2.1.5.5 phase 1 step 2.1.1)
//     and leaves every other holder's caching valid.
//
//  2. Break the parent directory's Handle and Read leases (content
//     change). Matches the explicit CLOSE path at close.go:334.
//
// Both breaks are async: the triggering SMB request (TDIS / LOGOFF / CLOSE /
// transport close) is on tree2/session2, while the lease holder is on a
// different session/tree on the same transport. Waiting for an ACK here
// would deadlock — the holder can only ack after the triggering request
// returns.

func (h *Handler) handleDeleteOnClose(ctx context.Context, sess *session.Session, openFile *OpenFile, target docTarget, caller string) {
	name := target.Name
	authCtx := h.buildCleanupAuthContext(ctx, sess)
	// Thread the closing handle's RqLs ParentLeaseKey so notifyDirChange can
	// apply the Samba `dirlease_should_break` parent-key
	// suppression rule on the parent dir lease. Suppression applies only when
	// the closer's key matches the key whoever committed the delete-on-close
	// recorded; when they differ every parent dir lease breaks. Same rule the
	// explicit CLOSE path applies (close.go step 8).
	docSetterKeysDiffer := target.HasDocSetterParentKey &&
		openFile.HasParentLeaseKey &&
		target.DocSetterParentKey != openFile.ParentLeaseKey
	if !docSetterKeysDiffer {
		PropagateOpenFileParentLeaseKey(authCtx, openFile)
	}
	// Remove what the election resolved — for a stream handle carrying a
	// base-file delete that is the base file, not the stream's own name —
	// through the shared helper CLOSE also uses, so the cascade to stream
	// siblings and the payload purge cannot drift between the two paths.
	// See doc_election.go.
	_, removed, err := h.removeElectedTarget(ctx, authCtx, openFile, target, caller)

	if err == nil && removed {
		if h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
			// Exclude the closing session: its leases on this file are about to
			// be released anyway, and firing self-breaks creates spurious
			// notifications that leak into later tests (observed regressing
			// smb2.lease.v1_bug15148 to count=2).
			excludeOwner := &lock.LockOwner{ClientID: fmt.Sprintf("smb:%d", openFile.SessionID)}
			if breakErr := h.LeaseManager.BreakFileHandleLeasesOnDelete(lockFileHandle, openFile.ShareName, excludeOwner); breakErr != nil {
				logger.Debug(caller+": file Handle lease break on delete failed", "path", name.Path, "error", breakErr)
			}
		}

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
	select {
	case <-h.cleanup.Idle():
	case <-time.After(3 * time.Second):
		logger.Warn("WaitForCleanup: timed out after 3s, proceeding with session setup")
	}
}

// SignalPendingCleanup registers count in-progress cleanups on the barrier.
// It MUST be called before any cleanup work begins — including draining the
// dying connection's in-flight requests — so WaitForCleanup() in a new
// session's SESSION_SETUP blocks until that cleanup is done. Every step
// arming happens after is a window in which WaitForCleanup sees an idle
// barrier while the old session's open files are still in the handle table.

func (h *Handler) SignalPendingCleanup(count int) {
	h.cleanup.Add(count)
}

// SignalCleanupDone retires one in-progress cleanup on the barrier. Used by
// the connection close path and by panic recovery in the cleanup loop, to
// release remaining slots when CleanupSession cannot be called (because it
// would call Done itself).

func (h *Handler) SignalCleanupDone() {
	h.cleanup.Done()
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
					p.releaseReplay()
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
// IMPORTANT: this consumes one cleanup-barrier count, retired from a defer so
// it is released on a panicking unwind too. The caller must have armed that
// count with SignalPendingCleanup before the call — one per session it is about
// to clean up — so the barrier is visible to new sessions for the whole span.

func (h *Handler) CleanupSession(ctx context.Context, sessionID uint64, isDisconnect bool) {
	defer h.cleanup.Done()

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
	payloadID := openFile.GetPayloadID()
	if payloadID == "" {
		return
	}
	// Snapshot once: a rename landing mid-flush would otherwise let the three
	// log lines below name different paths for the same operation.
	path := openFile.Name().Path

	blockStore, err := h.Registry.GetBlockStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("flushFileCache: block store not available for handle",
			"path", path,
			"error", err)
		return
	}

	// Use blocking Flush for immediate durability
	_, flushErr := blockStore.Flush(ctx, string(payloadID))
	if flushErr != nil {
		logger.Warn("flushFileCache: flush failed",
			"path", path,
			"payloadID", payloadID,
			"error", flushErr)
	} else {
		logger.Debug("flushFileCache: flushed",
			"path", path,
			"payloadID", payloadID)
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
		uid, gid := uidGIDFromSessionUser(sess.User)
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
