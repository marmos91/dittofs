package smb

import (
	"crypto/sha512"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
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
// Per [MS-SMB2] 3.3.5.5: each session has its own preauth hash chain
// (PreauthSessionTable) initialized from the connection hash after NEGOTIATE.
// SESSION_SETUP messages update per-session hashes, not the connection hash.
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

	// mu protects all negotiation fields for concurrent access.
	mu sync.RWMutex

	// preauthHash is the connection-level preauth integrity hash value.
	// Updated only by NEGOTIATE messages. H(0) = 64 zero bytes.
	preauthHash [64]byte

	// sessionPreauthHashes holds per-session preauth integrity hash values.
	// Per [MS-SMB2] 3.3.5.5, each session tracks its own preauth hash
	// chain initialized from the connection hash after NEGOTIATE completes.
	// Key: session ID, Value: current preauth hash for that session.
	sessionPreauthHashes map[uint64][64]byte

}

// NewConnectionCryptoState creates a new ConnectionCryptoState with all
// fields zeroed. H(0) is 64 zero bytes per the MS-SMB2 specification.
func NewConnectionCryptoState() *ConnectionCryptoState {
	return &ConnectionCryptoState{
		sessionPreauthHashes: make(map[uint64][64]byte),
	}
}

// chainHash computes the next value in a preauth integrity hash chain:
//
//	H(i) = SHA-512(H(i-1) || message)
//
// This is the core computation shared by connection-level and per-session
// preauth hash updates.
func chainHash(current [64]byte, message []byte) [64]byte {
	h := sha512.New()
	h.Write(current[:])
	h.Write(message)
	var result [64]byte
	copy(result[:], h.Sum(nil))
	return result
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
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.preauthHash = chainHash(cs.preauthHash, message)
}

// GetPreauthHash returns a copy of the current preauth integrity hash value.
//
// Thread-safe: acquires read lock on the hash.
func (cs *ConnectionCryptoState) GetPreauthHash() [64]byte {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.preauthHash
}

// InitSessionPreauthHash creates a new per-session preauth hash entry
// initialized from the current connection-level preauth hash, then chains
// the SESSION_SETUP request bytes into it.
//
// Per [MS-SMB2] 3.3.5.5: "Connection.PreauthSession.PreauthIntegrityHashValue
// MUST be set to Connection.PreauthIntegrityHashValue."
//
// `ssRequestBytes` MUST be the rawMessage from THIS request. The previous
// implementation pulled them from a per-connection single-slot stash, which
// raced when multiple SESSION_SETUPs were dispatched concurrently on the
// same connection (issue #362 — "Bad SMB2 (sign_algo_id=2) signature").
//
// Thread-safe: acquires write lock.
func (cs *ConnectionCryptoState) InitSessionPreauthHash(sessionID uint64, ssRequestBytes []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Start from connection hash (post-NEGOTIATE)
	cs.sessionPreauthHashes[sessionID] = cs.preauthHash

	if len(ssRequestBytes) > 0 {
		updated := chainHash(cs.sessionPreauthHashes[sessionID], ssRequestBytes)
		cs.sessionPreauthHashes[sessionID] = updated
		logger.Debug("InitSessionPreauthHash: chained SESSION_SETUP request",
			"sessionID", sessionID,
			"hashPrefix", fmt.Sprintf("%x", updated[:16]),
			"connHashPrefix", fmt.Sprintf("%x", cs.preauthHash[:16]))
	}
}

// UpdateSessionPreauthHash updates the per-session preauth hash for the given
// session ID with the provided message bytes.
//
//	H(i) = SHA-512(H(i-1) || message)
//
// No-op if the session ID is not in the table.
//
// Thread-safe: acquires write lock.
func (cs *ConnectionCryptoState) UpdateSessionPreauthHash(sessionID uint64, message []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	current, ok := cs.sessionPreauthHashes[sessionID]
	if !ok {
		return
	}
	cs.sessionPreauthHashes[sessionID] = chainHash(current, message)
}

// GetSessionPreauthHash returns a copy of the per-session preauth hash for
// the given session ID. Returns the connection-level hash as fallback if the
// session is not in the table (e.g., non-3.1.1 dialects).
//
// Thread-safe: acquires read lock.
func (cs *ConnectionCryptoState) GetSessionPreauthHash(sessionID uint64) [64]byte {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if h, ok := cs.sessionPreauthHashes[sessionID]; ok {
		return h
	}
	return cs.preauthHash
}

// DeleteSessionPreauthHash removes the per-session preauth hash entry.
// Called after session setup completes (keys derived) to free memory.
//
// Thread-safe: acquires write lock.
func (cs *ConnectionCryptoState) DeleteSessionPreauthHash(sessionID uint64) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.sessionPreauthHashes, sessionID)
}

