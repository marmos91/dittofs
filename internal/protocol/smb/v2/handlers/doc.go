// Package handlers provides SMB2 command handlers for DittoFS.
//
// # Overview
//
// This package implements SMB2 protocol handlers for file operations,
// session management, and tree (share) connections. Each handler corresponds
// to an SMB2 command as defined in [MS-SMB2].
//
// # Production Features
//
// Connection Management:
//   - Configurable timeouts for read, write, and idle connections
//   - Graceful shutdown with configurable timeout for draining
//   - Automatic cleanup on connection errors or panics
//
// Performance Optimization:
//   - Per-connection parallel request handling via semaphore
//   - Lock-free session and tree state via sync.Map
//   - Atomic ID generation with no contention
//   - Cache integration for read/write operations
//
// Security:
//   - NTLM authentication with SPNEGO wrapping
//   - Per-share permission validation
//   - Guest authentication fallback
//   - Session isolation between clients
//
// # Handler Architecture
//
// The Handler type is the central coordinator for all SMB2 operations.
// It maintains state across connections:
//
//	Handler
//	├── sessions     (sync.Map)   Active authenticated sessions
//	├── pendingAuth  (sync.Map)   Sessions mid-authentication
//	├── trees        (sync.Map)   Tree connections (mounted shares)
//	└── files        (sync.Map)   Open file handles
//
// Each SMB2 command is handled by a method on Handler:
//
//	Command          Handler Method       Purpose
//	---------------  ------------------   ---------------------------
//	NEGOTIATE        Negotiate()          Protocol negotiation
//	SESSION_SETUP    SessionSetup()       Authentication (NTLM)
//	LOGOFF           Logoff()             Session termination
//	TREE_CONNECT     TreeConnect()        Mount a share
//	TREE_DISCONNECT  TreeDisconnect()     Unmount a share
//	CREATE           Create()             Open/create file
//	CLOSE            Close()              Close file handle
//	READ             Read()               Read file data
//	WRITE            Write()              Write file data
//	QUERY_INFO       QueryInfo()          Get file attributes
//	QUERY_DIRECTORY  QueryDirectory()     List directory contents
//
// # Session Lifecycle
//
// Sessions track authenticated connections from clients:
//
//  1. Client connects (TCP)
//  2. NEGOTIATE: Protocol version agreed
//  3. SESSION_SETUP Round 1: Client sends NTLM NEGOTIATE
//     → Server stores PendingAuth, returns CHALLENGE
//  4. SESSION_SETUP Round 2: Client sends NTLM AUTHENTICATE
//     → Server creates Session, removes PendingAuth
//  5. TREE_CONNECT: Client mounts shares
//  6. File operations: CREATE, READ, WRITE, etc.
//  7. TREE_DISCONNECT: Client unmounts shares
//  8. LOGOFF: Session terminated
//
// The Session struct tracks:
//   - SessionID:  Unique identifier for the session
//   - IsGuest:    True for guest/anonymous authentication
//   - Username:   Authenticated username ("guest" for guest auth)
//   - ClientAddr: Client's network address
//   - CreatedAt:  Session creation timestamp
//
// # Thread Safety
//
// All state management uses sync.Map for lock-free concurrent access.
// This is critical because:
//   - Multiple goroutines handle concurrent client connections
//   - Sessions may be accessed from different TCP connections
//   - ID generation must be atomic across all handlers
//
// Atomic operations are used for ID generation:
//   - nextSessionID: atomic.Uint64
//   - nextTreeID:    atomic.Uint32
//   - nextFileID:    atomic.Uint64
//
// # Pending Authentication
//
// The PendingAuth struct tracks sessions in the middle of NTLM handshake:
//
//	Round 1: Type 1 NEGOTIATE → Create PendingAuth → Return Type 2 CHALLENGE
//	Round 2: Type 3 AUTHENTICATE → Validate PendingAuth → Create Session
//
// PendingAuth contains:
//   - SessionID:  Temporary ID assigned during handshake
//   - ClientAddr: Client address for validation
//   - CreatedAt:  Timestamp for timeout purposes
//
// Pending authentications should be cleaned up on timeout (not currently
// implemented) to prevent resource leaks from abandoned handshakes.
//
// # Tree Connections
//
// Tree connections represent mounted shares (similar to NFS exports):
//
//	Client: TREE_CONNECT \\server\sharename
//	Server: Validates share exists, returns TreeID
//
// The TreeConnection struct tracks:
//   - TreeID:    Unique identifier for this mount
//   - SessionID: Owning session (for access control)
//   - ShareName: Name of the mounted share
//   - ShareType: Share type (disk, printer, IPC, etc.)
//
// # Open Files
//
// Open files track file handles created by CREATE operations:
//
// The OpenFile struct tracks:
//   - FileID:       SMB2 file identifier (16 bytes)
//   - TreeID:       Associated tree connection
//   - SessionID:    Owning session
//   - Path:         File path within share
//   - ShareName:    Share name for registry lookup
//   - IsDirectory:  True if handle is for a directory
//
// # Error Handling
//
// Handlers return NT_STATUS codes via the HandlerResult type:
//
//	STATUS_SUCCESS                    Operation completed
//	STATUS_MORE_PROCESSING_REQUIRED   Continue NTLM handshake
//	STATUS_INVALID_PARAMETER          Malformed request
//	STATUS_USER_SESSION_DELETED       Invalid session ID
//	STATUS_NETWORK_NAME_DELETED       Invalid tree connection
//	STATUS_FILE_CLOSED                Invalid file handle
//
// # References
//
//   - [MS-SMB2] Server Message Block Protocol Versions 2 and 3
//     https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2
//   - [MS-SMB2] Section 2.2.5 - SESSION_SETUP Request
//   - [MS-SMB2] Section 2.2.6 - SESSION_SETUP Response
//   - [MS-SMB2] Section 3.3.5.5 - Receiving an SMB2 SESSION_SETUP Request
package handlers
