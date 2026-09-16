package auth

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ShareIdentityRuntime is the slice of the runtime needed to turn a client's
// presented credentials into the effective identity for a share.
type ShareIdentityRuntime interface {
	GetShare(name string) (*runtime.Share, error)
	GetIdentityStore() models.IdentityStore
	ApplyIdentityMapping(shareName string, ident *metadata.Identity) (*metadata.Identity, error)
}

// Credentials are the credentials a client presented on the RPC, before any
// share policy has been applied to them. A nil UID is an anonymous caller
// (AUTH_NULL, or AUTH_UNIX whose credential did not parse).
type Credentials struct {
	UID  *uint32
	GID  *uint32
	GIDs []uint32

	// ClientAddr is the transport source, "IP:port" or bare IP. Share policy
	// can be host-scoped, so it takes part in resolving the permission.
	ClientAddr string

	// AuthMethod records how the client authenticated ("unix", "anonymous").
	// It is audit information, not an input to the permission decision.
	AuthMethod string
}

// AuthMethodFor names the authentication method for an identity, using the
// convention the auth contexts carry: credentials present means "unix".
func (c Credentials) AuthMethodFor() string {
	if c.AuthMethod != "" {
		return c.AuthMethod
	}
	if c.UID == nil {
		return "anonymous"
	}
	return "unix"
}

// BuildAuthContext resolves the credentials a client presented into the
// effective identity for a share, and is the single construction behind every
// NFS-side authorization decision on that share.
//
// It exists as one function because the alternative is several: NFSv3 reads a
// file through this, and NLM authorizes a byte-range lock on the same file
// through it too. Two builders would let the two gates disagree about one user
// on one file -- which reads to whoever hits it as "locking is broken", not as
// an authorization gap, because the read that proves they have access succeeds.
//
// The steps, in order:
//  1. Build the identity from the presented Unix credentials.
//  2. Resolve the share permission, which yields the read-only ceiling and the
//     principal name that named and SID ACEs are matched against.
//  3. Apply the share's squash policy (root_squash, all_squash).
//
// Step 2 is what a uid-only identity misses: a file whose ACL grants access
// through a named or SID ACE rather than through OWNER@/GROUP@/EVERYONE@ or
// the mode bits is invisible to a bare uid.
//
// Returns ErrShareAccessDenied when share policy denies the caller outright.
func BuildAuthContext(
	ctx context.Context,
	rt ShareIdentityRuntime,
	shareName string,
	creds Credentials,
) (*metadata.AuthContext, error) {
	identity := &metadata.Identity{
		UID:  creds.UID,
		GID:  creds.GID,
		GIDs: creds.GIDs,
	}
	if identity.UID != nil {
		identity.Username = fmt.Sprintf("uid:%d", *identity.UID)
	}

	share, err := rt.GetShare(shareName)
	if err != nil {
		return nil, fmt.Errorf("failed to get share: %w", err)
	}

	// The GID set (supplementary + primary) lets a direct AD/SID grant match by
	// GID, so a login with no local user is still resolvable.
	permGIDs := append([]uint32(nil), creds.GIDs...)
	if creds.GID != nil {
		permGIDs = append(permGIDs, *creds.GID)
	}
	permResult, err := ResolveSharePermission(
		ctx, rt.GetIdentityStore(), share, shareName, creds.ClientAddr, creds.UID, permGIDs)
	if err != nil {
		return nil, err
	}
	if permResult.Username != "" {
		identity.Username = permResult.Username
	}

	effective, err := rt.ApplyIdentityMapping(shareName, identity)
	if err != nil {
		return nil, fmt.Errorf("failed to apply identity mapping: %w", err)
	}

	return &metadata.AuthContext{
		Context:       ctx,
		ClientAddr:    creds.ClientAddr,
		AuthMethod:    creds.AuthMethodFor(),
		Identity:      effective,
		ShareReadOnly: permResult.ReadOnly,
	}, nil
}
