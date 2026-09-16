package auth

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/pkg/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
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
type UnixTranslator struct {
	// identityStore resolves a wire UID to a DittoFS user, so the result can
	// carry that user's name. Nil means every authentication produces a
	// synthetic identity from the wire credentials alone.
	identityStore models.IdentityStore
}

var _ auth.Translator = (*UnixTranslator)(nil)

// NewUnixTranslator creates a UnixTranslator. identityStore may be nil.
func NewUnixTranslator(identityStore models.IdentityStore) *UnixTranslator {
	return &UnixTranslator{identityStore: identityStore}
}

// Method implements auth.Translator.
func (t *UnixTranslator) Method() string { return "unix" }

// Translate parses AUTH_UNIX credential bytes and resolves them to an identity.
//
// The wire UID/GID/GIDs are authoritative: AUTH_UNIX asserts a Unix identity
// rather than proving one, so they are carried into the result as-is. The
// optional store lookup only supplies the username for a UID the control plane
// already knows; it never overrides the wire credentials. A UID with no user
// record yields a synthetic "unix:<uid>" identity so an unknown client still
// operates under the credentials it presented.
func (t *UnixTranslator) Translate(ctx context.Context, token []byte) (*auth.Result, []byte, error) {
	if len(token) == 0 {
		return nil, nil, fmt.Errorf("auth_unix: empty credentials")
	}

	unixAuth, err := rpc.ParseUnixAuth(token)
	if err != nil {
		return nil, nil, fmt.Errorf("auth_unix: %w", err)
	}

	identity := &metadata.Identity{
		UID:  &unixAuth.UID,
		GID:  &unixAuth.GID,
		GIDs: unixAuth.GIDs,
	}

	if t.identityStore != nil {
		user, err := t.identityStore.GetUserByUID(ctx, unixAuth.UID)
		if err == nil && user != nil {
			identity.Username = user.Username
			return &auth.Result{Identity: identity, Method: t.Method()}, nil, nil
		}
	}

	identity.Username = fmt.Sprintf("unix:%d", unixAuth.UID)
	return &auth.Result{Identity: identity, Method: t.Method()}, nil, nil
}
