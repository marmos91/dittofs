// Package handlers provides SMB2 command handlers and session management.
//
// This file implements the SMB2 CHANGE_NOTIFY command [MS-SMB2] 2.2.35, 2.2.36.
// CHANGE_NOTIFY allows clients to watch directories for changes.
package handlers

import (
	"encoding/binary"
	"fmt"
	"sync"

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
//
// Clients use this to register for directory change notifications.
// The server responds asynchronously when changes occur.
//
// **Wire Format (32 bytes):**
//
//	Offset  Size  Field              Description
//	------  ----  -----------------  ----------------------------------
//	0       2     StructureSize      Always 32
//	2       2     Flags              SMB2_WATCH_TREE for recursive
//	4       4     OutputBufferLength Maximum response size
//	8       16    FileId             Directory handle
//	24      4     CompletionFilter   Types of changes to watch
//	28      4     Reserved           Reserved (0)
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
//
// **Wire Format (8 bytes + variable):**
//
//	Offset  Size  Field              Description
//	------  ----  -----------------  ----------------------------------
//	0       2     StructureSize      Always 9
//	2       2     OutputBufferOffset Offset to output buffer
//	4       4     OutputBufferLength Length of output buffer
//	8+      var   Buffer             Array of FileNotifyInformation
type ChangeNotifyResponse struct {
	SMBResponseBase
	OutputBufferOffset uint16
	OutputBufferLength uint32
	Buffer             []byte // Serialized FileNotifyInformation array
}

// FileNotifyInformation represents a single change notification [MS-FSCC] 2.4.42.
//
// **Wire Format (12 bytes + variable):**
//
//	Offset  Size  Field             Description
//	------  ----  ----------------  ----------------------------------
//	0       4     NextEntryOffset   Offset to next entry (0 if last)
//	4       4     Action            FileAction* constant
//	8       4     FileNameLength    Length of file name in bytes
//	12      var   FileName          UTF-16LE file name
type FileNotifyInformation struct {
	Action   uint32
	FileName string // Relative path within watched directory
}

// ============================================================================
// Pending Notify Registry
// ============================================================================

// PendingNotify tracks a pending CHANGE_NOTIFY request waiting for events.
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

	// Connection for async response
	// Note: For MVP, we don't send async responses - just complete on next matching event
}

// NotifyRegistry manages pending CHANGE_NOTIFY requests.
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
func (r *NotifyRegistry) Register(notify *PendingNotify) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fileIDKey := string(notify.FileID[:])
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

// MatchesFilter checks if an action matches a completion filter.
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

// DecodeChangeNotifyRequest parses a CHANGE_NOTIFY request.
func DecodeChangeNotifyRequest(body []byte) (*ChangeNotifyRequest, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("CHANGE_NOTIFY request too short: %d bytes", len(body))
	}

	structSize := binary.LittleEndian.Uint16(body[0:2])
	if structSize != 32 {
		return nil, fmt.Errorf("invalid CHANGE_NOTIFY structure size: %d", structSize)
	}

	req := &ChangeNotifyRequest{
		Flags:              binary.LittleEndian.Uint16(body[2:4]),
		OutputBufferLength: binary.LittleEndian.Uint32(body[4:8]),
		CompletionFilter:   binary.LittleEndian.Uint32(body[24:28]),
	}
	copy(req.FileID[:], body[8:24])

	return req, nil
}

// Encode serializes the ChangeNotifyResponse to wire format.
func (resp *ChangeNotifyResponse) Encode() ([]byte, error) {
	// Calculate total size: 8 bytes fixed + buffer
	bufLen := len(resp.Buffer)
	totalLen := 8 + bufLen

	buf := make([]byte, totalLen)
	binary.LittleEndian.PutUint16(buf[0:2], 9) // StructureSize (always 9)

	if bufLen > 0 {
		binary.LittleEndian.PutUint16(buf[2:4], 72) // OutputBufferOffset (after SMB2 header)
		binary.LittleEndian.PutUint32(buf[4:8], uint32(bufLen))
		copy(buf[8:], resp.Buffer)
	} else {
		binary.LittleEndian.PutUint16(buf[2:4], 0)
		binary.LittleEndian.PutUint32(buf[4:8], 0)
	}

	return buf, nil
}

