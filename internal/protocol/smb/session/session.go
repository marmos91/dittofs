// Package session provides unified session management for SMB2 protocol.
//
// This package combines session identity (authentication state, username) with
// credit tracking into a single source of truth. It eliminates the dual ownership
// problem where Handler.sessions and CreditManager.sessions tracked sessions
// independently.
//
// # Architecture
//
// The package provides:
//   - Session: Combines identity (username, guest status) with credit accounting
//   - SessionManager: Manages session lifecycle and provides credit operations
//
// # Usage
//
// Create a SessionManager during server initialization:
//
//	mgr := session.NewManager(session.DefaultCreditConfig())
//
// Create sessions during SESSION_SETUP:
//
//	sess := mgr.CreateSession(clientAddr, true, "guest", "")
//
// Track credits during request processing:
//
//	mgr.RequestStarted(sessionID)
//	defer mgr.RequestCompleted(sessionID)
//	credits := mgr.GrantCredits(sessionID, requested, creditCharge)
//
// Clean up on LOGOFF:
//
//	mgr.DeleteSession(sessionID)
//
// # Thread Safety
//
// All SessionManager methods are safe for concurrent use.
// Session methods that modify credit state use internal synchronization.
package session

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/smb/signing"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Session represents an SMB2 session with both identity and credit tracking.
//
// A session is created during SESSION_SETUP and destroyed on LOGOFF or
// connection close. Each session has:
//   - Identity: username, guest/null status, creation info
//   - Credits: adaptive flow control accounting
//   - Signing: message signing state and key
//
// Thread safety:
// Session fields are read-only after creation, except for credit fields
// which are protected by internal synchronization.
type Session struct {
	// Identity fields (read-only after creation)
	SessionID  uint64
	IsGuest    bool
	IsNull     bool
	CreatedAt  time.Time
	ClientAddr string
	Username   string
	Domain     string

	// DittoFS user (nil for guest sessions)
	// This links the SMB session to the authenticated DittoFS user
	// for permission checking and share access control.
	User *models.User

	// Signing state for message integrity
	// This tracks whether signing is enabled and holds the signing key.
	Signing *signing.SessionSigningState

	// Credit tracking
	credits Credits

	// mu protects credit mutation operations
	mu sync.Mutex
}

// Credits tracks credit accounting for a session.
//
// SMB2 uses credit-based flow control to limit outstanding requests.
// Each credit allows one request or 64KB of data transfer.
type Credits struct {
	// Cumulative accounting
	Granted  uint32 // Total credits ever granted
	Consumed uint32 // Total credits ever consumed

	// Current state
	Outstanding int32 // Current balance (Granted - Consumed - Returned)

	// Request tracking for adaptive algorithm
	OutstandingRequests atomic.Int64  // Currently processing requests
	TotalRequests       atomic.Uint64 // Total requests ever processed
	LastActivity        atomic.Int64  // Unix timestamp of last activity

	// Monitoring
	HighWaterMark uint32 // Maximum Outstanding ever reached
}

// NewSession creates a new session with the given identity.
// Called internally by SessionManager.CreateSession.
func NewSession(sessionID uint64, clientAddr string, isGuest bool, username, domain string) *Session {
	s := &Session{
		SessionID:  sessionID,
		IsGuest:    isGuest,
		IsNull:     username == "" && !isGuest,
		CreatedAt:  time.Now(),
		ClientAddr: clientAddr,
		Username:   username,
		Domain:     domain,
		Signing:    signing.NewSessionSigningState(),
	}
	s.credits.LastActivity.Store(time.Now().Unix())
	return s
}

// NewSessionWithUser creates a new session with the given identity and DittoFS user.
// Use this when the user has been authenticated against the UserStore.
func NewSessionWithUser(sessionID uint64, clientAddr string, user *models.User, domain string) *Session {
	s := &Session{
		SessionID:  sessionID,
		IsGuest:    false,
		IsNull:     false,
		CreatedAt:  time.Now(),
		ClientAddr: clientAddr,
		Username:   user.Username,
		Domain:     domain,
		User:       user,
		Signing:    signing.NewSessionSigningState(),
	}
	s.credits.LastActivity.Store(time.Now().Unix())
	return s
}

