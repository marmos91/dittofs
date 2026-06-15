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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
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

	// cryptoState holds per-session cryptographic state (signing keys, signer,
	// encryption/decryption keys). Replaces the old Signing field.
	// For 2.x: HMAC-SHA256 signer. For 3.x: CMAC/GMAC signer + KDF-derived keys.
	//
	// Stored as an atomic pointer: SESSION_SETUP re-authentication
	// (configureSessionSigningWithKey -> SetCryptoState) swaps the whole
	// SessionCryptoState on an already-published session while in-flight
	// requests on the same session read it via ShouldSign / ShouldEncrypt /
	// VerifyMessage / DecryptMessage and the response path. A plain pointer
	// store/load across goroutines is a data race under the Go memory model
	// and can observe a torn or partially-initialised state. Access only via
	// GetCryptoState / SetCryptoState (and the wrapper methods below).
	cryptoState atomic.Pointer[SessionCryptoState]

	// newlyCreated is true for sessions just established via SESSION_SETUP.
	// The framing layer uses this to suppress encryption on the initial
	// SESSION_SETUP SUCCESS response (client hasn't derived keys yet).
	// For re-authenticated sessions this is false, so the response is encrypted
	// with existing keys as expected by the client.
	// Cleared by the framing layer after the first response. Atomic because the
	// per-request dispatch goroutines race the clear against subsequent reads
	// (response.go) — mirrors LoggedOff.
	newlyCreated atomic.Bool

	// ExpiresAt holds the Kerberos ticket end time for Kerberos-authenticated
	// sessions. Zero value means no expiration (NTLM or guest sessions).
	// Checked in the dispatch path to return STATUS_NETWORK_SESSION_EXPIRED.
	ExpiresAt time.Time

	// LoggedOff is set to true by the LOGOFF handler before sending the
	// response. This eliminates a race between the deferred session delete
	// and the next request's signing verification: the verifier and dispatch
	// layer check this flag to return STATUS_USER_SESSION_DELETED instead of
	// attempting signature verification on a defunct session.
	LoggedOff atomic.Bool

	// OriginConnID is the ConnID of the TCP connection that created this
	// session via SESSION_SETUP. For SMB 2.x (below 3.0), session lookup
	// is per-connection per MS-SMB2 §3.3.5.5, so a SESSION_SETUP from a
	// different connection referencing this session's ID must return
	// STATUS_USER_SESSION_DELETED. For SMB 3.x, sessions are global.
	OriginConnID uint64

	// Dialect is the SMB2 dialect negotiated on the origin connection when
	// this session was established. Used by SESSION_SETUP bind validation
	// to enforce dialect-match across channels (MS-SMB2 §3.3.5.5.2; Samba
	// source3/smbd/smb2_sesssetup.c:752-757).
	Dialect types.Dialect

	// SigningAlgo is the signing algorithm ID negotiated on the origin
	// connection. Used by bind to enforce GMAC symmetry between channels
	// (Samba source3/smbd/smb2_sesssetup.c:724-735).
	SigningAlgo uint16

	// CipherId is the encryption cipher negotiated on the origin connection.
	// Used by bind to reject channels whose cipher doesn't match (Samba
	// source3/smbd/smb2_sesssetup.c:759-764).
	CipherId uint16

	// ClientGUID is the SMB2 ClientGuid presented in the NEGOTIATE of the
	// origin connection. Recorded so bind can reject cross-client requests
	// (an attacker holding a SessionID from one client must not be able to
	// bind to it from a different ClientGuid).
	ClientGUID [16]byte

	// PreauthIntegrityHash is the per-session SHA-512 preauth hash captured
	// at the end of the ORIGINAL SESSION_SETUP that established this session
	// (MS-SMB2 §3.3.5.5.3 Session.PreauthIntegrityHashValue). The spec freezes
	// this value once the session is established: on re-authentication the
	// server MUST re-derive Session.SigningKey / EncryptionKey / DecryptionKey
	// from the NEW SessionBaseKey combined with this UNCHANGED preauth hash.
	// Without this snapshot the server would have to reset the per-connection
	// per-session hash entry from the fresh NEGOTIATE hash, producing a
	// different key than the client computes ("Bad SMB2 (sign_algo_id=2)
	// signature" rejection at reauth1-5).
	PreauthIntegrityHash [64]byte

	// PeerUsedEncryption is sticky-set to true the first time we receive an
	// encrypted (Transform Header) request on this session. Per MS-SMB2
	// §3.3.4.1.4 the response to an encrypted request MUST be encrypted; the
	// synchronous response path already honours this via SMBHandlerContext
	// .RequestEncrypted, but ASYNC completions (e.g. CHANGE_NOTIFY
	// CANCELLED) lose that context once they're dispatched from the registry
	// goroutine. Reading this sticky flag in SendAsyncChangeNotifyResponse /
	// SendAsyncCompletionResponse lets them mirror the peer's encryption
	// stance even when Session.EncryptData stays false (preferred mode
	// without per-share enforcement). Smbtorture's
	// encryption-aes-128-{ccm,gcm} forces encryption per-connection but does
	// not set the session flag, so this is the only signal we have.
	PeerUsedEncryption atomic.Bool

	// Credit tracking
	credits Credits

	// mu protects credit mutation operations
	mu sync.Mutex

	// channels holds TCP connections explicitly bound to this session, keyed
	// by ConnID. Only secondary (bound) connections are currently
	// registered via SESSION_SETUP with SMB2_SESSION_FLAG_BINDING per
	// MS-SMB2 §3.3.5.5.2; the original connection continues to use the
	// session-level Signer via the dispatch fallback. All channels share
	// the session key but each derives its own signing key. Access via
	// AddChannel / GetChannel / RemoveChannel / ListChannels (channel.go).
	//
	// A plain map + RWMutex (rather than sync.Map) is used so the cap
	// enforcement in AddChannel — "count < MaxChannelsPerSession, then
	// insert" — is atomic under concurrent binds.
	channelsMu sync.RWMutex
	channels   map[uint64]*Channel
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
		channels:   make(map[uint64]*Channel),
	}
	s.cryptoState.Store(&SessionCryptoState{})
	s.newlyCreated.Store(true)
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
		channels:   make(map[uint64]*Channel),
	}
	s.cryptoState.Store(&SessionCryptoState{})
	s.newlyCreated.Store(true)
	s.credits.LastActivity.Store(time.Now().Unix())
	return s
}

