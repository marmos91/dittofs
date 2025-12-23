package handlers

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/cache"
	"github.com/marmos91/dittofs/internal/protocol/smb/rpc"
	"github.com/marmos91/dittofs/internal/protocol/smb/session"
	"github.com/marmos91/dittofs/internal/protocol/smb/signing"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Handler manages SMB2 protocol handling
type Handler struct {
	Registry  *registry.Registry
	StartTime time.Time

	// Server identity
	ServerGUID [16]byte

	// Session management (unified with credit tracking)
	SessionManager *session.Manager

	// Pending auth sessions (mid-handshake)
	pendingAuth sync.Map // sessionID -> *PendingAuth

	// Tree connections
	trees      sync.Map // treeID -> *TreeConnection
	nextTreeID atomic.Uint32

	// Open files
	files      sync.Map // string(fileID) -> *OpenFile
	nextFileID atomic.Uint64

	// Named pipe management (for IPC$ RPC)
	PipeManager *rpc.PipeManager

	// Oplock management
	OplockManager *OplockManager

	// Change notification management
	NotifyRegistry *NotifyRegistry

	// Configuration
	MaxTransactSize uint32
	MaxReadSize     uint32
	MaxWriteSize    uint32

	// Signing configuration
	SigningConfig signing.SigningConfig
}

// PendingAuth tracks sessions in the middle of NTLM authentication.
// This stores the server's challenge for NTLMv2 response validation
// and session key derivation.
type PendingAuth struct {
	SessionID       uint64
	ClientAddr      string
	CreatedAt       time.Time
	ServerChallenge [8]byte // Random challenge sent in Type 2 message
}

// TreeConnection represents a tree connection (share)
type TreeConnection struct {
	TreeID     uint32
	SessionID  uint64
	ShareName  string
	ShareType  uint8
	CreatedAt  time.Time
	Permission identity.SharePermission // User's permission level for this share
}

// OpenFile represents an open file handle
type OpenFile struct {
	FileID              [16]byte
	TreeID              uint32
	SessionID           uint64
	Path                string
	ShareName           string
	OpenTime            time.Time
	DesiredAccess       uint32
	IsDirectory         bool
	IsPipe              bool   // True if this is a named pipe (IPC$)
	PipeName            string // Named pipe name (e.g., "srvsvc")
	EnumerationComplete bool   // For directories: true if directory listing was returned

	// Store integration fields
	MetadataHandle metadata.FileHandle // Link to metadata store file handle
	ContentID      metadata.ContentID  // Content identifier for read/write operations

	// Directory enumeration state
	EnumerationCookie []byte // Opaque cookie for resuming directory listing
	EnumerationIndex  int    // Current index in directory listing

	// Delete on close support (FileDispositionInformation)
	DeletePending bool                // If true, delete file/directory when handle is closed
	ParentHandle  metadata.FileHandle // Parent directory handle for deletion
	FileName      string              // File name within parent for deletion

	// Oplock state
	// OplockLevel is the current oplock level for this handle.
	// Thread safety: This field is written during CREATE (before storing in sync.Map)
	// and during OPLOCK_BREAK (for a specific FileID). Since file handles are session-
	// specific and OPLOCK_BREAK targets a specific FileID, concurrent access is not
	// expected. If this changes, consider using atomic operations.
	OplockLevel uint8
}

// NewHandler creates a new SMB2 handler with default session manager.
// For custom session management (e.g., shared across adapters), use
// NewHandlerWithSessionManager instead.
func NewHandler() *Handler {
	return NewHandlerWithSessionManager(session.NewDefaultManager())
}