// EncodeFileNotifyInformation encodes a list of change notifications.
func EncodeFileNotifyInformation(changes []FileNotifyInformation) []byte {
	if len(changes) == 0 {
		return nil
	}

	// Calculate total size
	totalSize := 0
	for _, c := range changes {
		// 12 bytes header + UTF-16LE filename (2 bytes per char)
		entrySize := 12 + len(c.FileName)*2
		// Align to 4 bytes
		if entrySize%4 != 0 {
			entrySize += 4 - (entrySize % 4)
		}
		totalSize += entrySize
	}

	buf := make([]byte, totalSize)
	offset := 0

	for i, c := range changes {
		entryStart := offset

		// Skip NextEntryOffset for now (fill in later)
		offset += 4

		// Action
		binary.LittleEndian.PutUint32(buf[offset:], c.Action)
		offset += 4

		// FileNameLength (in bytes, UTF-16LE)
		nameLen := len(c.FileName) * 2
		binary.LittleEndian.PutUint32(buf[offset:], uint32(nameLen))
		offset += 4

		// FileName (UTF-16LE)
		for _, r := range c.FileName {
			binary.LittleEndian.PutUint16(buf[offset:], uint16(r))
			offset += 2
		}

		// Align to 4 bytes
		for offset%4 != 0 {
			buf[offset] = 0
			offset++
		}

		// Fill in NextEntryOffset (0 for last entry)
		if i < len(changes)-1 {
			binary.LittleEndian.PutUint32(buf[entryStart:], uint32(offset-entryStart))
		}
	}

	return buf
}

// ============================================================================
// Notification Helpers
// ============================================================================

// NotifyChange records a filesystem change that may trigger pending CHANGE_NOTIFY
// requests. For MVP, we only log the notification - actual async responses
// require connection-level async response support.
//
// Parameters:
//   - shareName: The share where the change occurred
//   - parentPath: Share-relative path of the directory containing the changed item
//   - fileName: Name of the changed file/directory
//   - action: One of FileAction* constants
//
// Note: For full CHANGE_NOTIFY support, this would need to:
// 1. Find all pending notifies watching parentPath (or ancestors if recursive)
// 2. Check if the action matches the CompletionFilter
// 3. Build an async response with the change information
// 4. Send the response on the appropriate connection
//
// For MVP, we log the potential notification for debugging.
func (r *NotifyRegistry) NotifyChange(shareName, parentPath, fileName string, action uint32) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Get watchers for this path
	watchers := r.pending[parentPath]
	if len(watchers) == 0 {
		return
	}

	// Log for debugging (full implementation would send async responses)
	for _, w := range watchers {
		if w.ShareName != shareName {
			continue
		}
		if !MatchesFilter(action, w.CompletionFilter) {
			continue
		}

		logger.Debug("CHANGE_NOTIFY: would notify watcher",
			"watchPath", w.WatchPath,
			"fileName", fileName,
			"action", actionToString(action),
			"messageID", w.MessageID)
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

// GetParentPath returns the parent directory path from a full path.
// Examples:
//   - "/foo/bar/file.txt" -> "/foo/bar"
//   - "/file.txt" -> "/"
//   - "/" -> "/"
func GetParentPath(path string) string {
	if path == "" || path == "/" {
		return "/"
	}

	// Remove trailing slash if present
	if path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}

	// Find last separator
	lastSlash := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			lastSlash = i
			break
		}
	}

	if lastSlash <= 0 {
		return "/"
	}
	return path[:lastSlash]
}

// GetFileName returns the file name from a full path.
// Examples:
//   - "/foo/bar/file.txt" -> "file.txt"
//   - "/file.txt" -> "file.txt"
//   - "/" -> ""
func GetFileName(path string) string {
	if path == "" || path == "/" {
		return ""
	}

	// Remove trailing slash if present
	if path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}

	// Find last separator
	lastSlash := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			lastSlash = i
			break
		}
	}

	if lastSlash < 0 {
		return path
	}
	return path[lastSlash+1:]
}
