package handlers

import (
	"fmt"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// SMB2 CHANGE_NOTIFY Constants [MS-SMB2] 2.2.35
// ============================================================================

// CompletionFilter flags specify which changes to watch for.
const (
	// FileNotifyChangeFileName watches for file name changes (create, delete, rename).
	FileNotifyChangeFileName uint32 = 0x00000001

	// FileNotifyChangeDirName watches for directory name changes.
	FileNotifyChangeDirName uint32 = 0x00000002

	// FileNotifyChangeAttributes watches for attribute changes.
	FileNotifyChangeAttributes uint32 = 0x00000004

	// FileNotifyChangeSize watches for file size changes.
	FileNotifyChangeSize uint32 = 0x00000008

	// FileNotifyChangeLastWrite watches for last write time changes.
	FileNotifyChangeLastWrite uint32 = 0x00000010

	// FileNotifyChangeLastAccess watches for last access time changes.
	FileNotifyChangeLastAccess uint32 = 0x00000020

	// FileNotifyChangeCreation watches for creation time changes.
	FileNotifyChangeCreation uint32 = 0x00000040

	// FileNotifyChangeEa watches for extended attribute changes.
	FileNotifyChangeEa uint32 = 0x00000080

	// FileNotifyChangeSecurity watches for security descriptor changes.
	FileNotifyChangeSecurity uint32 = 0x00000100

	// FileNotifyChangeStreamName watches for alternate data stream name changes
	// (create, delete, rename). [MS-SMB2] 2.2.35 / [MS-FSCC] 2.6.
	FileNotifyChangeStreamName uint32 = 0x00000200

	// FileNotifyChangeStreamSize watches for alternate data stream size changes.
	// [MS-SMB2] 2.2.35 / [MS-FSCC] 2.6.
	FileNotifyChangeStreamSize uint32 = 0x00000400

	// FileNotifyChangeStreamWrite watches for alternate data stream write changes.
	// [MS-SMB2] 2.2.35 / [MS-FSCC] 2.6.
	FileNotifyChangeStreamWrite uint32 = 0x00000800

	// AllValidCompletionFilterFlags is the bitmask of all recognized completion
	// filter flags per MS-SMB2 2.2.35. Used by MatchesFilter for event routing;
	// not used for request validation (any non-zero filter is accepted).
	AllValidCompletionFilterFlags uint32 = FileNotifyChangeFileName | FileNotifyChangeDirName |
		FileNotifyChangeAttributes | FileNotifyChangeSize | FileNotifyChangeLastWrite |
		FileNotifyChangeLastAccess | FileNotifyChangeCreation | FileNotifyChangeEa |
		FileNotifyChangeSecurity | FileNotifyChangeStreamName | FileNotifyChangeStreamSize |
		FileNotifyChangeStreamWrite
)

// Change action codes for FileNotifyInformation.
const (
	FileActionAdded          uint32 = 0x00000001
	FileActionRemoved        uint32 = 0x00000002
	FileActionModified       uint32 = 0x00000003
	FileActionRenamedOldName uint32 = 0x00000004
	FileActionRenamedNewName uint32 = 0x00000005
)

// Flags for CHANGE_NOTIFY request.
const (
	// SMB2WatchTree indicates recursive watching of subdirectories.
	SMB2WatchTree uint16 = 0x0001
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// ChangeNotifyRequest represents an SMB2 CHANGE_NOTIFY request [MS-SMB2] 2.2.35.
// Clients use this to register for directory change notifications.
// The server responds asynchronously when changes occur. The fixed wire
// format is 32 bytes.
type ChangeNotifyRequest struct {
	// Flags controls watch behavior.
	// SMB2_WATCH_TREE (0x0001) enables recursive watching.
	Flags uint16

	// OutputBufferLength is the maximum size of the response buffer.
	OutputBufferLength uint32

	// FileID is the directory handle to watch.
	FileID [16]byte

	// CompletionFilter specifies which changes trigger notifications.
	// Combination of FileNotifyChange* flags.
	CompletionFilter uint32
}

// ChangeNotifyResponse represents an SMB2 CHANGE_NOTIFY response [MS-SMB2] 2.2.36.
// Contains an array of FileNotifyInformation entries describing the changes.
type ChangeNotifyResponse struct {
	SMBResponseBase
	OutputBufferOffset uint16
	OutputBufferLength uint32
	Buffer             []byte // Serialized FileNotifyInformation array
}

// FileNotifyInformation represents a single change notification [MS-FSCC] 2.4.42.
type FileNotifyInformation struct {
	Action   uint32
	FileName string // Relative path within watched directory
}

// ============================================================================
// Pending Notify Registry
// ============================================================================

// AsyncResponseCallback is called when an async CHANGE_NOTIFY response is ready.
// The callback receives the session ID, message ID, async ID, and response data.
// The asyncId must match the one sent in the interim STATUS_PENDING response.
// Returns an error if the response could not be sent (e.g., connection closed).
type AsyncResponseCallback func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error

// OnOverflow is an optional hook fired when sendAndUnregister returns
// STATUS_NOTIFY_ENUM_DIR. The directory handle's overflow flag must be set
// so the next CHANGE_NOTIFY on the same handle also returns ENUM_DIR
// (smb2.notify.valid-req: "if the first notify returns NOTIFY_ENUM_DIR,
// all do"). Wired by the registering handler; nil for unit tests.
type OnOverflow func(fileID [16]byte)

// notifyFlushDelay is the accumulation window for buffered CHANGE_NOTIFY
// events. When the first event matches a live watcher, a timer starts;
// subsequent events from the same burst (e.g. REMOVED + ADDED + MODIFIED
// for an OVERWRITE/SUPERSEDE) append to the buffer. When the timer fires,
// all accumulated events are encoded into a single FILE_NOTIFY_INFORMATION
// response. Mirrors Samba's tevent-based deferred delivery.
const notifyFlushDelay = 5 * time.Millisecond

// PendingNotify tracks a pending CHANGE_NOTIFY request waiting for filesystem events.
// Each instance represents one client watch registered via the CHANGE_NOTIFY command.
// It stores the watch path, completion filter, and the async callback for delivering
// notifications. CHANGE_NOTIFY is one-shot: after a notification is sent, the watcher
// is unregistered and the client must re-issue the request for more notifications.
type PendingNotify struct {
	// Request identification
	FileID    [16]byte
	SessionID uint64
	// ConnID is the stable per-TCP-connection identifier on which this
	// CHANGE_NOTIFY arrived. Needed alongside MessageID because SMB2 scopes
	// MessageID to a single connection — two independent sessions on two
	// TCP connections will routinely pick the same MessageID values, and
	// keying the registry by MessageID alone silently evicts the earlier
	// pending notify when the later one registers (issue #416).
	ConnID    uint64
	MessageID uint64
	AsyncId   uint64 // Unique async ID for interim/final response correlation

	// Watch parameters
	WatchPath        string // Share-relative directory path
	ShareName        string
	TreeID           uint32
	CompletionFilter uint32
	WatchTree        bool // Recursive watching
	MaxOutputLength  uint32

	// AsyncCallback is called when a matching change is detected.
	// If nil, the change is logged but no response is sent.
	// The callback is responsible for sending the async SMB2 response.
	AsyncCallback AsyncResponseCallback

	// OnOverflow is fired when sendAndUnregister returns
	// STATUS_NOTIFY_ENUM_DIR so the open handle's sticky overflow flag can
	// be set. Optional.
	OnOverflow OnOverflow

	// bufferedChanges accumulates events during the notifyFlushDelay window.
	// Protected by NotifyRegistry.mu.
	bufferedChanges []FileNotifyInformation

	// flushTimer fires after notifyFlushDelay to deliver all buffered events.
	// nil when no events are buffered. Protected by NotifyRegistry.mu.
	flushTimer *time.Timer
}

// NotifyRegistry manages pending CHANGE_NOTIFY requests from SMB2 clients.
// It maps directory watch paths to pending notifications and supports both
// exact-path and recursive (WatchTree) matching. When a filesystem change
// occurs (via NotifyChange), it walks up the directory hierarchy to find
// matching watchers and delivers async responses via AsyncCallback.
// Thread-safe: all operations are protected by a read-write mutex.
type NotifyRegistry struct {
	mu       sync.RWMutex
	pending  map[string][]*PendingNotify // path -> pending requests
	byFileID map[string]*PendingNotify   // fileID string -> pending request
	// byMsgKey is keyed by (ConnID, MessageID) because MessageID is scoped
	// per TCP connection in SMB2. Keying by MessageID alone conflates
	// independent pending notifies from different connections and silently
	// evicts them on Register (issue #416).
	byMsgKey  map[notifyMsgKey]*PendingNotify
	byAsyncId map[uint64]*PendingNotify // asyncId -> pending request (for async CANCEL)

	// armed tracks directory handles that have at some point issued a
	// CHANGE_NOTIFY. Mirrors Samba `notify_buffer`: once a watcher has been
	// "armed" on a handle, subsequent matching filesystem events MUST be
	// counted (and their would-be encoded size accumulated) even when no
	// live PendingNotify is currently waiting. When the accumulated size
	// would exceed the watcher's last MaxOutputLength, OnOverflow fires so
	// the open handle's sticky-overflow flag is set; the next CHANGE_NOTIFY
	// on the handle then returns STATUS_NOTIFY_ENUM_DIR with zero output
	// (MS-SMB2 §3.3.5.19 / smb2.notify.overflow).
	//
	// Keyed by FileID string. Lifetime = handle: an entry survives across
	// CANCEL and one-shot completion of pending notifies, and is only torn
	// down by Disarm (called from CLOSE and from session/tree teardown).
	armed map[string]*armedHandle

	// cancelTombstones records an SMB2_CANCEL that arrived BEFORE the matching
	// CHANGE_NOTIFY had a chance to Register. Each request is dispatched on its
	// own goroutine (pkg/adapter/smb/connection.go), so a client that fires
	// CHANGE_NOTIFY immediately followed by CANCEL (smb2.notify.dir's "notify
	// cancel" subtest does exactly this) can race: the CANCEL handler runs
	// first, finds nothing in the registry, and returns. The CHANGE_NOTIFY
	// then registers a watcher that nobody will ever cancel — the test waits
	// forever for STATUS_CANCELLED and the whole notify suite times out at
	// 120s (issue #623).
	//
	// Tombstones are keyed by (ConnID, MessageID) — the only identifier the
	// client can reference before it has seen the server-assigned AsyncId.
	// They expire after cancelTombstoneTTL to bound memory; any later
	// matching Register short-circuits to STATUS_CANCELLED. AsyncId-flagged
	// CANCEL cannot race because the client only learns AsyncId from the
	// interim response, which is sent strictly AFTER Register has run.
	cancelTombstones map[notifyMsgKey]time.Time
}

// cancelTombstoneTTL bounds how long a pre-arrival SMB2_CANCEL is remembered
// when no matching CHANGE_NOTIFY ever registers (e.g. client cancelled a
// notify that the server rejected synchronously). Five seconds is generous
// enough for any plausible per-request goroutine scheduling delay while still
// expiring promptly if no Register ever happens.
const cancelTombstoneTTL = 5 * time.Second

// armedHandle records the buffered-events accounting for a single open
// directory handle that has ever armed a CHANGE_NOTIFY. See NotifyRegistry.armed.
type armedHandle struct {
	FileID           [16]byte
	SessionID        uint64
	ShareName        string
	TreeID           uint32
	WatchPath        string
	CompletionFilter uint32
	WatchTree        bool
	// MaxOutputLength is the most recent OutputBufferLength advertised by a
	// CHANGE_NOTIFY on this handle. The overflow threshold is recomputed
	// against this value on every event so a client that re-arms with a
	// larger buffer can keep buffering rather than overflow at the old cap.
	MaxOutputLength uint32
	// BufferedBytes is the running estimate of how many bytes a paired
	// FILE_NOTIFY_INFORMATION list of the buffered events would occupy.
	BufferedBytes uint32
	// OnOverflow is invoked exactly once (per arming) when BufferedBytes
	// exceeds MaxOutputLength. Wires the handle-level sticky NotifyOverflowed
	// flag — see OpenFile.NotifyOverflowed.
	OnOverflow OnOverflow
	// Overflowed is set on first overflow trip and prevents further
	// OnOverflow invocations until Disarm/ResetArmedOverflow.
	Overflowed bool
	// BufferedEvents accumulates events that arrived when no live watcher
	// was pending (one-shot consumed, client hasn't re-armed yet). Replayed
	// on the next Register — mirrors Samba's notify_buffer (smb2.notify.tcon).
	BufferedEvents []FileNotifyInformation
}

// notifyMsgKey identifies a pending notify by the tuple that uniquely names
// an SMB2 request: the per-connection ID plus the per-connection MessageID.
type notifyMsgKey struct {
	ConnID    uint64
	MessageID uint64
}

// NewNotifyRegistry creates a new notify registry.
func NewNotifyRegistry() *NotifyRegistry {
	return &NotifyRegistry{
		pending:          make(map[string][]*PendingNotify),
		byFileID:         make(map[string]*PendingNotify),
		byMsgKey:         make(map[notifyMsgKey]*PendingNotify),
		byAsyncId:        make(map[uint64]*PendingNotify),
		armed:            make(map[string]*armedHandle),
		cancelTombstones: make(map[notifyMsgKey]time.Time),
	}
}

// ErrAlreadyCancelled is returned from Register when a matching SMB2_CANCEL
// arrived before this CHANGE_NOTIFY could register (issue #623). The handler
// must respond with STATUS_CANCELLED synchronously instead of returning
// STATUS_PENDING.
var ErrAlreadyCancelled = fmt.Errorf("change_notify already cancelled before register")

// MarkPendingCancel records a tombstone for a CHANGE_NOTIFY that may not yet
// have registered. Called by the SMB2_CANCEL handler when its lookup by
// (ConnID, MessageID) finds nothing — the matching CHANGE_NOTIFY is likely
// being processed concurrently on another goroutine. If that NOTIFY then
// reaches Register, the tombstone causes it to return ErrAlreadyCancelled
// instead of registering and waiting forever (issue #623).
//
// Tombstones expire after cancelTombstoneTTL so a stray CANCEL for a notify
// that was already rejected synchronously (e.g. invalid filter) doesn't
// cancel a future unrelated CHANGE_NOTIFY that happens to reuse the same
// (ConnID, MessageID) much later.
func (r *NotifyRegistry) MarkPendingCancel(connID, messageID uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelTombstones[notifyMsgKey{ConnID: connID, MessageID: messageID}] = time.Now()
	r.gcCancelTombstonesLocked()
}

// gcCancelTombstonesLocked evicts expired tombstones. Must be called with
// r.mu held for write. Called opportunistically from MarkPendingCancel and
// Register so we don't need a background ticker; tombstones are only created
// in races and expire quickly even under high load.
func (r *NotifyRegistry) gcCancelTombstonesLocked() {
	now := time.Now()
	for k, ts := range r.cancelTombstones {
		if now.Sub(ts) > cancelTombstoneTTL {
			delete(r.cancelTombstones, k)
		}
	}
}

// consumeCancelTombstoneLocked checks for and removes a tombstone matching
// the given (ConnID, MessageID). Returns true if one was found (and the
// caller should treat the operation as cancelled). Must be called with r.mu
// held for write. Expired tombstones are treated as absent.
func (r *NotifyRegistry) consumeCancelTombstoneLocked(connID, messageID uint64) bool {
	key := notifyMsgKey{ConnID: connID, MessageID: messageID}
	ts, ok := r.cancelTombstones[key]
	if !ok {
		return false
	}
	delete(r.cancelTombstones, key)
	return time.Since(ts) <= cancelTombstoneTTL
}

// minNotifyEntryBytes is a lower-bound estimate of one encoded
// FILE_NOTIFY_INFORMATION entry: 12-byte fixed header + at least one UTF-16
// code unit + 4-byte alignment padding. Used to charge bytes against an
// armed handle's MaxOutputLength when an event arrives without a live
// watcher to encode against.
const minNotifyEntryBytes uint32 = 16

// armLocked records or refreshes the armed-handle entry for a pending
// notify. Must be called with r.mu held.
func (r *NotifyRegistry) armLocked(n *PendingNotify) {
	key := string(n.FileID[:])
	if existing, ok := r.armed[key]; ok {
		// Refresh routing fields (path/filter/recursive can change across
		// re-arms on the same handle, e.g. after a cancel) but preserve
		// BufferedBytes and Overflowed so events that arrived in the gap
		// still count.
		existing.SessionID = n.SessionID
		existing.ShareName = n.ShareName
		existing.TreeID = n.TreeID
		existing.WatchPath = n.WatchPath
		existing.CompletionFilter = n.CompletionFilter
		existing.WatchTree = n.WatchTree
		existing.MaxOutputLength = n.MaxOutputLength
		if n.OnOverflow != nil {
			existing.OnOverflow = n.OnOverflow
		}
		return
	}
	r.armed[key] = &armedHandle{
		FileID:           n.FileID,
		SessionID:        n.SessionID,
		ShareName:        n.ShareName,
		TreeID:           n.TreeID,
		WatchPath:        n.WatchPath,
		CompletionFilter: n.CompletionFilter,
		WatchTree:        n.WatchTree,
		MaxOutputLength:  n.MaxOutputLength,
		OnOverflow:       n.OnOverflow,
	}
}

// ClearBufferedEvents discards queued events on the armed handle for fileID.
// Called after CANCEL so the next Register doesn't replay stale events
// (smb2.notify.mask).
func (r *NotifyRegistry) ClearBufferedEvents(fileID [16]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.armed[string(fileID[:])]; ok {
		a.BufferedEvents = nil
	}
}

// Disarm tears down the buffered-event accounting for a directory handle.
// Called from CLOSE and from session/tree teardown. After Disarm, no further
// matching events will be counted against this handle until a new
// CHANGE_NOTIFY re-arms it. Returns true if an entry was removed.
func (r *NotifyRegistry) Disarm(fileID [16]byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := string(fileID[:])
	if _, ok := r.armed[key]; !ok {
		return false
	}
	delete(r.armed, key)
	return true
}

// ResetArmedOverflow clears the buffered-byte counter and overflow flag for
// a directory handle while keeping it armed. Called by the handler after
// it has consumed the sticky overflow flag and responded with
// STATUS_NOTIFY_ENUM_DIR — the next event must once again accumulate from
// zero against the freshly advertised MaxOutputLength.
func (r *NotifyRegistry) ResetArmedOverflow(fileID [16]byte, newMaxOutputLength uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.armed[string(fileID[:])]; ok {
		a.BufferedBytes = 0
		a.Overflowed = false
		a.MaxOutputLength = newMaxOutputLength
	}
}

// MaxPendingWatches is the maximum number of concurrent ChangeNotify watches
// allowed globally. Prevents memory exhaustion from clients registering
// unbounded watches without cancelling them.
const MaxPendingWatches = 4096

// ErrTooManyWatches is returned when the global watch limit is exceeded.
var ErrTooManyWatches = fmt.Errorf("too many pending ChangeNotify watches (max %d)", MaxPendingWatches)

// Register adds a pending notification request.
// If a request with the same FileID already exists, it is replaced.
// Returns ErrTooManyWatches if the global limit would be exceeded.
// Returns ErrAlreadyCancelled when an SMB2_CANCEL for the same
// (ConnID, MessageID) arrived before this Register call (issue #623); the
// caller MUST respond with STATUS_CANCELLED synchronously instead of
// emitting STATUS_PENDING.
func (r *NotifyRegistry) Register(notify *PendingNotify) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check for a CANCEL that arrived before us. Tombstones are exact:
	// a matching (ConnID, MessageID) means the client's CANCEL has already
	// been dispatched but couldn't find us yet. Short-circuit instead of
	// registering — the handler will emit STATUS_CANCELLED synchronously.
	if r.consumeCancelTombstoneLocked(notify.ConnID, notify.MessageID) {
		logger.Debug("NotifyRegistry: register short-circuited by pre-arrival CANCEL",
			"connID", notify.ConnID,
			"messageID", notify.MessageID,
			"path", notify.WatchPath)
		return ErrAlreadyCancelled
	}
	r.gcCancelTombstonesLocked()

	// If there's already a registration for this FileID, remove the old entry
	// to keep data structures consistent.
	if old, ok := r.byFileID[string(notify.FileID[:])]; ok {
		r.unregisterLocked(old)
	} else if len(r.byFileID) >= MaxPendingWatches {
		return ErrTooManyWatches
	}

	// Clean up any existing entry from the same (ConnID, MessageID) slot
	// with a different FileID. SMB2 MessageIDs are unique per connection,
	// so a same-connection collision means the client reused a MessageID
	// — a client bug we defensively recover from. Cross-connection
	// duplicates MUST NOT fall into this branch: they represent distinct
	// pending requests on independent TCP connections (issue #416).
	msgKey := notifyMsgKey{ConnID: notify.ConnID, MessageID: notify.MessageID}
	if oldByMsg, ok := r.byMsgKey[msgKey]; ok {
		if string(oldByMsg.FileID[:]) != string(notify.FileID[:]) {
			r.unregisterLocked(oldByMsg)
		}
	}

	r.byFileID[string(notify.FileID[:])] = notify
	r.byMsgKey[msgKey] = notify
	r.byAsyncId[notify.AsyncId] = notify
	r.pending[notify.WatchPath] = append(r.pending[notify.WatchPath], notify)

	// Arm (or refresh) the handle for buffered-event accounting. On re-arm,
	// MaxOutputLength is updated but BufferedBytes/Overflowed are preserved.
	r.armLocked(notify)

	// Replay events that arrived while no live watcher was pending
	// (goroutine-per-request race). The flush timer delivers them after
	// notifyFlushDelay; a racing CANCEL clears them before the timer fires.
	key := string(notify.FileID[:])
	if a, ok := r.armed[key]; ok && len(a.BufferedEvents) > 0 {
		for _, ev := range a.BufferedEvents {
			if !MatchesFilter(ev.Action, notify.CompletionFilter) {
				continue
			}
			// WatchTree is non-sticky — respect the current request's flag.
			// Events buffered while the armed handle was recursive may
			// include subdirectory entries; skip them for non-recursive.
			if !notify.WatchTree && strings.Contains(ev.FileName, "/") {
				continue
			}
			r.bufferEventLocked(notify, ev)
		}
		a.BufferedEvents = nil
	}

	logger.Debug("NotifyRegistry: registered watch",
		"path", notify.WatchPath,
		"filter", fmt.Sprintf("0x%08X", notify.CompletionFilter),
		"recursive", notify.WatchTree,
		"totalWatches", len(r.byFileID))

	return nil
}

// Unregister removes a pending notification by FileID.
// Called when the directory handle is closed or the request is cancelled.
func (r *NotifyRegistry) Unregister(fileID [16]byte) *PendingNotify {
	r.mu.Lock()
	defer r.mu.Unlock()

	notify, ok := r.byFileID[string(fileID[:])]
	if !ok {
		return nil
	}

	return r.unregisterLocked(notify)
}

// UnregisterByMessageID removes a pending notification by (ConnID, MessageID).
// Called by CANCEL when SMB2_FLAGS_ASYNC_COMMAND is not set on the cancel
// request (spec requires the server to match the original request's
// MessageID on its connection). Returns the removed PendingNotify, or nil
// if not found.
func (r *NotifyRegistry) UnregisterByMessageID(connID, messageID uint64) *PendingNotify {
	r.mu.Lock()
	defer r.mu.Unlock()

	notify, ok := r.byMsgKey[notifyMsgKey{ConnID: connID, MessageID: messageID}]
	if !ok {
		return nil
	}

	return r.unregisterLocked(notify)
}

// UnregisterByAsyncId removes a pending notification by AsyncId.
// Called by CANCEL when the client sends a cancel with SMB2_FLAGS_ASYNC_COMMAND.
// Returns the removed PendingNotify, or nil if not found.
func (r *NotifyRegistry) UnregisterByAsyncId(asyncId uint64) *PendingNotify {
	r.mu.Lock()
	defer r.mu.Unlock()

	notify, ok := r.byAsyncId[asyncId]
	if !ok {
		return nil
	}

	return r.unregisterLocked(notify)
}

// unregisterLocked removes a PendingNotify from all internal maps and cancels
// any pending flush timer. Must be called with r.mu held.
func (r *NotifyRegistry) unregisterLocked(notify *PendingNotify) *PendingNotify {
	if notify.flushTimer != nil {
		notify.flushTimer.Stop()
		notify.flushTimer = nil
	}

	fileIDKey := string(notify.FileID[:])
	delete(r.byFileID, fileIDKey)
	delete(r.byMsgKey, notifyMsgKey{ConnID: notify.ConnID, MessageID: notify.MessageID})
	delete(r.byAsyncId, notify.AsyncId)

	// Remove from pending path list
	pending := r.pending[notify.WatchPath]
	for i, p := range pending {
		if string(p.FileID[:]) == fileIDKey {
			r.pending[notify.WatchPath] = append(pending[:i], pending[i+1:]...)
			break
		}
	}
	if len(r.pending[notify.WatchPath]) == 0 {
		delete(r.pending, notify.WatchPath)
	}

	return notify
}

// GetWatchersForPath returns all pending notifies for a path.
// path should be the share-relative directory path.
func (r *NotifyRegistry) GetWatchersForPath(path string) []*PendingNotify {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Get exact match
	result := make([]*PendingNotify, len(r.pending[path]))
	copy(result, r.pending[path])

	return result
}

// Stream change action codes for ADS notifications.
const (
	FileActionAddedStream    uint32 = 0x00000006
	FileActionRemovedStream  uint32 = 0x00000007
	FileActionModifiedStream uint32 = 0x00000008
)

// MatchesFilter checks if a filesystem change action matches a CHANGE_NOTIFY
// completion filter [MS-SMB2] 2.2.35. It maps FileAction* constants to the
// corresponding FileNotifyChange* flags. For example, FileActionAdded matches
// FileNotifyChangeFileName and FileNotifyChangeDirName.
func MatchesFilter(action uint32, filter uint32) bool {
	switch action {
	case FileActionAdded, FileActionRemoved:
		// File/directory created or deleted; also matches stream name changes
		// (ADS create/delete fires FILE_ACTION_ADDED/REMOVED with stream name filter).
		return filter&(FileNotifyChangeFileName|FileNotifyChangeDirName|FileNotifyChangeStreamName) != 0
	case FileActionModified:
		// File modified — matches any content/metadata change filter,
		// including EA changes, security descriptor changes, and stream writes.
		return filter&(FileNotifyChangeSize|FileNotifyChangeLastWrite|FileNotifyChangeAttributes|FileNotifyChangeLastAccess|FileNotifyChangeCreation|FileNotifyChangeEa|FileNotifyChangeSecurity|FileNotifyChangeStreamSize|FileNotifyChangeStreamWrite) != 0
	case FileActionRenamedOldName, FileActionRenamedNewName:
		// Rename; also matches stream name changes (ADS rename).
		return filter&(FileNotifyChangeFileName|FileNotifyChangeDirName|FileNotifyChangeStreamName) != 0
	case FileActionAddedStream, FileActionRemovedStream:
		// ADS stream created or deleted
		return filter&FileNotifyChangeStreamName != 0
	case FileActionModifiedStream:
		// ADS stream modified
		return filter&(FileNotifyChangeStreamSize|FileNotifyChangeStreamWrite) != 0
	default:
		return false
	}
}

// ============================================================================
// Decode/Encode Functions
// ============================================================================

// DecodeChangeNotifyRequest parses an SMB2 CHANGE_NOTIFY request [MS-SMB2] 2.2.35
// from the wire format. The request body must be at least 32 bytes containing
// the structure size, flags, output buffer length, file ID, and completion filter.
// Returns an error if the body is too short or the structure size is invalid.
func DecodeChangeNotifyRequest(body []byte) (*ChangeNotifyRequest, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("CHANGE_NOTIFY request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	structSize := r.ReadUint16()
	if structSize != 32 {
		return nil, fmt.Errorf("invalid CHANGE_NOTIFY structure size: %d", structSize)
	}

	flags := r.ReadUint16()
	outputBufLen := r.ReadUint32()
	fileID := r.ReadBytes(16)
	completionFilter := r.ReadUint32()
	if r.Err() != nil {
		return nil, fmt.Errorf("CHANGE_NOTIFY parse error: %w", r.Err())
	}

	req := &ChangeNotifyRequest{
		Flags:              flags,
		OutputBufferLength: outputBufLen,
		CompletionFilter:   completionFilter,
	}
	copy(req.FileID[:], fileID)

	return req, nil
}

// Encode serializes the ChangeNotifyResponse to wire format [MS-SMB2] 2.2.36.
// OutputBufferOffset is set to 72 (header + fixed body) unconditionally,
// matching Samba and Windows reference behavior.
func (resp *ChangeNotifyResponse) Encode() ([]byte, error) {
	bufLen := len(resp.Buffer)
	w := smbenc.NewWriter(8 + max(bufLen, 1))
	w.WriteUint16(9)
	w.WriteUint16(72)
	w.WriteUint32(uint32(bufLen))
	w.WriteVariableSection(resp.Buffer)

	return w.Bytes(), w.Err()
}

// EncodeFileNotifyInformation encodes a list of change notifications.
// Uses proper UTF-16LE encoding to handle surrogate pairs for characters
// outside the Basic Multilingual Plane (codepoints > U+FFFF).
func EncodeFileNotifyInformation(changes []FileNotifyInformation) []byte {
	if len(changes) == 0 {
		return nil
	}

	// Pre-encode all filenames to UTF-16 to get accurate sizes
	encodedNames := make([][]uint16, len(changes))
	totalSize := 0
	for i, c := range changes {
		encodedNames[i] = utf16.Encode([]rune(c.FileName))
		// 12 bytes header + UTF-16LE filename (2 bytes per uint16)
		entrySize := 12 + len(encodedNames[i])*2
		// Align to 4 bytes
		if entrySize%4 != 0 {
			entrySize += 4 - (entrySize % 4)
		}
		totalSize += entrySize
	}

	w := smbenc.NewWriter(totalSize)

	for i, c := range changes {
		entryStart := w.Len()

		// Placeholder for NextEntryOffset (backpatched below)
		w.WriteUint32(0)

		// Action
		w.WriteUint32(c.Action)

		// FileNameLength (in bytes, UTF-16LE)
		nameLen := len(encodedNames[i]) * 2
		w.WriteUint32(uint32(nameLen))

		// FileName (UTF-16LE) - using pre-encoded UTF-16
		for _, u := range encodedNames[i] {
			w.WriteUint16(u)
		}

		// Align to 4 bytes
		w.Pad(4)

		// Backpatch NextEntryOffset (0 for last entry)
		if i < len(changes)-1 {
			nextOffsetBytes := smbenc.NewWriter(4)
			nextOffsetBytes.WriteUint32(uint32(w.Len() - entryStart))
			w.WriteAt(entryStart, nextOffsetBytes.Bytes())
		}
	}

	return w.Bytes()
}

// ============================================================================
// Notification Helpers
// ============================================================================

// NotifyChange records a filesystem change that may trigger pending CHANGE_NOTIFY
// requests. Events are buffered for notifyFlushDelay before delivery so that
// multiple events from the same burst (e.g. OVERWRITE -> REMOVED+ADDED+MODIFIED)
// are batched into a single response.
//
// filter specifies which FILE_NOTIFY_CHANGE_* flags this event matches. Only
// watchers whose CompletionFilter intersects filter are notified.
func (r *NotifyRegistry) NotifyChange(shareName, parentPath, fileName string, action uint32, filter uint32) {
	r.mu.Lock()
	watchers := r.findWatchersLocked(shareName, parentPath, filter)
	liveFileIDs := make(map[[16]byte]struct{}, len(watchers))
	for _, w := range watchers {
		liveFileIDs[w.notify.FileID] = struct{}{}
		relativePath := relativePathFromWatch(w.watchPath, parentPath, fileName)
		r.bufferEventLocked(w.notify, FileNotifyInformation{Action: action, FileName: relativePath})
	}
	// Buffer events on armed handles that have no live watcher so they
	// can be replayed when the first event after re-register arrives.
	for _, a := range r.armed {
		if a.ShareName != shareName || a.Overflowed {
			continue
		}
		if _, live := liveFileIDs[a.FileID]; live {
			continue
		}
		if a.WatchPath != parentPath {
			if !a.WatchTree || !pathIsAncestor(a.WatchPath, parentPath) {
				continue
			}
		}
		if a.CompletionFilter&filter == 0 {
			continue
		}
		relName := relativePathFromWatch(a.WatchPath, parentPath, fileName)
		a.BufferedEvents = append(a.BufferedEvents, FileNotifyInformation{Action: action, FileName: relName})
	}
	r.mu.Unlock()

	r.chargeArmedBuffer(shareName, parentPath, filter, []string{fileName}, liveFileIDs)
}

// NotifyRename records a rename as a paired FILE_ACTION_RENAMED_OLD_NAME /
// FILE_ACTION_RENAMED_NEW_NAME response. filter specifies the
// FILE_NOTIFY_CHANGE_* flags (typically FileName or DirName).
func (r *NotifyRegistry) NotifyRename(shareName, oldParentPath, oldFileName, newParentPath, newFileName string, filter uint32) {
	r.mu.Lock()

	oldWatchers := r.findWatchersLocked(shareName, oldParentPath, filter)

	var newWatchers []watcherMatch
	if newParentPath != oldParentPath {
		matchedFileIDs := make(map[[16]byte]struct{}, len(oldWatchers))
		for _, m := range oldWatchers {
			matchedFileIDs[m.notify.FileID] = struct{}{}
		}

		allNew := r.findWatchersLocked(shareName, newParentPath, filter)
		for _, m := range allNew {
			if _, alreadyMatched := matchedFileIDs[m.notify.FileID]; !alreadyMatched {
				newWatchers = append(newWatchers, m)
			}
		}
	}

	combined := append(oldWatchers, newWatchers...)
	liveFileIDs := make(map[[16]byte]struct{}, len(combined))
	for _, w := range combined {
		liveFileIDs[w.notify.FileID] = struct{}{}
		oldRelativePath := relativePathFromWatch(w.watchPath, oldParentPath, oldFileName)
		newRelativePath := relativePathFromWatch(w.watchPath, newParentPath, newFileName)
		r.bufferEventLocked(w.notify, FileNotifyInformation{Action: FileActionRenamedOldName, FileName: oldRelativePath})
		r.bufferEventLocked(w.notify, FileNotifyInformation{Action: FileActionRenamedNewName, FileName: newRelativePath})
	}
	// Buffer rename events on armed handles with no live watcher (mirrors
	// the same pattern in NotifyChange).
	for _, a := range r.armed {
		if a.ShareName != shareName || a.Overflowed {
			continue
		}
		if _, live := liveFileIDs[a.FileID]; live {
			continue
		}
		matchesOld := a.WatchPath == oldParentPath || (a.WatchTree && pathIsAncestor(a.WatchPath, oldParentPath))
		matchesNew := a.WatchPath == newParentPath || (a.WatchTree && pathIsAncestor(a.WatchPath, newParentPath))
		if !matchesOld && !matchesNew {
			continue
		}
		if a.CompletionFilter&filter == 0 {
			continue
		}
		oldRel := relativePathFromWatch(a.WatchPath, oldParentPath, oldFileName)
		newRel := relativePathFromWatch(a.WatchPath, newParentPath, newFileName)
		a.BufferedEvents = append(a.BufferedEvents,
			FileNotifyInformation{Action: FileActionRenamedOldName, FileName: oldRel},
			FileNotifyInformation{Action: FileActionRenamedNewName, FileName: newRel},
		)
	}
	r.mu.Unlock()

	r.chargeArmedBuffer(shareName, oldParentPath, filter, []string{oldFileName, newFileName}, liveFileIDs)
	if newParentPath != oldParentPath {
		r.chargeArmedBuffer(shareName, newParentPath, filter, []string{oldFileName, newFileName}, liveFileIDs)
	}
}

// chargeArmedBuffer charges the byte cost of a not-yet-delivered set of
// FILE_NOTIFY_INFORMATION entries against every armed handle whose
// (share, path, filter, WatchTree) tuple would have matched. Handles that
// already had a live waiter (passed via skipFileIDs) are excluded because
// the live path already delivered the event one-shot. If the running
// total crosses the handle's MaxOutputLength and the handle isn't already
// flagged, OnOverflow fires once and the entry's Overflowed flag latches
// until the next CHANGE_NOTIFY consumes the sticky bit (which calls
// ResetArmedOverflow).
//
// The encoded size is computed PER-WATCHER: each armed handle's FileName
// field carries a watcher-relative path (relativePathFromWatch), so a
// recursive watcher rooted at "/" sees "subdir/file.txt" while an exact
// watcher on "/subdir" sees "file.txt". Charging the bare fileName against
// every handle would systematically undercount for ancestor / WatchTree
// watchers (PR #613 Copilot review) and let the overflow latch later than
// a real FILE_NOTIFY_INFORMATION marshal — Samba's notify_marshall_changes
// (source3/smbd/notify.c) likewise marshals the stored per-watcher name,
// with UTF-16 length doubling and a 4-byte alignment pad.
func (r *NotifyRegistry) chargeArmedBuffer(
	shareName, parentPath string, filter uint32,
	candidateNames []string,
	skipFileIDs map[[16]byte]struct{},
) {
	type pendingFire struct {
		fn OnOverflow
		id [16]byte
	}
	var toFire []pendingFire
	r.mu.Lock()
	if len(r.armed) == 0 {
		r.mu.Unlock()
		return
	}
	for _, a := range r.armed {
		if a.ShareName != shareName {
			continue
		}
		if _, live := skipFileIDs[a.FileID]; live {
			continue
		}
		// Match path scope: exact match always counts; ancestor paths only
		// count when WatchTree is set.
		if a.WatchPath != parentPath {
			if !a.WatchTree {
				continue
			}
			if !pathIsAncestor(a.WatchPath, parentPath) {
				continue
			}
		}
		if a.CompletionFilter&filter == 0 {
			continue
		}
		if a.Overflowed {
			continue
		}
		// Size against THIS watcher's view: relativePathFromWatch is what
		// the real marshal would put on the wire for this handle, so it's
		// also what should accumulate toward MaxOutputLength.
		var addBytes uint32
		for _, name := range candidateNames {
			relName := relativePathFromWatch(a.WatchPath, parentPath, name)
			addBytes += encodedNotifyEntrySize(relName)
		}
		a.BufferedBytes += addBytes
		if a.MaxOutputLength == 0 || a.BufferedBytes > a.MaxOutputLength {
			a.Overflowed = true
			if a.OnOverflow != nil {
				toFire = append(toFire, pendingFire{fn: a.OnOverflow, id: a.FileID})
			}
		}
	}
	r.mu.Unlock()

	// Fire OnOverflow outside the lock — it touches OpenFile state through
	// the Handler and we don't want to nest those locks under r.mu.
	for _, f := range toFire {
		f.fn(f.id)
	}
}

// encodedNotifyEntrySize returns the wire size of a single
// FILE_NOTIFY_INFORMATION entry whose FileName is name (MS-FSCC §2.4.42):
// 12-byte fixed header (NextEntryOffset | Action | FileNameLength) plus the
// UTF-16LE filename bytes, aligned up to 4 bytes. Empty names are charged
// the minNotifyEntryBytes floor to avoid undercounting a sentinel-encoded
// entry on the wire.
func encodedNotifyEntrySize(name string) uint32 {
	nameBytes := 2 * uint32(len(utf16.Encode([]rune(name))))
	if nameBytes == 0 {
		nameBytes = 2
	}
	entry := 12 + nameBytes
	if entry%4 != 0 {
		entry += 4 - (entry % 4)
	}
	if entry < minNotifyEntryBytes {
		entry = minNotifyEntryBytes
	}
	return entry
}

// pathIsAncestor reports whether ancestor is a path-prefix of descendant
// at a directory boundary. "/" is treated as an ancestor of every path.
func pathIsAncestor(ancestor, descendant string) bool {
	if ancestor == "" || ancestor == "/" {
		return descendant != "" && descendant != "/"
	}
	if !strings.HasPrefix(descendant, ancestor) {
		return false
	}
	if len(descendant) == len(ancestor) {
		return false
	}
	return descendant[len(ancestor)] == '/'
}

// watcherMatch pairs a matched watcher with the watch path that matched it.
// The watchPath is needed to compute relative paths for notifications.
type watcherMatch struct {
	notify    *PendingNotify
	watchPath string // The path in the hierarchy where the watcher was found
}

// findWatchersLocked walks up the directory hierarchy from parentPath to find
// watchers whose CompletionFilter intersects the event's filter mask. Must be
// called with r.mu held (at least read-locked).
func (r *NotifyRegistry) findWatchersLocked(shareName, parentPath string, filter uint32) []watcherMatch {
	var matches []watcherMatch

	currentPath := parentPath
	for {
		for _, w := range r.pending[currentPath] {
			if w.ShareName != shareName {
				continue
			}
			if currentPath != parentPath && !w.WatchTree {
				continue
			}
			if w.CompletionFilter&filter == 0 {
				continue
			}
			matches = append(matches, watcherMatch{notify: w, watchPath: currentPath})
		}

		if currentPath == "/" || currentPath == "" {
			break
		}
		currentPath = GetParentPath(currentPath)
	}

	return matches
}

// bufferEventLocked appends a change to the watcher's buffer and starts the
// flush timer on the first event. Must be called with r.mu held for write.
func (r *NotifyRegistry) bufferEventLocked(notify *PendingNotify, change FileNotifyInformation) {
	notify.bufferedChanges = append(notify.bufferedChanges, change)
	if notify.flushTimer == nil {
		fileID := notify.FileID
		notify.flushTimer = time.AfterFunc(notifyFlushDelay, func() {
			r.flushWatcher(fileID)
		})
	}
}

// flushWatcher drains the buffered events for a watcher identified by fileID,
// unregisters it (one-shot), and sends the async response. Called by the flush
// timer after notifyFlushDelay.
func (r *NotifyRegistry) flushWatcher(fileID [16]byte) {
	r.mu.Lock()
	notify, ok := r.byFileID[string(fileID[:])]
	if !ok {
		r.mu.Unlock()
		return
	}
	changes := notify.bufferedChanges
	notify.bufferedChanges = nil
	notify.flushTimer = nil
	r.unregisterLocked(notify)
	r.mu.Unlock()

	r.deliverChanges(notify, changes)
}

// deliverChanges encodes and sends buffered events via the watcher's
// AsyncCallback. Called from flushWatcher (timer path) and FlushAll (test
// path). Must be called AFTER the watcher is unregistered — caller is
// responsible for one-shot semantics.
func (r *NotifyRegistry) deliverChanges(notify *PendingNotify, changes []FileNotifyInformation) {
	if notify.AsyncCallback == nil || len(changes) == 0 {
		return
	}

	buffer := EncodeFileNotifyInformation(changes)

	if uint32(len(buffer)) > notify.MaxOutputLength {
		logger.Warn("CHANGE_NOTIFY: flush exceeds MaxOutputLength; sending STATUS_NOTIFY_ENUM_DIR",
			"watchPath", notify.WatchPath,
			"numChanges", len(changes),
			"encodedLength", len(buffer),
			"maxOutputLength", notify.MaxOutputLength,
			"messageID", notify.MessageID)
		if notify.OnOverflow != nil {
			notify.OnOverflow(notify.FileID)
		}
		enumResp := &ChangeNotifyResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyEnumDir},
		}
		if err := notify.AsyncCallback(notify.SessionID, notify.MessageID, notify.AsyncId, enumResp); err != nil {
			logger.Warn("CHANGE_NOTIFY: failed to send STATUS_NOTIFY_ENUM_DIR",
				"messageID", notify.MessageID, "error", err)
		}
		return
	}

	response := &ChangeNotifyResponse{
		OutputBufferLength: uint32(len(buffer)),
		Buffer:             buffer,
	}

	logger.Debug("CHANGE_NOTIFY: flush — sending async response",
		"watchPath", notify.WatchPath,
		"numChanges", len(changes),
		"messageID", notify.MessageID,
		"sessionID", notify.SessionID)

	if err := notify.AsyncCallback(notify.SessionID, notify.MessageID, notify.AsyncId, response); err != nil {
		logger.Warn("CHANGE_NOTIFY: failed to send async response",
			"messageID", notify.MessageID, "error", err)
	}
}

