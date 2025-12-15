package handlers

import (
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/registry"
)

// Handler manages SMB2 protocol handling
type Handler struct {
	Registry  *registry.Registry
	StartTime time.Time

	// Server identity
	ServerGUID [16]byte

	// Session management
	sessions      sync.Map // sessionID -> *Session
	nextSessionID atomic.Uint64

	// Pending auth sessions (mid-handshake)
	pendingAuth sync.Map // sessionID -> *PendingAuth

	// Tree connections
	trees      sync.Map // treeID -> *TreeConnection
	nextTreeID atomic.Uint32

	// Open files
	files      sync.Map // string(fileID) -> *OpenFile
	nextFileID atomic.Uint64

	// Configuration
	MaxTransactSize uint32
	MaxReadSize     uint32
	MaxWriteSize    uint32
}

// PendingAuth tracks sessions in the middle of NTLM authentication
type PendingAuth struct {
	SessionID  uint64
	ClientAddr string
	CreatedAt  time.Time
}

// Session represents an SMB session
type Session struct {
	SessionID  uint64
	IsGuest    bool
	IsNull     bool
	CreatedAt  time.Time
	ClientAddr string
	Username   string
	Domain     string
}

// TreeConnection represents a tree connection (share)
type TreeConnection struct {
	TreeID    uint32
	SessionID uint64
	ShareName string
	ShareType uint8
	CreatedAt time.Time
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
	EnumerationComplete bool // For directories: true if directory listing was returned
}

// NewHandler creates a new SMB2 handler
func NewHandler() *Handler {
	h := &Handler{
		StartTime:       time.Now(),
		MaxTransactSize: 65536,
		MaxReadSize:     65536,
		MaxWriteSize:    65536,
	}

	// Generate random server GUID
	_, _ = rand.Read(h.ServerGUID[:])

	// Start session/tree IDs at 1 (0 is reserved)
	h.nextSessionID.Store(1)
	h.nextTreeID.Store(1)
	h.nextFileID.Store(1)

	return h
}

// GetSession retrieves a session by ID
func (h *Handler) GetSession(sessionID uint64) (*Session, bool) {
	v, ok := h.sessions.Load(sessionID)
	if !ok {
		return nil, false
	}
	return v.(*Session), true
}

// DeleteSession removes a session by ID
func (h *Handler) DeleteSession(sessionID uint64) {
	h.sessions.Delete(sessionID)
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

// GenerateSessionID generates a new unique session ID
func (h *Handler) GenerateSessionID() uint64 {
	return h.nextSessionID.Add(1)
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

// StoreSession stores a session
func (h *Handler) StoreSession(session *Session) {
	h.sessions.Store(session.SessionID, session)
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