// SetBindIdentity records the negotiated dialect, signing algorithm, cipher,
// and client GUID of the origin connection so subsequent SESSION_SETUP bind
// requests can validate that the new channel's negotiated parameters match
// the session (MS-SMB2 §3.3.5.5.2). Safe to call on any session; for SMB 2.x
// only Dialect is meaningful but it's harmless to set the rest.
func (s *Session) SetBindIdentity(dialect types.Dialect, signingAlgo uint16, cipherId uint16, clientGUID [16]byte) {
	s.Dialect = dialect
	s.SigningAlgo = signingAlgo
	s.CipherId = cipherId
	s.ClientGUID = clientGUID
}

// IsExpired returns true if the session has a Kerberos ticket that has expired.
func (s *Session) IsExpired() bool {
	return !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt)
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

// GetCryptoState returns the session's current cryptographic state via an
// atomic load. May return nil before the state is initialised. All readers
// MUST use this accessor (never the field) so the SESSION_SETUP re-auth swap
// in SetCryptoState is observed atomically.
func (s *Session) GetCryptoState() *SessionCryptoState {
	if s == nil {
		return nil
	}
	return s.cryptoState.Load()
}

// IsNewlyCreated reports whether this session was just established via
// SESSION_SETUP and has not yet had its first response sent. The framing
// layer uses it to suppress encryption on the initial SESSION_SETUP SUCCESS
// response. Atomic load — the clear (ClearNewlyCreated) races per-request
// dispatch goroutines.
func (s *Session) IsNewlyCreated() bool {
	return s.newlyCreated.Load()
}

// ClearNewlyCreated marks the session as no longer newly created, so
// subsequent responses are encrypted with the now-derived keys.
func (s *Session) ClearNewlyCreated() {
	s.newlyCreated.Store(false)
}

// SetSigningKey sets the signing key from the session key.
// This creates a CryptoState with an HMACSigner for SMB 2.x sessions.
// For 3.x sessions, use SetCryptoState with DeriveAllKeys instead.
func (s *Session) SetSigningKey(sessionKey []byte) {
	s.cryptoState.Store(DeriveAllKeys(sessionKey, types.Dialect0202, [64]byte{}, 0, signing.SigningAlgHMACSHA256, false))
}

// EnableSigning enables message signing for this session.
//
// The current crypto state is copied, the signing flags are set on the copy,
// and the copy is published atomically. Mutating the published state in place
// would race in-flight readers that loaded the same pointer.
func (s *Session) EnableSigning(required bool) {
	cur := s.cryptoState.Load()
	if cur == nil {
		return
	}
	updated := *cur
	updated.SigningEnabled = true
	updated.SigningRequired = required
	s.cryptoState.Store(&updated)
}