// NotifyRmdir handles directory removal notification: send STATUS_NOTIFY_CLEANUP
// to any watchers on the removed directory itself, and notify the parent watcher
// with FileActionRemoved for the directory name.
//
// Per MS-SMB2 3.3.5.15: when a directory being watched is deleted, the pending
// CHANGE_NOTIFY request must be completed with STATUS_NOTIFY_CLEANUP.
func (r *NotifyRegistry) NotifyRmdir(shareName, parentPath, dirName string) {
	dirPath := path.Join(parentPath, dirName)

	// First: send STATUS_NOTIFY_CLEANUP to any watchers on the removed directory
	r.mu.Lock()
	var cleanupWatchers []*PendingNotify
	for _, w := range r.pending[dirPath] {
		if w.ShareName == shareName {
			cleanupWatchers = append(cleanupWatchers, w)
		}
	}
	// Remove them from the registry while holding the lock
	for _, w := range cleanupWatchers {
		r.unregisterLocked(w)
	}
	r.mu.Unlock()

	// Send STATUS_NOTIFY_CLEANUP to each removed watcher
	for _, w := range cleanupWatchers {
		if w.AsyncCallback != nil {
			cleanupResp := &ChangeNotifyResponse{
				SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyCleanup},
			}
			if err := w.AsyncCallback(w.SessionID, w.MessageID, w.AsyncId, cleanupResp); err != nil {
				logger.Warn("CHANGE_NOTIFY: failed to send STATUS_NOTIFY_CLEANUP for rmdir",
					"dirPath", dirPath,
					"messageID", w.MessageID,
					"error", err)
			}
		}
	}

	// Second: notify parent watchers about the directory removal
	r.NotifyChange(shareName, parentPath, dirName, FileActionRemoved, FileNotifyChangeDirName)
}