// NewHandlerWithSessionManager creates a new SMB2 handler with an external session manager.
// This allows sharing the session manager with other components (e.g., SMBAdapter for credits).
func NewHandlerWithSessionManager(sessionManager *session.Manager) *Handler {
	h := &Handler{
		StartTime:       time.Now(),
		SessionManager:  sessionManager,
		PipeManager:     rpc.NewPipeManager(),
		OplockManager:   NewOplockManager(),
		NotifyRegistry:  NewNotifyRegistry(),
		MaxTransactSize: 65536,
		MaxReadSize:     65536,
		MaxWriteSize:    65536,
		SigningConfig:   signing.DefaultSigningConfig(),
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
// This automatically cleans up credit tracking as well.
func (h *Handler) DeleteSession(sessionID uint64) {
	h.SessionManager.DeleteSession(sessionID)
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

// DeleteOpenFile removes an open file by FileID
func (h *Handler) DeleteOpenFile(fileID [16]byte) {
	h.files.Delete(string(fileID[:]))
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

		// Release locks for this file
		metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
		if err != nil {
			logger.Warn("ReleaseAllLocksForSession: failed to get metadata store",
				"share", openFile.ShareName,
				"path", openFile.Path,
				"error", err)
			return true // Continue despite error
		}

		// UnlockAllForSession doesn't return errors for missing locks
		if unlockErr := metadataStore.UnlockAllForSession(ctx, openFile.MetadataHandle, sessionID); unlockErr != nil {
			logger.Warn("ReleaseAllLocksForSession: failed to release locks",
				"share", openFile.ShareName,
				"path", openFile.Path,
				"error", unlockErr)
		}

		return true
	})
}

// CloseAllFilesForSession closes all open files for a session.
// This releases locks, flushes caches, handles delete-on-close, and removes file handles.
// Returns the number of files closed.
func (h *Handler) CloseAllFilesForSession(ctx context.Context, sessionID uint64) int {
	var closed int
	var toDelete [][16]byte

	// Get session for auth context (may be nil if session already deleted)
	sess, _ := h.GetSession(sessionID)

	// First pass: collect files to close and release locks
	h.files.Range(func(key, value any) bool {
		openFile := value.(*OpenFile)
		if openFile.SessionID != sessionID {
			return true // Continue iterating
		}

		// Handle pipe close
		if openFile.IsPipe {
			h.PipeManager.ClosePipe(openFile.FileID)
			toDelete = append(toDelete, openFile.FileID)
			closed++
			return true
		}

		metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
		if err != nil {
			logger.Warn("CloseAllFilesForSession: failed to get metadata store",
				"share", openFile.ShareName,
				"error", err)
			toDelete = append(toDelete, openFile.FileID)
			closed++
			return true
		}

		// Release locks for this file
		if !openFile.IsDirectory && len(openFile.MetadataHandle) > 0 {
			_ = metadataStore.UnlockAllForSession(ctx, openFile.MetadataHandle, sessionID)
		}

		// Flush cache if needed
		if !openFile.IsDirectory && openFile.ContentID != "" {
			h.flushFileCache(ctx, openFile)
		}

		// Handle delete-on-close (FileDispositionInformation)
		if openFile.DeletePending && len(openFile.ParentHandle) > 0 && openFile.FileName != "" {
			authCtx := h.buildCleanupAuthContext(ctx, sess)
			if openFile.IsDirectory {
				if err := metadataStore.RemoveDirectory(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
					logger.Debug("CloseAllFilesForSession: failed to delete directory",
						"path", openFile.Path, "error", err)
				} else {
					logger.Debug("CloseAllFilesForSession: directory deleted", "path", openFile.Path)
				}
			} else {
				if _, err := metadataStore.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
					logger.Debug("CloseAllFilesForSession: failed to delete file",
						"path", openFile.Path, "error", err)
				} else {
					logger.Debug("CloseAllFilesForSession: file deleted", "path", openFile.Path)
				}
			}
		}

		toDelete = append(toDelete, openFile.FileID)
		closed++
		return true
	})

	// Second pass: delete collected file handles
	for _, fileID := range toDelete {
		h.DeleteOpenFile(fileID)
	}

	if closed > 0 {
		logger.Debug("CloseAllFilesForSession: closed files",
			"sessionID", sessionID,
			"count", closed)
	}

	return closed
}

// CloseAllFilesForTree closes all open files associated with a tree connection.
// This releases locks, flushes caches, handles delete-on-close, and removes file handles.
// The sessionID parameter is used for authorization context during delete-on-close
// and lock release operations. Files are filtered by both treeID and sessionID for safety.
// Returns the number of files closed.
func (h *Handler) CloseAllFilesForTree(ctx context.Context, treeID uint32, sessionID uint64) int {
	var closed int
	var toDelete [][16]byte

	// Get session for auth context (may be nil if session already deleted)
	sess, _ := h.GetSession(sessionID)

	// First pass: collect files to close and release locks
	h.files.Range(func(key, value any) bool {
		openFile := value.(*OpenFile)
		// Filter by both treeID and sessionID for safety
		if openFile.TreeID != treeID || openFile.SessionID != sessionID {
			return true // Continue iterating
		}

		// Handle pipe close
		if openFile.IsPipe {
			h.PipeManager.ClosePipe(openFile.FileID)
			toDelete = append(toDelete, openFile.FileID)
			closed++
			return true
		}

		metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
		if err != nil {
			logger.Warn("CloseAllFilesForTree: failed to get metadata store",
				"share", openFile.ShareName,
				"error", err)
			toDelete = append(toDelete, openFile.FileID)
			closed++
			return true
		}

		// Release locks for this file
		if !openFile.IsDirectory && len(openFile.MetadataHandle) > 0 {
			_ = metadataStore.UnlockAllForSession(ctx, openFile.MetadataHandle, sessionID)
		}

		// Flush cache if needed
		if !openFile.IsDirectory && openFile.ContentID != "" {
			h.flushFileCache(ctx, openFile)
		}

		// Handle delete-on-close (FileDispositionInformation)
		if openFile.DeletePending && len(openFile.ParentHandle) > 0 && openFile.FileName != "" {
			authCtx := h.buildCleanupAuthContext(ctx, sess)
			if openFile.IsDirectory {
				if err := metadataStore.RemoveDirectory(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
					logger.Debug("CloseAllFilesForTree: failed to delete directory",
						"path", openFile.Path, "error", err)
				} else {
					logger.Debug("CloseAllFilesForTree: directory deleted", "path", openFile.Path)
				}
			} else {
				if _, err := metadataStore.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
					logger.Debug("CloseAllFilesForTree: failed to delete file",
						"path", openFile.Path, "error", err)
				} else {
					logger.Debug("CloseAllFilesForTree: file deleted", "path", openFile.Path)
				}
			}
		}

		toDelete = append(toDelete, openFile.FileID)
		closed++
		return true
	})

	// Second pass: delete collected file handles
	for _, fileID := range toDelete {
		h.DeleteOpenFile(fileID)
	}

	if closed > 0 {
		logger.Debug("CloseAllFilesForTree: closed files",
			"treeID", treeID,
			"count", closed)
	}

	return closed
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

