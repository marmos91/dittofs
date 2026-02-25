// Package auth provides authentication for SMB protocol adapters.
//
// This file implements the adapter.Authenticator interface for SMB,
// wrapping the existing NTLM and SPNEGO authentication mechanisms.
// The SMBAuthenticator bridges SPNEGO-wrapped NTLM/Kerberos tokens
// to DittoFS's identity model via the control plane UserStore.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// SMBAuthenticator implements adapter.Authenticator for SMB protocol authentication.
//
// It handles SPNEGO-wrapped tokens, detecting whether the client is using NTLM
// or Kerberos, and delegates to the appropriate authentication mechanism.
//
// For NTLM, the authenticator manages multi-round state internally using
// session IDs to correlate Type 1 (NEGOTIATE) and Type 3 (AUTHENTICATE) messages.
//
// For Kerberos, authentication completes in a single round (placeholder for Phase 39+).
//
// Thread safety: Safe for concurrent use. Each authentication session is tracked
// independently using atomic session IDs and sync.Map for pending state.
type SMBAuthenticator struct {
	// userStore provides user lookup for mapping authenticated identities
	// to DittoFS control plane users.
	userStore models.UserStore

	// pendingAuths tracks in-progress NTLM handshakes.
	// Key: session ID (uint64), Value: *pendingNTLMAuth
	pendingAuths sync.Map

	// nextSessionID generates unique session IDs for tracking auth rounds.
	nextSessionID atomic.Uint64
}

// pendingNTLMAuth tracks state between NTLM authentication rounds.
type pendingNTLMAuth struct {
	serverChallenge [8]byte
}

// Compile-time interface check.
var _ adapter.Authenticator = (*SMBAuthenticator)(nil)

// NewSMBAuthenticator creates a new SMBAuthenticator.
//
// Parameters:
//   - userStore: User store for looking up DittoFS users by username.
//     May be nil, in which case all authentications resolve to guest.
func NewSMBAuthenticator(userStore models.UserStore) *SMBAuthenticator {
	return &SMBAuthenticator{
		userStore: userStore,
	}
}

// Authenticate processes an SMB authentication token (typically SPNEGO-wrapped).
//
// The token is first parsed as SPNEGO to detect the underlying mechanism:
//   - NTLM: Two-round authentication via challenge-response
//   - Kerberos: Single-round authentication (placeholder, returns error)
//   - Raw NTLM: Handled directly if SPNEGO parsing fails
//
// For NTLM Type 1 (NEGOTIATE):
//
//	Returns (nil, challengeToken, ErrMoreProcessingRequired)
//	The challengeToken is an SPNEGO-wrapped NTLM Type 2 (CHALLENGE) message.
//
// For NTLM Type 3 (AUTHENTICATE):
//
//	Returns (result, nil, nil) on success with the authenticated user.
//	Returns (nil, nil, error) on authentication failure.
func (a *SMBAuthenticator) Authenticate(ctx context.Context, token []byte) (*adapter.AuthResult, []byte, error) {
	if len(token) == 0 {
		return &adapter.AuthResult{IsGuest: true}, nil, nil
	}

	// Try SPNEGO parsing to detect mechanism
	if len(token) >= 2 && (token[0] == 0x60 || token[0] == 0xa0 || token[0] == 0xa1) {
		parsed, err := Parse(token)
		if err == nil {
			switch parsed.Type {
			case TokenTypeInit:
				// NegTokenInit - check mechanism
				if parsed.HasKerberos() && len(parsed.MechToken) > 0 {
					return nil, nil, fmt.Errorf("kerberos authentication not yet supported via Authenticator interface")
				}
				if parsed.HasNTLM() && len(parsed.MechToken) > 0 {
					return a.handleNTLMToken(ctx, parsed.MechToken, true)
				}
				// No recognized mechanism
				return &adapter.AuthResult{IsGuest: true}, nil, nil

			case TokenTypeResp:
				// NegTokenResp - continuation of existing auth
				if len(parsed.MechToken) > 0 {
					return a.handleNTLMToken(ctx, parsed.MechToken, true)
				}
			}
		}
	}

	// Try raw NTLM
	if IsValid(token) {
		return a.handleNTLMToken(ctx, token, false)
	}

	// Unrecognized - guest
	return &adapter.AuthResult{IsGuest: true}, nil, nil
}

// handleNTLMToken processes a raw NTLM message (Type 1 or Type 3).
func (a *SMBAuthenticator) handleNTLMToken(ctx context.Context, ntlmToken []byte, wrapSPNEGO bool) (*adapter.AuthResult, []byte, error) {
	if !IsValid(ntlmToken) {
		return nil, nil, fmt.Errorf("invalid NTLM token")
	}

	msgType := GetMessageType(ntlmToken)

	switch msgType {
	case Negotiate:
		return a.handleNTLMNegotiate(wrapSPNEGO)

	case Authenticate:
		return a.handleNTLMAuthenticate(ctx, ntlmToken)

	default:
		return nil, nil, fmt.Errorf("unexpected NTLM message type: %d", msgType)
	}
}

