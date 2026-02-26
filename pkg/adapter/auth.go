// Package adapter provides protocol adapter interfaces for DittoFS.
//
// This file defines the Authenticator interface that unifies authentication
// across all protocol adapters (NFS, SMB). Each protocol implements its own
// Authenticator that bridges protocol-specific auth mechanisms (NTLM, Kerberos,
// AUTH_UNIX) to DittoFS's identity model.

package adapter

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// AuthResult contains the outcome of a successful authentication.
//
// The User field links the authenticated identity to the DittoFS control plane,
// enabling permission resolution and audit logging. SessionKey is used by
// protocols that require message signing (e.g., SMB with NTLM/Kerberos).
type AuthResult struct {
	// User is the authenticated DittoFS user from the control plane.
	// May be nil for guest/anonymous authentication.
	User *models.User

	// SessionKey is the cryptographic key derived during authentication.
	// Used for message signing and encryption in protocols that support it
	// (e.g., SMB uses this for NTLM session signing).
	// Empty for protocols that don't derive session keys (e.g., NFS AUTH_UNIX).
	SessionKey []byte

	// IsGuest indicates whether this is a guest/anonymous authentication.
	// When true, User may be nil or a synthetic guest user.
	IsGuest bool
}

// Authenticator defines the interface for protocol-specific authentication.
//
// Authentication may complete in a single round-trip or require multiple
// exchanges (e.g., NTLM challenge-response). The three return patterns are:
//
//  1. Success: (result, nil, nil)
//     Authentication completed. AuthResult contains the authenticated user
//     identity and any derived session keys.
//
//  2. More processing required: (nil, challengeToken, ErrMoreProcessingRequired)
//     Multi-round authentication in progress. The challenge token must be sent
//     back to the client, and the next client response should be passed to
//     Authenticate again to continue the handshake.
//
//  3. Failure: (nil, nil, error)
//     Authentication failed. The error describes the failure reason.
//
// Implementations must be safe for concurrent use across multiple sessions.
// State between authentication rounds (e.g., NTLM server challenge) is
// managed internally by the implementation.
type Authenticator interface {
	// Authenticate processes an authentication token and returns the result.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout control
	//   - token: Protocol-specific authentication token (e.g., SPNEGO/NTLM bytes,
	//     AUTH_UNIX credentials)
	//
	// Returns:
	//   - result: Non-nil on successful authentication
	//   - challenge: Non-nil challenge token when more rounds are needed
	//   - err: Non-nil on failure, or ErrMoreProcessingRequired for multi-round auth
	Authenticate(ctx context.Context, token []byte) (result *AuthResult, challenge []byte, err error)
}

// ErrMoreProcessingRequired is returned by Authenticator.Authenticate when
// the authentication protocol requires additional round-trips to complete.
//
// This is the expected flow for NTLM authentication:
//   - Round 1: Client sends NEGOTIATE (Type 1) -> server returns CHALLENGE (Type 2)
//     with ErrMoreProcessingRequired
//   - Round 2: Client sends AUTHENTICATE (Type 3) -> server returns AuthResult
//
// For single-round protocols (NFS AUTH_UNIX, Kerberos), this error is never returned.
var ErrMoreProcessingRequired = errors.New("auth: more processing required")