// UnregisterAllForSession unregisters all pending watchers for a session.
// Sends STATUS_NOTIFY_CLEANUP for each. Called during LOGOFF or session cleanup.
func (r *NotifyRegistry) UnregisterAllForSession(sessionID uint64) []*PendingNotify {
	r.mu.Lock()
	var toRemove []*PendingNotify
	for _, watchers := range r.pending {
		for _, w := range watchers {
			if w.SessionID == sessionID {
				toRemove = append(toRemove, w)
			}
		}
	}
	for _, w := range toRemove {
		r.unregisterLocked(w)
	}
	// Disarm any handles armed by this session so their buffered-event
	// accounting doesn't outlive the session that opened them.
	for key, a := range r.armed {
		if a.SessionID == sessionID {
			delete(r.armed, key)
		}
	}
	r.mu.Unlock()
	return toRemove
}

// UnregisterAllForTree unregisters all pending watchers for a specific tree
// connect (identified by sessionID + treeID). Uses TreeID rather than
// ShareName because multiple tree connects to the same share each get a
// distinct TreeID, and TREE_DISCONNECT must only tear down watchers opened
// through *that* tree connect — not watchers from a sibling tree connect to
// the same share (smb2.notify.tcon).
func (r *NotifyRegistry) UnregisterAllForTree(sessionID uint64, treeID uint32) []*PendingNotify {
	r.mu.Lock()
	var toRemove []*PendingNotify
	for _, watchers := range r.pending {
		for _, w := range watchers {
			if w.SessionID == sessionID && w.TreeID == treeID {
				toRemove = append(toRemove, w)
			}
		}
	}
	for _, w := range toRemove {
		r.unregisterLocked(w)
	}
	for key, a := range r.armed {
		if a.SessionID == sessionID && a.TreeID == treeID {
			delete(r.armed, key)
		}
	}
	r.mu.Unlock()
	return toRemove
}

