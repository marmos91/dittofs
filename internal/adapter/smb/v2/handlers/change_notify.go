package handlers

import (
	"fmt"
	"path"
	"strings"
	"sync"
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

	// AllValidCompletionFilterFlags is the bitmask of all valid completion filter flags.
	// Used to validate CHANGE_NOTIFY requests per MS-SMB2 3.3.5.15.
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

// PendingNotify tracks a pending CHANGE_NOTIFY request waiting for filesystem events.
// Each instance represents one client watch registered via the CHANGE_NOTIFY command.
// It stores the watch path, completion filter, and the async callback for delivering
// notifications. CHANGE_NOTIFY is one-shot: after a notification is sent, the watcher
// is unregistered and the client must re-issue the request for more notifications.
type PendingNotify struct {
	// Request identification
	FileID    [16]byte
	SessionID uint64
	MessageID uint64
	AsyncId   uint64 // Unique async ID for interim/final response correlation

	// Watch parameters
	WatchPath        string // Share-relative directory path
	ShareName        string
	CompletionFilter uint32
	WatchTree        bool // Recursive watching
	MaxOutputLength  uint32

	// AsyncCallback is called when a matching change is detected.
	// If nil, the change is logged but no response is sent.
	// The callback is responsible for sending the async SMB2 response.
	AsyncCallback AsyncResponseCallback
}

// NotifyRegistry manages pending CHANGE_NOTIFY requests from SMB2 clients.
// It maps directory watch paths to pending notifications and supports both
// exact-path and recursive (WatchTree) matching. When a filesystem change
// occurs (via NotifyChange), it walks up the directory hierarchy to find
// matching watchers and delivers async responses via AsyncCallback.
// Thread-safe: all operations are protected by a read-write mutex.
type NotifyRegistry struct {
	mu          sync.RWMutex
	pending     map[string][]*PendingNotify // path -> pending requests
	byFileID    map[string]*PendingNotify   // fileID string -> pending request
	byMessageID map[uint64]*PendingNotify   // messageID -> pending request (for CANCEL)
	byAsyncId   map[uint64]*PendingNotify   // asyncId -> pending request (for async CANCEL)
}

// NewNotifyRegistry creates a new notify registry.
func NewNotifyRegistry() *NotifyRegistry {
	return &NotifyRegistry{
		pending:     make(map[string][]*PendingNotify),
		byFileID:    make(map[string]*PendingNotify),
		byMessageID: make(map[uint64]*PendingNotify),
		byAsyncId:   make(map[uint64]*PendingNotify),
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
func (r *NotifyRegistry) Register(notify *PendingNotify) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// If there's already a registration for this FileID, remove the old entry
	// to keep data structures consistent.
	if old, ok := r.byFileID[string(notify.FileID[:])]; ok {
		r.unregisterLocked(old)
	} else if len(r.byFileID) >= MaxPendingWatches {
		return ErrTooManyWatches
	}

	// Clean up any existing entry with the same MessageID to prevent orphans
	// in byFileID/pending (a misbehaving client may reuse MessageIDs).
	if oldByMsg, ok := r.byMessageID[notify.MessageID]; ok {
		if string(oldByMsg.FileID[:]) != string(notify.FileID[:]) {
			r.unregisterLocked(oldByMsg)
		}
	}

	r.byFileID[string(notify.FileID[:])] = notify
	r.byMessageID[notify.MessageID] = notify
	r.byAsyncId[notify.AsyncId] = notify
	r.pending[notify.WatchPath] = append(r.pending[notify.WatchPath], notify)

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

// UnregisterByMessageID removes a pending notification by MessageID.
// Called by CANCEL to cancel a pending CHANGE_NOTIFY request.
// Returns the removed PendingNotify, or nil if not found.
func (r *NotifyRegistry) UnregisterByMessageID(messageID uint64) *PendingNotify {
	r.mu.Lock()
	defer r.mu.Unlock()

	notify, ok := r.byMessageID[messageID]
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

// unregisterLocked removes a PendingNotify from all internal maps.
// Must be called with r.mu held.
func (r *NotifyRegistry) unregisterLocked(notify *PendingNotify) *PendingNotify {
	fileIDKey := string(notify.FileID[:])
	delete(r.byFileID, fileIDKey)
	delete(r.byMessageID, notify.MessageID)
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

// Encode serializes the ChangeNotifyResponse to wire format.
func (resp *ChangeNotifyResponse) Encode() ([]byte, error) {
	bufLen := len(resp.Buffer)
	w := smbenc.NewWriter(8 + bufLen)
	w.WriteUint16(9) // StructureSize (always 9)

	if bufLen > 0 {
		w.WriteUint16(72)             // OutputBufferOffset (after SMB2 header)
		w.WriteUint32(uint32(bufLen)) // OutputBufferLength
		w.WriteBytes(resp.Buffer)     // Buffer
	} else {
		w.WriteUint16(0) // OutputBufferOffset
		w.WriteUint32(0) // OutputBufferLength
	}

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
// requests. When a matching watcher has an AsyncCallback set, it sends the
// async response. Otherwise, the change is logged for debugging.
//
// Parameters:
//   - shareName: The share where the change occurred
//   - parentPath: Share-relative path of the directory containing the changed item
//   - fileName: Name of the changed file/directory
//   - action: One of FileAction* constants
//
// The function walks up the directory hierarchy to support recursive (WatchTree)
// watchers. When a matching watcher is found:
//  1. Builds a FileNotifyInformation structure with the change details
//  2. Encodes it into the response format
//  3. Calls the AsyncCallback to send the response
//  4. Unregisters the watcher (CHANGE_NOTIFY is one-shot per request)
func (r *NotifyRegistry) NotifyChange(shareName, parentPath, fileName string, action uint32) {
	r.mu.RLock()
	watchers := r.findWatchersLocked(shareName, parentPath, action)
	r.mu.RUnlock()

	for _, w := range watchers {
		relativePath := relativePathFromWatch(w.watchPath, parentPath, fileName)
		changes := []FileNotifyInformation{
			{Action: action, FileName: relativePath},
		}
		r.sendAndUnregister(w.notify, changes, actionToString(action))
	}
}

// NotifyRename records a rename event as a paired FILE_NOTIFY_INFORMATION response.
//
// Per [MS-FSCC] 2.4.42 and [MS-SMB2] 3.3.4.4, a rename notification MUST contain
// two entries in a single response: FILE_ACTION_RENAMED_OLD_NAME followed by
// FILE_ACTION_RENAMED_NEW_NAME. Sending them as separate one-shot notifications
// is incorrect because CHANGE_NOTIFY is one-shot -- the first notification
// unregisters the watcher, causing the second to be silently dropped.
//
// Parameters:
//   - shareName: The share where the rename occurred
//   - oldParentPath: Share-relative directory path of the old location
//   - oldFileName: Old filename
//   - newParentPath: Share-relative directory path of the new location
//   - newFileName: New filename
func (r *NotifyRegistry) NotifyRename(shareName, oldParentPath, oldFileName, newParentPath, newFileName string) {
	r.mu.RLock()

	// Walk up from the old parent path to find watchers.
	// We match against the old parent path as the primary watch target,
	// since that's where Explorer has its directory watch.
	oldWatchers := r.findWatchersLocked(shareName, oldParentPath, FileActionRenamedOldName)

	// Also walk up from the new parent path if different, to catch watchers
	// on the destination directory that aren't ancestors of the old path.
	var newWatchers []watcherMatch
	if newParentPath != oldParentPath {
		// Build a set of already-matched FileIDs for O(1) dedup lookup
		matchedFileIDs := make(map[[16]byte]struct{}, len(oldWatchers))
		for _, m := range oldWatchers {
			matchedFileIDs[m.notify.FileID] = struct{}{}
		}

		allNew := r.findWatchersLocked(shareName, newParentPath, FileActionRenamedNewName)
		for _, m := range allNew {
			if _, alreadyMatched := matchedFileIDs[m.notify.FileID]; !alreadyMatched {
				newWatchers = append(newWatchers, m)
			}
		}
	}

	r.mu.RUnlock()

	// Send paired rename notifications for all matched watchers.
	// Both old and new names are computed relative to each watcher's watch path,
	// so recursive watchers at higher-level directories report correct paths.
	for _, w := range append(oldWatchers, newWatchers...) {
		oldRelativePath := relativePathFromWatch(w.watchPath, oldParentPath, oldFileName)
		newRelativePath := relativePathFromWatch(w.watchPath, newParentPath, newFileName)
		changes := []FileNotifyInformation{
			{Action: FileActionRenamedOldName, FileName: oldRelativePath},
			{Action: FileActionRenamedNewName, FileName: newRelativePath},
		}
		r.sendAndUnregister(w.notify, changes, "RENAME")
	}
}

// watcherMatch pairs a matched watcher with the watch path that matched it.
// The watchPath is needed to compute relative paths for notifications.
type watcherMatch struct {
	notify    *PendingNotify
	watchPath string // The path in the hierarchy where the watcher was found
}

// findWatchersLocked walks up the directory hierarchy from parentPath to find
// watchers matching the given share and action. Must be called with r.mu held
// (at least read-locked). Returns matched watchers with their watch paths.
func (r *NotifyRegistry) findWatchersLocked(shareName, parentPath string, action uint32) []watcherMatch {
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
			if !MatchesFilter(action, w.CompletionFilter) {
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

// sendAndUnregister encodes a list of FileNotifyInformation changes, sends
// the notification via the watcher's AsyncCallback, and unregisters the
// watcher (CHANGE_NOTIFY is one-shot). If the encoded buffer exceeds the
// watcher's MaxOutputLength, the watcher is unregistered without sending.
func (r *NotifyRegistry) sendAndUnregister(w *PendingNotify, changes []FileNotifyInformation, label string) {
	// Unregister FIRST to prevent double-fire: two concurrent NotifyChange calls
	// can both snapshot the same watcher under RLock. By unregistering before
	// calling the callback, only the first caller proceeds (Unregister returns
	// nil for the second). This is critical because sending two async responses
	// for the same MessageID violates the protocol.
	removed := r.Unregister(w.FileID)
	if removed == nil {
		return // already fired by another goroutine
	}

	if removed.AsyncCallback == nil {
		logger.Debug("CHANGE_NOTIFY: would notify watcher (no callback)",
			"watchPath", removed.WatchPath,
			"action", label,
			"messageID", removed.MessageID)
		return
	}

	buffer := EncodeFileNotifyInformation(changes)

	// Per MS-SMB2 3.3.4.4 / MS-FSCC 2.4.42: when the encoded notification
	// exceeds MaxOutputLength, complete the request with STATUS_NOTIFY_ENUM_DIR
	// to tell the client to re-enumerate the directory.
	if uint32(len(buffer)) > removed.MaxOutputLength {
		logger.Warn("CHANGE_NOTIFY: notification exceeds MaxOutputLength; sending STATUS_NOTIFY_ENUM_DIR",
			"watchPath", removed.WatchPath,
			"action", label,
			"encodedLength", len(buffer),
			"maxOutputLength", removed.MaxOutputLength,
			"messageID", removed.MessageID)
		enumResp := &ChangeNotifyResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyEnumDir},
		}
		if err := removed.AsyncCallback(removed.SessionID, removed.MessageID, removed.AsyncId, enumResp); err != nil {
			logger.Warn("CHANGE_NOTIFY: failed to send STATUS_NOTIFY_ENUM_DIR",
				"messageID", removed.MessageID,
				"error", err)
		}
		return
	}

	response := &ChangeNotifyResponse{
		OutputBufferLength: uint32(len(buffer)),
		Buffer:             buffer,
	}

	logger.Debug("CHANGE_NOTIFY: sending async response",
		"watchPath", removed.WatchPath,
		"action", label,
		"messageID", removed.MessageID,
		"sessionID", removed.SessionID)

	if err := removed.AsyncCallback(removed.SessionID, removed.MessageID, removed.AsyncId, response); err != nil {
		logger.Warn("CHANGE_NOTIFY: failed to send async response",
			"messageID", removed.MessageID,
			"error", err)
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
	r.NotifyChange(shareName, parentPath, dirName, FileActionRemoved)
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
	r.mu.Unlock()
	return toRemove
}

// UnregisterAllForTree unregisters all pending watchers for a specific tree
// connect (identified by sessionID + shareName). Sends STATUS_NOTIFY_CLEANUP
// for each. Called during TREE_DISCONNECT cleanup.
func (r *NotifyRegistry) UnregisterAllForTree(sessionID uint64, shareName string) []*PendingNotify {
	r.mu.Lock()
	var toRemove []*PendingNotify
	for _, watchers := range r.pending {
		for _, w := range watchers {
			if w.SessionID == sessionID && w.ShareName == shareName {
				toRemove = append(toRemove, w)
			}
		}
	}
	for _, w := range toRemove {
		r.unregisterLocked(w)
	}
	r.mu.Unlock()
	return toRemove
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

// IsValidCompletionFilter checks if the CompletionFilter contains only valid flags.
// Per MS-SMB2 3.3.5.15: if CompletionFilter is 0 or contains invalid flags,
// return STATUS_INVALID_PARAMETER.
func IsValidCompletionFilter(filter uint32) bool {
	return filter != 0 && (filter & ^AllValidCompletionFilterFlags) == 0
}

// actionToString converts an action code to a readable string.
func actionToString(action uint32) string {
	switch action {
	case FileActionAdded:
		return "ADDED"
	case FileActionRemoved:
		return "REMOVED"
	case FileActionModified:
		return "MODIFIED"
	case FileActionRenamedOldName:
		return "RENAMED_OLD"
	case FileActionRenamedNewName:
		return "RENAMED_NEW"
	case FileActionAddedStream:
		return "ADDED_STREAM"
	case FileActionRemovedStream:
		return "REMOVED_STREAM"
	case FileActionModifiedStream:
		return "MODIFIED_STREAM"
	default:
		return fmt.Sprintf("UNKNOWN(0x%X)", action)
	}
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
