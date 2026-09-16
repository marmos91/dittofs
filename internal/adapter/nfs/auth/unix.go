package auth

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/pkg/auth"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// UnixTranslator implements auth.Translator for NFS AUTH_UNIX credentials
// (RFC 1831 section 9.2).
//
// It lives here rather than in pkg/auth because decoding the credential is a
// protocol concern: AUTH_UNIX's token is XDR, and pkg/auth holds no wire
// format. The shared interface and result type stay protocol-agnostic.
//
// AUTH_UNIX is single-round: Translate never returns
// auth.ErrMoreProcessingRequired, and the challenge return is always nil.
type UnixTranslator struct{}

var _ auth.Translator = (*UnixTranslator)(nil)

// NewUnixTranslator creates a UnixTranslator.
func NewUnixTranslator() *UnixTranslator { return &UnixTranslator{} }

// Method implements auth.Translator.
func (t *UnixTranslator) Method() string { return "unix" }

// Translate parses AUTH_UNIX credential bytes and resolves them to an identity.
//
// Only the wire UID/GID/GIDs are carried. AUTH_UNIX asserts a Unix identity
// rather than proving one, so they are authoritative, and no user record is
// consulted here: the UID-to-user resolution that turns them into an
// authorization decision happens when the per-operation auth context is built,
// where ResolveSharePermission applies the user.Enabled gate and fills in the
// username.
func (t *UnixTranslator) Translate(_ context.Context, token []byte) (*auth.Result, []byte, error) {
	if len(token) == 0 {
		return nil, nil, fmt.Errorf("auth_unix: empty credentials")
	}

	unixAuth, err := rpc.ParseUnixAuth(token)
	if err != nil {
		return nil, nil, fmt.Errorf("auth_unix: %w", err)
	}

	return &auth.Result{
		Identity: &metadata.Identity{
			UID:  &unixAuth.UID,
			GID:  &unixAuth.GID,
			GIDs: unixAuth.GIDs,
		},
		Method: t.Method(),
	}, nil, nil
}