// FlushAll stops all pending flush timers and immediately delivers buffered
// events for every watcher that has data. Drains buffers under the lock to
// prevent a TOCTOU race where a concurrent NotifyChange buffers an event
// between the timer stop and the flush. Used by unit tests.
func (r *NotifyRegistry) FlushAll() {
	type snapshot struct {
		notify  *PendingNotify
		changes []FileNotifyInformation
	}
	r.mu.Lock()
	var snaps []snapshot
	for _, notify := range r.byFileID {
		if len(notify.bufferedChanges) == 0 {
			continue
		}
		if notify.flushTimer != nil {
			notify.flushTimer.Stop()
			notify.flushTimer = nil
		}
		snaps = append(snaps, snapshot{
			notify:  notify,
			changes: notify.bufferedChanges,
		})
		notify.bufferedChanges = nil
		r.unregisterLocked(notify)
	}
	r.mu.Unlock()

	for _, s := range snaps {
		r.deliverChanges(s.notify, s.changes)
	}
}

// WatcherCount returns the total number of pending notify watchers.
// Used for state debugging instrumentation.
func (r *NotifyRegistry) WatcherCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byFileID)
}

// RangeWatchers iterates over all pending watchers, calling fn for each.
// Return false to stop iteration. Used for state debugging instrumentation.
func (r *NotifyRegistry) RangeWatchers(fn func(n *PendingNotify) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.byFileID {
		if !fn(n) {
			return
		}
	}
}

