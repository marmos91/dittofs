package smb

import (
	"crypto/sha512"
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// ConnectionCryptoState holds per-connection cryptographic state for SMB 3.1.1
// dialect negotiation and the preauth integrity hash chain.
//
// The preauth integrity hash chain is defined in [MS-SMB2] Section 3.2.5.2:
//
//	H(i) = SHA-512(H(i-1) || Message(i))
//
// where H(0) is 64 bytes of zeros, and each Message(i) is a complete SMB2
// NEGOTIATE or SESSION_SETUP request/response.
//
// Fields are set-once during negotiation (Dialect, CipherId, etc.) and the
// preauth hash is updated with a mutex for concurrent safety.
type ConnectionCryptoState struct {
	// Negotiated dialect (set during NEGOTIATE)
	Dialect types.Dialect

	// Selected cipher for encryption (set during NEGOTIATE for 3.1.1)
	CipherId uint16

	// Selected signing algorithm (set during NEGOTIATE)
	SigningAlgorithmId uint16

	// Server's GUID (set during NEGOTIATE)
	ServerGUID [16]byte

	// Server's capabilities (set during NEGOTIATE)
	ServerCapabilities types.Capabilities

	// Server's security mode (set during NEGOTIATE)
	ServerSecurityMode types.SecurityMode

	// Client's capabilities (captured from NEGOTIATE request)
	ClientCapabilities types.Capabilities

	// Client's GUID (captured from NEGOTIATE request)
	ClientGUID [16]byte

	// Client's security mode (captured from NEGOTIATE request)
	ClientSecurityMode types.SecurityMode

	// Client's offered dialects (captured from NEGOTIATE request)
	ClientDialects []types.Dialect

	// Selected preauth integrity hash algorithm ID (set during NEGOTIATE)
	PreauthIntegrityHashId uint16

	// hashMu protects the preauthHash field for concurrent access.
	hashMu sync.RWMutex

	// preauthHash is the current preauth integrity hash value.
	// H(0) = 64 zero bytes (initialized by NewConnectionCryptoState).
	preauthHash [64]byte
}

// NewConnectionCryptoState creates a new ConnectionCryptoState with all
// fields zeroed. H(0) is 64 zero bytes per the MS-SMB2 specification.
func NewConnectionCryptoState() *ConnectionCryptoState {
	return &ConnectionCryptoState{}
}

// UpdatePreauthHash updates the preauth integrity hash chain:
//
//	H(i) = SHA-512(H(i-1) || message)
//
// This MUST be called with the complete SMB2 message (header + body) for
// each NEGOTIATE and SESSION_SETUP request/response.
//
// Thread-safe: acquires write lock on the hash.
func (cs *ConnectionCryptoState) UpdatePreauthHash(message []byte) {
	cs.hashMu.Lock()
	defer cs.hashMu.Unlock()

	h := sha512.New()
	h.Write(cs.preauthHash[:])
	h.Write(message)
	copy(cs.preauthHash[:], h.Sum(nil))
}

// GetPreauthHash returns a copy of the current preauth integrity hash value.
//
// Thread-safe: acquires read lock on the hash.
func (cs *ConnectionCryptoState) GetPreauthHash() [64]byte {
	cs.hashMu.RLock()
	defer cs.hashMu.RUnlock()
	return cs.preauthHash
}

// ============================================================================
// CryptoState interface implementation
// ============================================================================
// These methods satisfy the handlers.CryptoState interface, allowing the
// negotiate handler to update connection-level crypto state without importing
// the internal/adapter/smb package (which would create a circular import).

// SetDialect records the negotiated dialect on the connection.
func (cs *ConnectionCryptoState) SetDialect(d types.Dialect) { cs.Dialect = d }

// GetDialect returns the negotiated dialect.
func (cs *ConnectionCryptoState) GetDialect() types.Dialect { return cs.Dialect }

// SetCipherId records the selected encryption cipher.
func (cs *ConnectionCryptoState) SetCipherId(id uint16) { cs.CipherId = id }

// SetPreauthIntegrityHashId records the selected hash algorithm.
func (cs *ConnectionCryptoState) SetPreauthIntegrityHashId(id uint16) {
	cs.PreauthIntegrityHashId = id
}

// SetServerGUID records the server's GUID.
func (cs *ConnectionCryptoState) SetServerGUID(guid [16]byte) { cs.ServerGUID = guid }

// GetServerGUID returns the server's GUID.
func (cs *ConnectionCryptoState) GetServerGUID() [16]byte { return cs.ServerGUID }

// SetServerCapabilities records the server's negotiated capabilities.
func (cs *ConnectionCryptoState) SetServerCapabilities(caps types.Capabilities) {
	cs.ServerCapabilities = caps
}

// GetServerCapabilities returns the server's negotiated capabilities.
func (cs *ConnectionCryptoState) GetServerCapabilities() types.Capabilities {
	return cs.ServerCapabilities
}

// SetServerSecurityMode records the server's security mode.
func (cs *ConnectionCryptoState) SetServerSecurityMode(mode types.SecurityMode) {
	cs.ServerSecurityMode = mode
}

// GetServerSecurityMode returns the server's security mode.
func (cs *ConnectionCryptoState) GetServerSecurityMode() types.SecurityMode {
	return cs.ServerSecurityMode
}

// SetClientGUID records the client's GUID from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientGUID(guid [16]byte) { cs.ClientGUID = guid }

// GetClientGUID returns the client's GUID.
func (cs *ConnectionCryptoState) GetClientGUID() [16]byte { return cs.ClientGUID }

// SetClientCapabilities records the client's capabilities from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientCapabilities(caps types.Capabilities) {
	cs.ClientCapabilities = caps
}

// GetClientCapabilities returns the client's capabilities.
func (cs *ConnectionCryptoState) GetClientCapabilities() types.Capabilities {
	return cs.ClientCapabilities
}

// SetClientSecurityMode records the client's security mode from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientSecurityMode(mode types.SecurityMode) {
	cs.ClientSecurityMode = mode
}

// GetClientSecurityMode returns the client's security mode.
func (cs *ConnectionCryptoState) GetClientSecurityMode() types.SecurityMode {
	return cs.ClientSecurityMode
}

// SetClientDialects records the client's offered dialect list from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientDialects(dialects []types.Dialect) {
	cs.ClientDialects = dialects
}

// GetClientDialects returns the client's offered dialect list.
func (cs *ConnectionCryptoState) GetClientDialects() []types.Dialect {
	return cs.ClientDialects
}
