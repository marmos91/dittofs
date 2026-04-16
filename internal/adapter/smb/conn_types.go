package smb

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/encryption"
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

	// EncryptionMiddleware handles transparent encrypt/decrypt of SMB3 messages.
	// Nil when encryption is not configured or not yet negotiated.
	EncryptionMiddleware encryption.EncryptionMiddleware

	// DecryptFailures tracks consecutive decryption failures for this connection.
	// After 5 consecutive failures, the connection is dropped per security best practice.
	// Reset to 0 on each successful decrypt.
	DecryptFailures *atomic.Int32

	// SequenceWindow tracks granted MessageIds per MS-SMB2 3.3.1.1.
	// Initialized with {0} on connection establishment.
	// Expanded by credit grants on every response.
	SequenceWindow *session.CommandSequenceWindow

	// SupportsMultiCredit is true for SMB 2.1+ connections.
	// Set during NEGOTIATE based on negotiated dialect.
	// When false, CreditCharge payload validation is skipped (SMB 2.0.2 compat).
	SupportsMultiCredit bool
}

// SessionTracker provides callbacks for session lifecycle tracking.
// The Connection struct in pkg/ implements this interface so that
// dispatch functions can track session creation/deletion without
// depending on the Connection type.
type SessionTracker interface {
	TrackSession(sessionID uint64)
	UntrackSession(sessionID uint64)
}

// NewSequenceWindowForConnection creates a CommandSequenceWindow sized for the
// given session manager's credit configuration. The window's maxSize is set
// to MaxSessionCredits — the same cap Samba uses (`smb2 max credits`,
// source3/smbd/smb2_server.c credits.max = lp_smb2_max_credits()). The Samba
// client hard-caps its own per-connection cur_credits counter at uint16 max
// (libcli/smb/smbXcli_base.c:4295–4298), so this cap protects against
// INVALID_NETWORK_RESPONSE — issue #378.
func NewSequenceWindowForConnection(mgr *session.Manager) *session.CommandSequenceWindow {
	maxSize := uint64(8192) // Samba default
	if mgr != nil {
		maxSize = uint64(mgr.Config().MaxSessionCredits)
	}
	return session.NewCommandSequenceWindow(maxSize)
}