// ============================================================================
// Generalized Async Response Registry (D-21)
// ============================================================================
// AsyncResponseRegistry provides a general-purpose mechanism for tracking
// pending async operations. ChangeNotify is the primary consumer; future
// async operations (lock waits, etc.) can also use it.

// AsyncOperation tracks a pending async operation.
type AsyncOperation struct {
	AsyncId   uint64
	SessionID uint64
	MessageID uint64
	// Callback is invoked to send the async completion response.
	Callback func(sessionID, messageID, asyncId uint64, status types.Status, data []byte) error
}

// AsyncResponseRegistry tracks pending async operations by AsyncId.
// Thread-safe: all operations are protected by a read-write mutex.
type AsyncResponseRegistry struct {
	mu     sync.RWMutex
	ops    map[uint64]*AsyncOperation // asyncId -> operation
	maxOps int
}

// NewAsyncResponseRegistry creates a new async response registry.
func NewAsyncResponseRegistry(maxOps int) *AsyncResponseRegistry {
	return &AsyncResponseRegistry{
		ops:    make(map[uint64]*AsyncOperation),
		maxOps: maxOps,
	}
}

// Register adds a pending async operation. Returns error if limit exceeded.
func (r *AsyncResponseRegistry) Register(op *AsyncOperation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ops) >= r.maxOps {
		return fmt.Errorf("async response registry full (max %d)", r.maxOps)
	}
	r.ops[op.AsyncId] = op
	return nil
}

