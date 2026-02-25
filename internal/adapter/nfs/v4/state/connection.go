package state

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// Connection Direction and Type
// ============================================================================

// ConnectionDirection represents the channel direction of a bound connection.
type ConnectionDirection uint8

const (
	// ConnDirFore indicates a fore-channel-only connection.
	ConnDirFore ConnectionDirection = iota + 1
	// ConnDirBack indicates a back-channel-only connection.
	ConnDirBack
	// ConnDirBoth indicates a connection used for both fore and back channels.
	ConnDirBoth
)

// String returns a human-readable representation of the connection direction.
func (d ConnectionDirection) String() string {
	switch d {
	case ConnDirFore:
		return "fore"
	case ConnDirBack:
		return "back"
	case ConnDirBoth:
		return "both"
	default:
		return fmt.Sprintf("unknown(%d)", d)
	}
}

// ConnectionType represents the transport type of a bound connection.
type ConnectionType uint8

const (
	// ConnTypeTCP indicates a TCP transport connection.
	ConnTypeTCP ConnectionType = iota
	// ConnTypeRDMA indicates an RDMA transport connection.
	ConnTypeRDMA
)

// String returns a human-readable representation of the connection type.
func (t ConnectionType) String() string {
	switch t {
	case ConnTypeTCP:
		return "TCP"
	case ConnTypeRDMA:
		return "RDMA"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}

// ============================================================================
// BoundConnection
// ============================================================================

// BoundConnection represents a TCP connection that has been bound to a session
// via BIND_CONN_TO_SESSION (RFC 8881 Section 18.34) or auto-bound during
// CREATE_SESSION.
type BoundConnection struct {
	// ConnectionID is the unique identifier assigned at TCP accept() time.
	ConnectionID uint64

	// SessionID is the session this connection is bound to.
	SessionID types.SessionId4

	// Direction is the channel direction for this binding.
	Direction ConnectionDirection

	// ConnType is the transport type (TCP or RDMA).
	ConnType ConnectionType

	// BoundAt is the time this connection was bound to the session.
	BoundAt time.Time

	// LastActivity is the time of the last request on this connection.
	LastActivity time.Time

	// Draining indicates the connection is being drained (no new requests).
	// When true, the server returns NFS4ERR_DELAY for new COMPOUND requests.
	Draining bool
}

// ============================================================================
// BindConnResult
// ============================================================================

// BindConnResult holds the result of a successful BindConnToSession operation.
type BindConnResult struct {
	// ServerDir is the server-assigned channel direction (CDFS4_FORE, CDFS4_BACK, CDFS4_BOTH).
	ServerDir uint32
}

// ============================================================================
// Direction Negotiation
// ============================================================================

// negotiateDirection implements the generous direction negotiation policy
// per RFC 8881 Section 18.34.
//
// The server grants the most permissive direction compatible with the
// client's request:
//   - CDFC4_FORE -> ConnDirFore, CDFS4_FORE
//   - CDFC4_BACK -> ConnDirBack, CDFS4_BACK
//   - CDFC4_FORE_OR_BOTH -> ConnDirBoth, CDFS4_BOTH
//   - CDFC4_BACK_OR_BOTH -> ConnDirBoth, CDFS4_BOTH
//   - default -> ConnDirFore, CDFS4_FORE
func negotiateDirection(clientDir uint32) (ConnectionDirection, uint32) {
	switch clientDir {
	case types.CDFC4_FORE:
		return ConnDirFore, types.CDFS4_FORE
	case types.CDFC4_BACK:
		return ConnDirBack, types.CDFS4_BACK
	case types.CDFC4_FORE_OR_BOTH:
		return ConnDirBoth, types.CDFS4_BOTH
	case types.CDFC4_BACK_OR_BOTH:
		return ConnDirBoth, types.CDFS4_BOTH
	default:
		return ConnDirFore, types.CDFS4_FORE
	}
}
