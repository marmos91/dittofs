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
	"time"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

const (
	// pendingAuthTTL is the maximum lifetime of an incomplete NTLM handshake.
	// After this, the entry is eligible for eviction.
	pendingAuthTTL = 60 * time.Second

	// maxPendingAuths caps the number of in-flight NTLM handshakes to prevent
	// memory exhaustion from abandoned negotiations.
	maxPendingAuths = 1000
)

// pendingAuthKey correlates the NTLM NEGOTIATE and AUTHENTICATE rounds of a
// single handshake. It mirrors the handler-layer key (see handler.go): parallel
// binds on the same session from different connections must not clobber each
// other, so both the session ID and connection ID participate in the key.
type pendingAuthKey struct {
	sessionID uint64
	connID    uint64
}

type ctxKeySessionID struct{}

type ctxKeyConnID struct{}

// WithSessionID returns a context carrying the SMB session ID used to correlate
// the NTLM NEGOTIATE and AUTHENTICATE rounds of a single handshake. Both calls
// for the same exchange must carry the same value.
func WithSessionID(parent context.Context, id uint64) context.Context {
	return context.WithValue(parent, ctxKeySessionID{}, id)
}

// WithConnID returns a context carrying the TCP connection ID used to correlate
// the NTLM NEGOTIATE and AUTHENTICATE rounds of a single handshake. Both calls
// for the same exchange must carry the same value.
func WithConnID(parent context.Context, id uint64) context.Context {
	return context.WithValue(parent, ctxKeyConnID{}, id)
}

func sessionIDFromCtx(ctx context.Context) uint64 {
	v, _ := ctx.Value(ctxKeySessionID{}).(uint64)
	return v
}

func connIDFromCtx(ctx context.Context) uint64 {
	v, _ := ctx.Value(ctxKeyConnID{}).(uint64)
	return v
}

// SMBAuthenticator implements adapter.Authenticator for SMB protocol authentication.
//
// It handles SPNEGO-wrapped tokens, detecting whether the client is using NTLM
// or Kerberos, and delegates to the appropriate authentication mechanism.
//
// For NTLM, the authenticator manages multi-round state internally using
// session IDs to correlate Type 1 (NEGOTIATE) and Type 3 (AUTHENTICATE) messages.
//
// For Kerberos, authentication completes in a single round (placeholder).
//
// Thread safety: Safe for concurrent use. Each authentication session is tracked
// independently using atomic session IDs and sync.Map for pending state.
type SMBAuthenticator struct {
	// userStore provides user lookup for mapping authenticated identities
	// to DittoFS control plane users.
	userStore models.UserStore

	// pendingAuths tracks in-progress NTLM handshakes.
	// Key: pendingAuthKey{sessionID, connID}, Value: *pendingNTLMAuth
	pendingAuths sync.Map

	// nextSessionID generates a fallback session ID when the caller does not
	// inject one via WithSessionID. The atomic counter guarantees uniqueness so
	// concurrent fallback negotiations cannot collide.
	nextSessionID atomic.Uint64
}

// pendingNTLMAuth tracks state between NTLM authentication rounds.
type pendingNTLMAuth struct {
	serverChallenge [8]byte
	createdAt       time.Time
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
		return a.handleNTLMNegotiate(ctx, wrapSPNEGO)

	case Authenticate:
		return a.handleNTLMAuthenticate(ctx, ntlmToken)

	default:
		return nil, nil, fmt.Errorf("unexpected NTLM message type: %d", msgType)
	}
}