// Complete sends the completion response and removes the operation.
func (r *AsyncResponseRegistry) Complete(asyncId uint64, status types.Status, data []byte) error {
	r.mu.Lock()
	op, ok := r.ops[asyncId]
	if ok {
		delete(r.ops, asyncId)
	}
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("async operation %d not found", asyncId)
	}
	if op.Callback == nil {
		return nil
	}
	return op.Callback(op.SessionID, op.MessageID, asyncId, status, data)
}

// Cancel cancels a pending operation by sending STATUS_CANCELLED.
func (r *AsyncResponseRegistry) Cancel(asyncId uint64) error {
	return r.Complete(asyncId, types.StatusCancelled, nil)
}

// Unregister removes an operation without sending a response.
func (r *AsyncResponseRegistry) Unregister(asyncId uint64) {
	r.mu.Lock()
	delete(r.ops, asyncId)
	r.mu.Unlock()
}

// Len returns the number of pending operations.
func (r *AsyncResponseRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ops)
}

// IsValidCompletionFilter checks if the CompletionFilter is non-zero.
// Per MS-SMB2 3.3.5.15: if CompletionFilter is 0, return STATUS_INVALID_PARAMETER.
// Windows Server and Samba accept reserved/unknown bits — they simply never
// match any event. smb2.notify.mask iterates all 32 bit positions and expects
// each to be accepted (then cancelled).
func IsValidCompletionFilter(filter uint32) bool {
	return filter != 0
}