// ============================================================================
// CryptoState interface implementation
// ============================================================================
// These methods satisfy the handlers.CryptoState interface, allowing the
// negotiate handler to update connection-level crypto state without importing
// the internal/adapter/smb package (which would create a circular import).

// SetDialect records the negotiated dialect on the connection.
func (cs *ConnectionCryptoState) SetDialect(d types.Dialect) {
	cs.mu.Lock()
	cs.Dialect = d
	cs.mu.Unlock()
}

// GetDialect returns the negotiated dialect.
func (cs *ConnectionCryptoState) GetDialect() types.Dialect {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.Dialect
}

// SetCipherId records the selected encryption cipher.
func (cs *ConnectionCryptoState) SetCipherId(id uint16) {
	cs.mu.Lock()
	cs.CipherId = id
	cs.mu.Unlock()
}

// GetCipherId returns the selected encryption cipher.
func (cs *ConnectionCryptoState) GetCipherId() uint16 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.CipherId
}

// SetSigningAlgorithmId records the selected signing algorithm.
func (cs *ConnectionCryptoState) SetSigningAlgorithmId(id uint16) {
	cs.mu.Lock()
	cs.SigningAlgorithmId = id
	cs.mu.Unlock()
}

// GetSigningAlgorithmId returns the selected signing algorithm.
func (cs *ConnectionCryptoState) GetSigningAlgorithmId() uint16 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.SigningAlgorithmId
}

// SetPreauthIntegrityHashId records the selected hash algorithm.
func (cs *ConnectionCryptoState) SetPreauthIntegrityHashId(id uint16) {
	cs.mu.Lock()
	cs.PreauthIntegrityHashId = id
	cs.mu.Unlock()
}

// SetServerGUID records the server's GUID.
func (cs *ConnectionCryptoState) SetServerGUID(guid [16]byte) {
	cs.mu.Lock()
	cs.ServerGUID = guid
	cs.mu.Unlock()
}

// GetServerGUID returns the server's GUID.
func (cs *ConnectionCryptoState) GetServerGUID() [16]byte {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ServerGUID
}

// SetServerCapabilities records the server's negotiated capabilities.
func (cs *ConnectionCryptoState) SetServerCapabilities(caps types.Capabilities) {
	cs.mu.Lock()
	cs.ServerCapabilities = caps
	cs.mu.Unlock()
}

// GetServerCapabilities returns the server's negotiated capabilities.
func (cs *ConnectionCryptoState) GetServerCapabilities() types.Capabilities {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ServerCapabilities
}

// SetServerSecurityMode records the server's security mode.
func (cs *ConnectionCryptoState) SetServerSecurityMode(mode types.SecurityMode) {
	cs.mu.Lock()
	cs.ServerSecurityMode = mode
	cs.mu.Unlock()
}

// GetServerSecurityMode returns the server's security mode.
func (cs *ConnectionCryptoState) GetServerSecurityMode() types.SecurityMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ServerSecurityMode
}

// SetClientGUID records the client's GUID from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientGUID(guid [16]byte) {
	cs.mu.Lock()
	cs.ClientGUID = guid
	cs.mu.Unlock()
}

// GetClientGUID returns the client's GUID.
func (cs *ConnectionCryptoState) GetClientGUID() [16]byte {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ClientGUID
}

// SetClientCapabilities records the client's capabilities from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientCapabilities(caps types.Capabilities) {
	cs.mu.Lock()
	cs.ClientCapabilities = caps
	cs.mu.Unlock()
}

// GetClientCapabilities returns the client's capabilities.
func (cs *ConnectionCryptoState) GetClientCapabilities() types.Capabilities {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ClientCapabilities
}

// SetClientSecurityMode records the client's security mode from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientSecurityMode(mode types.SecurityMode) {
	cs.mu.Lock()
	cs.ClientSecurityMode = mode
	cs.mu.Unlock()
}

// GetClientSecurityMode returns the client's security mode.
func (cs *ConnectionCryptoState) GetClientSecurityMode() types.SecurityMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ClientSecurityMode
}

// SetClientDialects records the client's offered dialect list from the NEGOTIATE request.
func (cs *ConnectionCryptoState) SetClientDialects(dialects []types.Dialect) {
	cp := make([]types.Dialect, len(dialects))
	copy(cp, dialects)
	cs.mu.Lock()
	cs.ClientDialects = cp
	cs.mu.Unlock()
}

// GetClientDialects returns a copy of the client's offered dialect list.
func (cs *ConnectionCryptoState) GetClientDialects() []types.Dialect {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	cp := make([]types.Dialect, len(cs.ClientDialects))
	copy(cp, cs.ClientDialects)
	return cp
}