// handleNTLMNegotiate processes an NTLM Type 1 (NEGOTIATE) message.
// Returns a Type 2 (CHALLENGE) message with ErrMoreProcessingRequired.
func (a *SMBAuthenticator) handleNTLMNegotiate(ctx context.Context, wrapSPNEGO bool) (*adapter.AuthResult, []byte, error) {
	// Evict stale pending auths before adding a new one
	a.evictStalePendingAuths()

	// Build challenge
	challengeMsg, serverChallenge := BuildChallenge()

	// Correlate this handshake by (sessionID, connID). Callers thread these via
	// WithSessionID/WithConnID. If no session ID was injected, fall back to a
	// unique counter value so concurrent fallback negotiations cannot collide.
	sessionID := sessionIDFromCtx(ctx)
	if sessionID == 0 {
		sessionID = a.nextSessionID.Add(1)
	}
	key := pendingAuthKey{sessionID: sessionID, connID: connIDFromCtx(ctx)}
	a.pendingAuths.Store(key, &pendingNTLMAuth{
		serverChallenge: serverChallenge,
		createdAt:       time.Now(),
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
		return &adapter.AuthResult{IsGuest: true}, nil, nil
	}

	// Look up user
	if a.userStore == nil {
		return &adapter.AuthResult{IsGuest: true}, nil, nil
	}

	user, err := a.userStore.GetUser(ctx, authMsg.Username)
	if err != nil || user == nil || !user.Enabled {
		if user != nil && !user.Enabled {
			return nil, nil, fmt.Errorf("user account disabled: %s", authMsg.Username)
		}
		return &adapter.AuthResult{IsGuest: true}, nil, nil
	}

	// Try NTLMv2 validation against this session's pending challenge only.
	ntHash, hasNTHash := user.GetNTHash()
	if hasNTHash && len(authMsg.NtChallengeResponse) > 0 {
		// Look up the pending challenge stored for this exact (sessionID, connID)
		// pair. A direct lookup makes cross-session validation impossible: a
		// Type-3 from another session cannot match a challenge stored under a
		// different key.
		key := pendingAuthKey{sessionID: sessionIDFromCtx(ctx), connID: connIDFromCtx(ctx)}
		v, ok := a.pendingAuths.Load(key)
		if !ok {
			return nil, nil, errors.New("ntlm: no pending authentication for session")
		}
		pending := v.(*pendingNTLMAuth)
		a.pendingAuths.Delete(key) // consume the challenge

		hostname, _ := os.Hostname()
		domainsToTry := uniqueStrings([]string{
			authMsg.Domain,
			"",
			strings.ToUpper(hostname),
			"WORKGROUP",
		})

		var sessionKey [16]byte
		var validationErr error = errors.New("ntlm: authentication failed")
		for _, domain := range domainsToTry {
			sessionKey, validationErr = ValidateNTLMv2Response(
				ntHash,
				authMsg.Username,
				domain,
				pending.serverChallenge,
				authMsg.NtChallengeResponse,
			)
			if validationErr == nil {
				break
			}
		}
		if validationErr != nil {
			return nil, nil, errors.New("ntlm: authentication failed")
		}

		signingKey := DeriveSigningKey(sessionKey, authMsg.NegotiateFlags, authMsg.EncryptedRandomSessionKey)
		return &adapter.AuthResult{
			User:       user,
			SessionKey: signingKey[:],
			IsGuest:    false,
		}, nil, nil
	}

	// User exists but no NT hash - allow without credential validation
	return &adapter.AuthResult{
		User:    user,
		IsGuest: false,
	}, nil, nil
}

// evictStalePendingAuths removes expired entries and enforces the max count.
// Called on each new NEGOTIATE to prevent unbounded growth from abandoned handshakes.
func (a *SMBAuthenticator) evictStalePendingAuths() {
	now := time.Now()
	count := 0

	a.pendingAuths.Range(func(key, value any) bool {
		pending := value.(*pendingNTLMAuth)
		if now.Sub(pending.createdAt) > pendingAuthTTL {
			a.pendingAuths.Delete(key)
			return true
		}
		count++
		return true
	})

	// If still over the cap after TTL eviction, remove oldest entries
	if count > maxPendingAuths {
		a.pendingAuths.Range(func(key, _ any) bool {
			a.pendingAuths.Delete(key)
			count--
			return count > maxPendingAuths
		})
	}
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