// CleanupSession performs full cleanup for a session.
// This closes all files, releases all locks, removes all tree connections,
// and deletes the session. Called on LOGOFF or connection close.
func (h *Handler) CleanupSession(ctx context.Context, sessionID uint64) {
	logger.Debug("CleanupSession: starting cleanup", "sessionID", sessionID)

	// 1. Close all open files (this also releases locks and flushes caches)
	filesClosed := h.CloseAllFilesForSession(ctx, sessionID)

	// 2. Delete all tree connections
	treesDeleted := h.DeleteAllTreesForSession(sessionID)

	// 3. Clean up any pending auth state
	h.DeletePendingAuth(sessionID)

	// 4. Delete the session itself
	h.DeleteSession(sessionID)

	logger.Debug("CleanupSession: completed",
		"sessionID", sessionID,
		"filesClosed", filesClosed,
		"treesDeleted", treesDeleted)
}

// flushFileCache flushes cached data for an open file.
// This is a helper used during cleanup to ensure data durability.
func (h *Handler) flushFileCache(ctx context.Context, openFile *OpenFile) {
	fileCache := h.Registry.GetCacheForShare(openFile.ShareName)
	if fileCache == nil || fileCache.Size(openFile.ContentID) == 0 {
		return
	}

	contentStore, err := h.Registry.GetContentStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("flushFileCache: failed to get content store",
			"share", openFile.ShareName,
			"error", err)
		return
	}

	// Use FlushAndFinalizeCache for immediate durability (completes S3 uploads)
	_, flushErr := cache.FlushAndFinalizeCache(ctx, fileCache, contentStore, openFile.ContentID)
	if flushErr != nil {
		logger.Warn("flushFileCache: cache flush failed",
			"path", openFile.Path,
			"contentID", openFile.ContentID,
			"error", flushErr)
		// Continue despite error - background flusher will eventually persist
	} else {
		logger.Debug("flushFileCache: flushed and finalized",
			"path", openFile.Path,
			"contentID", openFile.ContentID)
	}
}

// buildCleanupAuthContext creates an AuthContext for cleanup operations.
// This is used during session/tree cleanup when we need to perform file operations
// (like delete-on-close) but don't have a full SMBHandlerContext.
// If the session is available, it uses the session's user credentials.
// Otherwise, it falls back to root credentials for cleanup operations.
func (h *Handler) buildCleanupAuthContext(ctx context.Context, sess *session.Session) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{},
	}

	if sess != nil && sess.User != nil {
		// Use session user's credentials
		authCtx.Identity.UID = &sess.User.UID
		authCtx.Identity.GID = &sess.User.GID
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
func (h *Handler) CreateSessionWithUser(sessionID uint64, clientAddr string, user *identity.User, domain string) *session.Session {
	sess := session.NewSessionWithUser(sessionID, clientAddr, user, domain)
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

// StorePendingAuth stores a pending authentication
func (h *Handler) StorePendingAuth(pending *PendingAuth) {
	h.pendingAuth.Store(pending.SessionID, pending)
}

// GetPendingAuth retrieves a pending authentication by session ID
func (h *Handler) GetPendingAuth(sessionID uint64) (*PendingAuth, bool) {
	v, ok := h.pendingAuth.Load(sessionID)
	if !ok {
		return nil, false
	}
	return v.(*PendingAuth), true
}

// DeletePendingAuth removes a pending authentication
func (h *Handler) DeletePendingAuth(sessionID uint64) {
	h.pendingAuth.Delete(sessionID)
}