// RequestStarted records that a request has started processing.
// Should be called at the start of each request handler.
func (s *Session) RequestStarted() {
	s.credits.OutstandingRequests.Add(1)
	s.credits.TotalRequests.Add(1)
	s.credits.LastActivity.Store(time.Now().Unix())
}

// RequestCompleted records that a request has finished processing.
// Should be called when each request handler completes.
func (s *Session) RequestCompleted() {
	s.credits.OutstandingRequests.Add(-1)
}

// ConsumeCredits records credit consumption for an operation.
// Called when processing a request that charges credits.
func (s *Session) ConsumeCredits(charge uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.credits.Consumed += uint32(charge)
	s.credits.Outstanding -= int32(charge)
}

// GrantCredits records credits granted in a response.
// Returns the updated outstanding balance.
func (s *Session) GrantCredits(grant uint16) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.credits.Granted += uint32(grant)
	s.credits.Outstanding += int32(grant)

	// Update high water mark
	if s.credits.Outstanding > 0 && uint32(s.credits.Outstanding) > s.credits.HighWaterMark {
		s.credits.HighWaterMark = uint32(s.credits.Outstanding)
	}

	return s.credits.Outstanding
}

// GetOutstanding returns the current outstanding credit balance.
func (s *Session) GetOutstanding() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credits.Outstanding
}

// GetOutstandingRequests returns the number of currently processing requests.
func (s *Session) GetOutstandingRequests() int64 {
	return s.credits.OutstandingRequests.Load()
}

// GetHighWaterMark returns the maximum outstanding credits ever reached.
func (s *Session) GetHighWaterMark() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credits.HighWaterMark
}

// GetStats returns a snapshot of session credit statistics.
func (s *Session) GetStats() SessionStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	return SessionStats{
		SessionID:           s.SessionID,
		Granted:             s.credits.Granted,
		Consumed:            s.credits.Consumed,
		Outstanding:         s.credits.Outstanding,
		OutstandingRequests: s.credits.OutstandingRequests.Load(),
		TotalRequests:       s.credits.TotalRequests.Load(),
		HighWaterMark:       s.credits.HighWaterMark,
	}
}

// SessionStats contains a snapshot of session credit statistics.
type SessionStats struct {
	SessionID           uint64
	Granted             uint32
	Consumed            uint32
	Outstanding         int32
	OutstandingRequests int64
	TotalRequests       uint64
	HighWaterMark       uint32
}

// =============================================================================
// Signing Methods
// =============================================================================

// SetSigningKey sets the signing key from the session key.
// This should be called after successful authentication when the session key
// is derived from the NTLM authentication exchange.
func (s *Session) SetSigningKey(sessionKey []byte) {
	if s.Signing != nil {
		s.Signing.SetSessionKey(sessionKey)
	}
}

// EnableSigning enables message signing for this session.
// After calling this, all messages should be signed/verified.
func (s *Session) EnableSigning(required bool) {
	if s.Signing != nil {
		s.Signing.SigningEnabled = true
		s.Signing.SigningRequired = required
	}
}

// ShouldSign returns true if outgoing messages should be signed.
func (s *Session) ShouldSign() bool {
	return s.Signing != nil && s.Signing.ShouldSign()
}

// ShouldVerify returns true if incoming messages should have signatures verified.
func (s *Session) ShouldVerify() bool {
	return s.Signing != nil && s.Signing.ShouldVerify()
}

// SignMessage signs an SMB2 message in place using the session's signing key.
// This should be called before sending a message if signing is enabled.
func (s *Session) SignMessage(message []byte) {
	if s.Signing != nil && s.Signing.ShouldSign() && s.Signing.SigningKey != nil {
		s.Signing.SigningKey.SignMessage(message)
	}
}

// VerifyMessage verifies the signature of an SMB2 message.
// Returns true if the signature is valid or if signing is not enabled.
func (s *Session) VerifyMessage(message []byte) bool {
	if s.Signing == nil || !s.Signing.ShouldVerify() || s.Signing.SigningKey == nil {
		return true // No verification needed
	}
	return s.Signing.SigningKey.Verify(message)
}
