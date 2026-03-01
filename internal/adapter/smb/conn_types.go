package smb

import (
	"net"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
)

// LockedWriter wraps a sync.Mutex for serializing writes to a connection.
// All response-sending functions accept a pointer to this type to ensure
// only one goroutine writes to the connection at a time.
type LockedWriter struct {
	sync.Mutex
}

// ConnInfo provides the connection context needed by dispatch and compound
// request processing functions. This decouples internal/ protocol logic from
// the Connection struct in pkg/adapter/smb/.
//
// Rather than passing the entire Connection, callers construct a ConnInfo
// with only the fields needed by internal functions.
type ConnInfo struct {
	// Conn is the underlying TCP connection (for RemoteAddr).
	Conn net.Conn

	// Handler processes SMB2 protocol operations.
	Handler *handlers.Handler

	// SessionManager provides session and credit management.
	SessionManager *session.Manager

	// WriteMu serializes writes to the connection.
	WriteMu *LockedWriter

	// WriteTimeout for response writes.
	WriteTimeout time.Duration

	// SessionTracker allows dispatch functions to track/untrack sessions
	// on the owning Connection. This is an interface to avoid importing pkg/.
	SessionTracker SessionTracker

	// CryptoState holds per-connection cryptographic negotiation state
	// including the preauth integrity hash chain for SMB 3.1.1.
	CryptoState *ConnectionCryptoState
}

// SessionTracker provides callbacks for session lifecycle tracking.
// The Connection struct in pkg/ implements this interface so that
// dispatch functions can track session creation/deletion without
// depending on the Connection type.
type SessionTracker interface {
	// TrackSession records a session as belonging to the connection.
	TrackSession(sessionID uint64)
	// UntrackSession removes a session from the connection's tracking.
	UntrackSession(sessionID uint64)
}
