package handlers

import (
	"fmt"
	"path"
	"sync"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
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

	// FileNotifyChangeSecurity watches for security descriptor changes.
	FileNotifyChangeSecurity uint32 = 0x00000100
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
// The callback receives the session ID, message ID, and response data.
// Returns an error if the response could not be sent (e.g., connection closed).
type AsyncResponseCallback func(sessionID, messageID uint64, response *ChangeNotifyResponse) error

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
	mu       sync.RWMutex
	pending  map[string][]*PendingNotify // path -> pending requests
	byFileID map[string]*PendingNotify   // fileID string -> pending request
}

// NewNotifyRegistry creates a new notify registry.
func NewNotifyRegistry() *NotifyRegistry {
	return &NotifyRegistry{
		pending:  make(map[string][]*PendingNotify),
		byFileID: make(map[string]*PendingNotify),
	}
}

// Register adds a pending notification request.
// If a request with the same FileID already exists, it is replaced.
func (r *NotifyRegistry) Register(notify *PendingNotify) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fileIDKey := string(notify.FileID[:])

	// If there's already a registration for this FileID, remove the old entry
	// from the pending map to keep data structures consistent.
	if old, ok := r.byFileID[fileIDKey]; ok {
		pending := r.pending[old.WatchPath]
		for i, p := range pending {
			if string(p.FileID[:]) == fileIDKey {
				r.pending[old.WatchPath] = append(pending[:i], pending[i+1:]...)
				break
			}
		}
		if len(r.pending[old.WatchPath]) == 0 {
			delete(r.pending, old.WatchPath)
		}
	}

	r.byFileID[fileIDKey] = notify
	r.pending[notify.WatchPath] = append(r.pending[notify.WatchPath], notify)

	logger.Debug("NotifyRegistry: registered watch",
		"path", notify.WatchPath,
		"filter", fmt.Sprintf("0x%08X", notify.CompletionFilter),
		"recursive", notify.WatchTree)
}

