package handlers

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Handler manages SMB2 protocol handling
type Handler struct {
	Registry  *runtime.Runtime
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

	// KerberosProvider holds the shared Kerberos keytab/config provider.
	// When set, the SESSION_SETUP handler supports Kerberos authentication
	// via SPNEGO in addition to NTLM. The same provider is used by the NFS
	// adapter, ensuring a shared Kerberos infrastructure across protocols.
	// nil when Kerberos is not enabled.
	//
	// Lifecycle: Not initialized by NewHandler/NewHandlerWithSessionManager.
	// Must be injected by the adapter layer (e.g., SMBAdapter.SetKerberosProvider)
	// before Serve() is called. When nil, Kerberos auth requests return
	// STATUS_LOGON_FAILURE gracefully (NTLM and guest auth still work).
	KerberosProvider *kerberos.Provider
}

// PendingAuth tracks sessions in the middle of NTLM authentication.
// This stores the server's challenge for NTLMv2 response validation
// and session key derivation.
type PendingAuth struct {
	SessionID       uint64
	ClientAddr      string
	CreatedAt       time.Time
	ServerChallenge [8]byte // Random challenge sent in Type 2 message
	UsedSPNEGO      bool    // Whether client used SPNEGO wrapping
}

// TreeConnection represents a tree connection (share)
type TreeConnection struct {
	TreeID     uint32
	SessionID  uint64
	ShareName  string
	ShareType  uint8
	CreatedAt  time.Time
	Permission models.SharePermission // User's permission level for this share
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
	PayloadID      metadata.PayloadID  // Content identifier for read/write operations

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

// NewHandler creates a new SMB2 handler with default session metaSvc.
// For custom session management (e.g., shared across adapters), use
// NewHandlerWithSessionManager instead.
func NewHandler() *Handler {
	return NewHandlerWithSessionManager(session.NewDefaultManager())
}

// NewHandlerWithSessionManager creates a new SMB2 handler with an external session metaSvc.
// This allows sharing the session metaSvc with other components (e.g., SMBAdapter for credits).
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
		metaSvc := h.Registry.GetMetadataService()

		// UnlockAllForSession doesn't return errors for missing locks
		if unlockErr := metaSvc.UnlockAllForSession(ctx, openFile.MetadataHandle, sessionID); unlockErr != nil {
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
	filter := func(f *OpenFile) bool {
		return f.SessionID == sessionID
	}
	return h.closeFilesWithFilter(ctx, sessionID, filter, "CloseAllFilesForSession")
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
	return h.closeFilesWithFilter(ctx, sessionID, filter, "CloseAllFilesForTree")
}

// closeFilesWithFilter closes files matching the filter predicate.
// This is the shared implementation for CloseAllFilesForSession and CloseAllFilesForTree.
func (h *Handler) closeFilesWithFilter(
	ctx context.Context,
	sessionID uint64,
	filter func(*OpenFile) bool,
	caller string,
) int {
	var closed int
	var toDelete [][16]byte

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
			h.PipeManager.ClosePipe(openFile.FileID)
			toDelete = append(toDelete, openFile.FileID)
			closed++
			return true
		}

		// Release locks for this file
		if !openFile.IsDirectory && len(openFile.MetadataHandle) > 0 {
			_ = metaSvc.UnlockAllForSession(ctx, openFile.MetadataHandle, sessionID)
		}

		// Flush cache if needed
		if !openFile.IsDirectory && openFile.PayloadID != "" {
			h.flushFileCache(ctx, openFile)
		}

		// Handle delete-on-close (FileDispositionInformation)
		if openFile.DeletePending && len(openFile.ParentHandle) > 0 && openFile.FileName != "" {
			h.handleDeleteOnClose(ctx, sess, openFile, caller)
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
		logger.Debug(caller+": closed files", "sessionID", sessionID, "count", closed)
	}

	return closed
}

// handleDeleteOnClose performs the delete operation for files marked with delete-on-close.
func (h *Handler) handleDeleteOnClose(ctx context.Context, sess *session.Session, openFile *OpenFile, caller string) {
	authCtx := h.buildCleanupAuthContext(ctx, sess, openFile.ShareName)
	metaSvc := h.Registry.GetMetadataService()

	if openFile.IsDirectory {
		if err := metaSvc.RemoveDirectory(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
			logger.Debug(caller+": failed to delete directory", "path", openFile.Path, "error", err)
		} else {
			logger.Debug(caller+": directory deleted", "path", openFile.Path)
		}
	} else {
		if _, err := metaSvc.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName); err != nil {
			logger.Debug(caller+": failed to delete file", "path", openFile.Path, "error", err)
		} else {
			logger.Debug(caller+": file deleted", "path", openFile.Path)
		}
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
	if openFile.PayloadID == "" {
		return
	}

	payloadSvc := h.Registry.GetBlockService()

	// Use blocking Flush for immediate durability
	_, flushErr := payloadSvc.Flush(ctx, openFile.PayloadID)
	if flushErr != nil {
		logger.Warn("flushFileCache: flush failed",
			"path", openFile.Path,
			"payloadID", openFile.PayloadID,
			"error", flushErr)
	} else {
		logger.Debug("flushFileCache: flushed",
			"path", openFile.Path,
			"payloadID", openFile.PayloadID)
	}
}

// buildCleanupAuthContext creates an AuthContext for cleanup operations.
// This is used during session/tree cleanup when we need to perform file operations
// (like delete-on-close) but don't have a full SMBHandlerContext.
// If the session is available, it uses the session user's UID/GID.
// Otherwise, it falls back to root credentials for cleanup operations.
func (h *Handler) buildCleanupAuthContext(ctx context.Context, sess *session.Session, _ string) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{},
	}

	if sess != nil && sess.User != nil {
		// Use session user's UID/GID from User object
		uid, gid := getUserIdentity(sess.User)
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
func (h *Handler) CreateSessionWithUser(sessionID uint64, clientAddr string, user *models.User, domain string) *session.Session {
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