// handleNTLMNegotiate processes an NTLM Type 1 (NEGOTIATE) message.
// Returns a Type 2 (CHALLENGE) message with ErrMoreProcessingRequired.
func (a *SMBAuthenticator) handleNTLMNegotiate(wrapSPNEGO bool) (*adapter.AuthResult, []byte, error) {
	// Build challenge
	challengeMsg, serverChallenge := BuildChallenge()

	// Generate session ID and store pending auth
	sessionID := a.nextSessionID.Add(1)
	a.pendingAuths.Store(sessionID, &pendingNTLMAuth{
		serverChallenge: serverChallenge,
	})

	// Wrap in SPNEGO if needed
	challenge := challengeMsg
	if wrapSPNEGO {
		spnegoResp, err := BuildAcceptIncomplete(OIDNTLMSSP, challengeMsg)
		if err == nil {
			challenge = spnegoResp
		}
		// Fall back to raw NTLM if SPNEGO wrapping fails
	}

	return nil, challenge, adapter.ErrMoreProcessingRequired
}

// handleNTLMAuthenticate processes an NTLM Type 3 (AUTHENTICATE) message.
// Validates the client's response and returns an AuthResult with the user identity.
func (a *SMBAuthenticator) handleNTLMAuthenticate(ctx context.Context, ntlmToken []byte) (*adapter.AuthResult, []byte, error) {
	// Parse the AUTHENTICATE message
	authMsg, err := ParseAuthenticate(ntlmToken)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse NTLM AUTHENTICATE: %w", err)
	}

	// Anonymous/empty username -> guest
	if authMsg.IsAnonymous || authMsg.Username == "" {
		a.cleanupAllPendingAuths()
		return &adapter.AuthResult{IsGuest: true}, nil, nil
	}

	// Look up user
	if a.userStore == nil {
		a.cleanupAllPendingAuths()
		return &adapter.AuthResult{IsGuest: true}, nil, nil
	}

	user, err := a.userStore.GetUser(ctx, authMsg.Username)
	if err != nil || user == nil || !user.Enabled {
		a.cleanupAllPendingAuths()
		if user != nil && !user.Enabled {
			return nil, nil, fmt.Errorf("user account disabled: %s", authMsg.Username)
		}
		return &adapter.AuthResult{IsGuest: true}, nil, nil
	}

	// Try NTLMv2 validation against all pending auths
	ntHash, hasNTHash := user.GetNTHash()
	if hasNTHash && len(authMsg.NtChallengeResponse) > 0 {
		sessionKey, pending := a.validateAgainstPendingAuths(ntHash, authMsg)
		if pending != nil {
			// Derive signing key
			signingKey := DeriveSigningKey(sessionKey, authMsg.NegotiateFlags, authMsg.EncryptedRandomSessionKey)
			a.cleanupAllPendingAuths()

			return &adapter.AuthResult{
				User:       user,
				SessionKey: signingKey[:],
				IsGuest:    false,
			}, nil, nil
		}

		// Validation failed
		a.cleanupAllPendingAuths()
		return nil, nil, errors.New("ntlm: authentication failed")
	}

	// User exists but no NT hash - allow without credential validation
	a.cleanupAllPendingAuths()
	return &adapter.AuthResult{
		User:    user,
		IsGuest: false,
	}, nil, nil
}

// validateAgainstPendingAuths tries to validate the NTLM response against
// all pending authentication challenges. Returns the session key and the
// matching pending auth on success, or zero values on failure.
func (a *SMBAuthenticator) validateAgainstPendingAuths(
	ntHash [16]byte,
	authMsg *AuthenticateMessage,
) ([16]byte, *pendingNTLMAuth) {
	hostname, _ := os.Hostname()
	domainsToTry := uniqueStrings([]string{
		authMsg.Domain,
		"",
		strings.ToUpper(hostname),
		"WORKGROUP",
	})

	var resultKey [16]byte
	var matchedPending *pendingNTLMAuth

	a.pendingAuths.Range(func(key, value any) bool {
		pending := value.(*pendingNTLMAuth)

		for _, domain := range domainsToTry {
			sessionKey, err := ValidateNTLMv2Response(
				ntHash,
				authMsg.Username,
				domain,
				pending.serverChallenge,
				authMsg.NtChallengeResponse,
			)
			if err == nil {
				resultKey = sessionKey
				matchedPending = pending
				return false // stop iteration
			}
		}
		return true // continue
	})

	return resultKey, matchedPending
}

// cleanupAllPendingAuths removes all pending authentication state.
// Called after authentication completes (success or failure).
func (a *SMBAuthenticator) cleanupAllPendingAuths() {
	a.pendingAuths.Clear()
}

// uniqueStrings returns a deduplicated slice preserving order.
func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
