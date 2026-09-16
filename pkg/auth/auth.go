// Package auth is the single home for DittoFS's authentication model: the
// Result an authentication mechanism produces, and the Translator interface
// each mechanism implements.
//
// It holds the shared model only. Each protocol's Translator lives in that
// protocol's package, because translating a credential means decoding its wire
// format and this package carries no wire format. NFS's AUTH_UNIX translator is
// in internal/adapter/nfs/auth; the stateful mechanisms (NTLM, RPCSEC_GSS,
// SPNEGO) are not consolidated anywhere yet.
//
// The interface carries the challenge-token return and the
// ErrMoreProcessingRequired sentinel so those multi-round mechanisms can move in
// later without a breaking change — a two-value Translate could not express
// them at all.
package auth

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Result is the outcome of a successful authentication.
//
// Identity is a *metadata.Identity rather than a type of its own: the
// authentication result is what the metadata layer's authorization checks
// consume, and metadata.Identity already carries the Unix (UID/GID/GIDs) and
// Windows (SID/GroupSIDs/Domain) halves both protocols need. A distinct type
// would duplicate it field-for-field, which is how the Windows half got
// dropped from a copy before.
type Result struct {
	// Identity is the authenticated client identity.
	Identity *metadata.Identity

	// SessionKey is the key derived during authentication, used for message
	// signing and encryption by protocols that support it (SMB's NTLM).
	// Empty for mechanisms that derive no key, such as AUTH_UNIX.
	SessionKey []byte

	// Method names the mechanism that produced this result ("unix",
	// "anonymous", ...). Mirrors metadata.AuthContext.AuthMethod.
	Method string

	// Guest reports that the caller was authenticated as guest/anonymous.
	// A guest result may still carry a synthetic identity.
	Guest bool
}

// Translator converts one protocol's wire credential into a Result.
//
// Translate returns (result, nil, nil) when authentication completes, and
// (nil, challenge, ErrMoreProcessingRequired) when the mechanism needs another
// round-trip; the challenge must then be sent to the client and the client's
// next response passed back to Translate. A real error means authentication
// failed.
//
// Implementations must be safe for concurrent use: one instance is shared
// across all sessions of a protocol.
type Translator interface {
	// Method names the mechanism this translator implements, matching
	// Result.Method.
	Method() string

	// Translate processes a wire token. See the interface doc for the three
	// return patterns.
	Translate(ctx context.Context, token []byte) (result *Result, challenge []byte, err error)
}

// ErrMoreProcessingRequired is returned by Translator.Translate when the
// mechanism needs additional round-trips to complete. Single-round mechanisms
// such as AUTH_UNIX never return it.
var ErrMoreProcessingRequired = errors.New("auth: more processing required")