// SetCryptoState sets the session's cryptographic state directly.
// Used by session setup when KDF-derived keys are available (3.x sessions).
func (s *Session) SetCryptoState(cs *SessionCryptoState) {
	s.cryptoState.Store(cs)
}

// ShouldEncrypt returns true if outgoing messages should be encrypted.
func (s *Session) ShouldEncrypt() bool {
	return s.cryptoState.Load().ShouldEncrypt()
}

// EncryptWithNonce encrypts plaintext using a pre-generated nonce and AAD.
// Used by the encryption middleware which needs to control the nonce.
func (s *Session) EncryptWithNonce(nonce, plaintext, aad []byte) ([]byte, error) {
	cs := s.cryptoState.Load()
	if cs == nil || cs.Encryptor == nil {
		return nil, fmt.Errorf("session has no encryptor")
	}
	return cs.Encryptor.EncryptWithNonce(nonce, plaintext, aad)
}

// DecryptMessage decrypts ciphertext using the given nonce and AAD.
func (s *Session) DecryptMessage(nonce, ciphertext, aad []byte) ([]byte, error) {
	cs := s.cryptoState.Load()
	if cs == nil || cs.Decryptor == nil {
		return nil, fmt.Errorf("session has no decryptor")
	}
	return cs.Decryptor.Decrypt(nonce, ciphertext, aad)
}

func (s *Session) EncryptorNonceSize() int {
	cs := s.cryptoState.Load()
	if cs == nil || cs.Encryptor == nil {
		return 0
	}
	return cs.Encryptor.NonceSize()
}

func (s *Session) DecryptorNonceSize() int {
	cs := s.cryptoState.Load()
	if cs == nil || cs.Decryptor == nil {
		return 0
	}
	return cs.Decryptor.NonceSize()
}

func (s *Session) EncryptorOverhead() int {
	cs := s.cryptoState.Load()
	if cs == nil || cs.Encryptor == nil {
		return 0
	}
	return cs.Encryptor.Overhead()
}

// IsNullSession reports whether this session was created with anonymous
// NTLM credentials (SMB2_SESSION_FLAG_IS_NULL). Mirrors Session.IsNull but
// is exposed through the encryption.EncryptableSession interface to
// avoid leaking the full Session type across packages.
func (s *Session) IsNullSession() bool {
	return s != nil && s.IsNull
}

// ShouldSign returns true if outgoing messages should be signed.
func (s *Session) ShouldSign() bool {
	return s.cryptoState.Load().ShouldSign()
}

// ShouldVerify returns true if incoming messages should have signatures verified.
func (s *Session) ShouldVerify() bool {
	return s.cryptoState.Load().ShouldVerify()
}

// SignMessage signs an SMB2 message in place using the session's signer.
// This should be called before sending a message if signing is enabled.
func (s *Session) SignMessage(message []byte) {
	cs := s.cryptoState.Load()
	if cs.ShouldSign() {
		signing.SignMessage(cs.Signer, message)
	}
}

// SignMessageOnChannel signs an outgoing SMB2 message using the per-channel
// signer when connID is bound via SMB2 session binding (MS-SMB2 §3.3.5.5.2),
// and the session-level signer otherwise. Safe to call when the session does
// not have signing enabled — it becomes a no-op.
func (s *Session) SignMessageOnChannel(connID uint64, message []byte) {
	if ch := s.GetChannel(connID); ch != nil && ch.Signer != nil {
		signing.SignMessage(ch.Signer, message)
		return
	}
	s.SignMessage(message)
}

// VerifyMessage verifies the signature of an SMB2 message.
// Returns true if the signature is valid or if signing is not enabled.
func (s *Session) VerifyMessage(message []byte) bool {
	cs := s.cryptoState.Load()
	if !cs.ShouldVerify() {
		return true
	}
	return cs.Signer.Verify(message)
}

// VerifyMessageOnChannel verifies an incoming message's signature against the
// per-channel signing key if connID is bound via SMB2 session binding
// (MS-SMB2 §3.3.5.5.2); otherwise falls back to the session-level key.
func (s *Session) VerifyMessageOnChannel(connID uint64, message []byte) bool {
	if ch := s.GetChannel(connID); ch != nil && ch.Signer != nil {
		return ch.Signer.Verify(message)
	}
	return s.VerifyMessage(message)
}