// Unregister removes a pending notification by FileID.
// Called when the directory handle is closed or the request is cancelled.
func (r *NotifyRegistry) Unregister(fileID [16]byte) *PendingNotify {
	r.mu.Lock()
	defer r.mu.Unlock()

	fileIDKey := string(fileID[:])
	notify, ok := r.byFileID[fileIDKey]
	if !ok {
		return nil
	}

	delete(r.byFileID, fileIDKey)

	// Remove from pending list
	pending := r.pending[notify.WatchPath]
	for i, p := range pending {
		if string(p.FileID[:]) == fileIDKey {
			r.pending[notify.WatchPath] = append(pending[:i], pending[i+1:]...)
			break
		}
	}

	// Clean up empty entries
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

// MatchesFilter checks if a filesystem change action matches a CHANGE_NOTIFY
// completion filter [MS-SMB2] 2.2.35. It maps FileAction* constants to the
// corresponding FileNotifyChange* flags. For example, FileActionAdded matches
// FileNotifyChangeFileName and FileNotifyChangeDirName.
func MatchesFilter(action uint32, filter uint32) bool {
	switch action {
	case FileActionAdded, FileActionRemoved:
		// File/directory created or deleted
		return filter&(FileNotifyChangeFileName|FileNotifyChangeDirName) != 0
	case FileActionModified:
		// File modified
		return filter&(FileNotifyChangeSize|FileNotifyChangeLastWrite|FileNotifyChangeAttributes) != 0
	case FileActionRenamedOldName, FileActionRenamedNewName:
		// Rename
		return filter&(FileNotifyChangeFileName|FileNotifyChangeDirName) != 0
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
	// First, collect matching watchers while holding read lock
	r.mu.RLock()

	// Build list of watchers to notify
	type matchedWatcher struct {
		notify       *PendingNotify
		relativePath string // Path relative to watch directory
	}
	var toNotify []matchedWatcher

	// Walk up the directory hierarchy to support recursive (WatchTree) watchers.
	// This checks the exact parentPath first, then ancestor directories.
	currentPath := parentPath
	for {
		watchers := r.pending[currentPath]

		for _, w := range watchers {
			// Only notify watchers on the same share
			if w.ShareName != shareName {
				continue
			}

			// For ancestor paths (recursive watch), require WatchTree flag
			if currentPath != parentPath && !w.WatchTree {
				continue
			}

			if !MatchesFilter(action, w.CompletionFilter) {
				continue
			}

			// Calculate the relative path from the watch directory.
			// For recursive watchers, this includes the subdirectory prefix,
			// e.g., watching "/" for change in "/subdir/file.txt" -> "subdir/file.txt"
			relativePath := relativePathFromWatch(currentPath, parentPath, fileName)

			toNotify = append(toNotify, matchedWatcher{
				notify:       w,
				relativePath: relativePath,
			})
		}

		// Stop after processing the root directory
		if currentPath == "/" || currentPath == "" {
			break
		}

		// Move to the parent directory for recursive watcher lookup
		currentPath = GetParentPath(currentPath)
	}

	r.mu.RUnlock()

	// Now process the notifications outside the lock
	for _, match := range toNotify {
		w := match.notify

		if w.AsyncCallback != nil {
			// Build the change notification
			changes := []FileNotifyInformation{
				{
					Action:   action,
					FileName: match.relativePath,
				},
			}
			buffer := EncodeFileNotifyInformation(changes)

			// Ensure we don't exceed the max output length. We must not truncate the
			// FILE_NOTIFY_INFORMATION structure, as that would corrupt it.
			if uint32(len(buffer)) > w.MaxOutputLength {
				logger.Warn("CHANGE_NOTIFY: encoded notification exceeds MaxOutputLength; skipping",
					"watchPath", w.WatchPath,
					"fileName", match.relativePath,
					"action", actionToString(action),
					"encodedLength", len(buffer),
					"maxOutputLength", w.MaxOutputLength,
					"messageID", w.MessageID,
					"sessionID", w.SessionID)
				// Unregister the watcher to avoid repeated failures
				// (the client's buffer is too small for this path's notifications)
				r.Unregister(w.FileID)
				continue
			}

			response := &ChangeNotifyResponse{
				SMBResponseBase:    SMBResponseBase{},
				OutputBufferLength: uint32(len(buffer)),
				Buffer:             buffer,
			}

			logger.Debug("CHANGE_NOTIFY: sending async response",
				"watchPath", w.WatchPath,
				"fileName", match.relativePath,
				"action", actionToString(action),
				"messageID", w.MessageID,
				"sessionID", w.SessionID)

			// Send the async response
			if err := w.AsyncCallback(w.SessionID, w.MessageID, response); err != nil {
				logger.Warn("CHANGE_NOTIFY: failed to send async response",
					"messageID", w.MessageID,
					"error", err)
			}
			// Always unregister the watcher after notification attempt.
			// CHANGE_NOTIFY is one-shot per request - the client must re-issue
			// the request for more notifications. If the callback failed, the
			// connection is likely broken and the watcher would be useless anyway.
			r.Unregister(w.FileID)
		} else {
			// No callback - just log for debugging
			logger.Debug("CHANGE_NOTIFY: would notify watcher (no callback)",
				"watchPath", w.WatchPath,
				"fileName", match.relativePath,
				"action", actionToString(action),
				"messageID", w.MessageID)
		}
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
	// Collect matching watchers while holding read lock.
	// We match against the OLD parent path as the primary watch target,
	// since that's where Explorer has its directory watch.
	r.mu.RLock()

	type matchedWatcher struct {
		notify          *PendingNotify
		oldRelativePath string
		newRelativePath string
	}
	var toNotify []matchedWatcher

	// Walk up from the old parent path to find watchers
	currentPath := oldParentPath
	for {
		watchers := r.pending[currentPath]

		for _, w := range watchers {
			if w.ShareName != shareName {
				continue
			}

			if currentPath != oldParentPath && !w.WatchTree {
				continue
			}

			// Rename matches FileName and DirName filters
			if !MatchesFilter(FileActionRenamedOldName, w.CompletionFilter) {
				continue
			}

			// Calculate relative paths from the watch directory
			oldRelativePath := relativePathFromWatch(currentPath, oldParentPath, oldFileName)
			newRelativePath := relativePathFromWatch(currentPath, newParentPath, newFileName)

			toNotify = append(toNotify, matchedWatcher{
				notify:          w,
				oldRelativePath: oldRelativePath,
				newRelativePath: newRelativePath,
			})
		}

		if currentPath == "/" || currentPath == "" {
			break
		}
		currentPath = GetParentPath(currentPath)
	}

	// Also walk up from the new parent path if different, to catch watchers
	// on the destination directory that aren't ancestors of the old path.
	if newParentPath != oldParentPath {
		// Build a set of already-matched FileIDs for O(1) dedup lookup
		matchedFileIDs := make(map[[16]byte]struct{}, len(toNotify))
		for _, m := range toNotify {
			matchedFileIDs[m.notify.FileID] = struct{}{}
		}

		currentPath = newParentPath
		for {
			watchers := r.pending[currentPath]

			for _, w := range watchers {
				if w.ShareName != shareName {
					continue
				}

				if currentPath != newParentPath && !w.WatchTree {
					continue
				}

				if !MatchesFilter(FileActionRenamedNewName, w.CompletionFilter) {
					continue
				}

				// Check if already matched via old parent walk
				if _, alreadyMatched := matchedFileIDs[w.FileID]; alreadyMatched {
					continue
				}

				newRelativePath := relativePathFromWatch(currentPath, newParentPath, newFileName)

				toNotify = append(toNotify, matchedWatcher{
					notify:          w,
					oldRelativePath: oldFileName,
					newRelativePath: newRelativePath,
				})
			}

			if currentPath == "/" || currentPath == "" {
				break
			}
			currentPath = GetParentPath(currentPath)
		}
	}

	r.mu.RUnlock()

	// Send paired rename notifications
	for _, match := range toNotify {
		w := match.notify

		if w.AsyncCallback != nil {
			// Build paired FILE_NOTIFY_INFORMATION with both old and new names
			changes := []FileNotifyInformation{
				{
					Action:   FileActionRenamedOldName,
					FileName: match.oldRelativePath,
				},
				{
					Action:   FileActionRenamedNewName,
					FileName: match.newRelativePath,
				},
			}
			buffer := EncodeFileNotifyInformation(changes)

			if uint32(len(buffer)) > w.MaxOutputLength {
				logger.Warn("CHANGE_NOTIFY: rename notification exceeds MaxOutputLength; skipping",
					"watchPath", w.WatchPath,
					"oldName", match.oldRelativePath,
					"newName", match.newRelativePath,
					"encodedLength", len(buffer),
					"maxOutputLength", w.MaxOutputLength)
				r.Unregister(w.FileID)
				continue
			}

			response := &ChangeNotifyResponse{
				SMBResponseBase:    SMBResponseBase{},
				OutputBufferLength: uint32(len(buffer)),
				Buffer:             buffer,
			}

			logger.Debug("CHANGE_NOTIFY: sending rename notification",
				"watchPath", w.WatchPath,
				"oldName", match.oldRelativePath,
				"newName", match.newRelativePath,
				"messageID", w.MessageID,
				"sessionID", w.SessionID)

			if err := w.AsyncCallback(w.SessionID, w.MessageID, response); err != nil {
				logger.Warn("CHANGE_NOTIFY: failed to send rename notification",
					"messageID", w.MessageID,
					"error", err)
			}
			r.Unregister(w.FileID)
		} else {
			logger.Debug("CHANGE_NOTIFY: would notify rename (no callback)",
				"watchPath", w.WatchPath,
				"oldName", match.oldRelativePath,
				"newName", match.newRelativePath,
				"messageID", w.MessageID)
		}
	}
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
	relDir := parentPath[len(watchPath):]
	if len(relDir) > 0 && relDir[0] == '/' {
		relDir = relDir[1:]
	}
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