// relativePathFromWatch computes the relative path of a changed item from the
// perspective of a watch directory. If the change occurred in the same directory
// as the watch, it returns fileName unchanged. For changes in subdirectories
// (recursive watchers), it prepends the relative directory prefix.
//
// Examples:
//   - watchPath="/", parentPath="/subdir", fileName="file.txt" -> "subdir/file.txt"
//   - watchPath="/subdir", parentPath="/subdir", fileName="file.txt" -> "file.txt"
func relativePathFromWatch(watchPath, parentPath, fileName string) string {
	if watchPath == parentPath {
		return fileName
	}
	// Guard against cross-path calls (e.g., NotifyRename where the watcher
	// was found via newParentPath but we're computing the old name relative
	// to a different directory). Without this check, TrimPrefix is a no-op
	// and we'd return an incorrect path.
	if !strings.HasPrefix(parentPath, watchPath) {
		return fileName
	}
	relDir := strings.TrimPrefix(parentPath[len(watchPath):], "/")
	if relDir != "" {
		return relDir + "/" + fileName
	}
	return fileName
}

// GetParentPath returns the parent directory path from a full path.
// Examples:
//   - "/foo/bar/file.txt" -> "/foo/bar"
//   - "/file.txt" -> "/"
//   - "/" -> "/"
func GetParentPath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	parent := path.Dir(p)
	if parent == "." {
		return "/"
	}
	return parent
}

// GetFileName returns the file name from a full path.
// Examples:
//   - "/foo/bar/file.txt" -> "file.txt"
//   - "/file.txt" -> "file.txt"
//   - "/" -> ""
func GetFileName(p string) string {
	if p == "" || p == "/" {
		return ""
	}
	return path.Base(p)
}
