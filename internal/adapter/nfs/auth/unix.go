// Package auth provides authentication for NFS protocol adapters.
//
// This file implements the adapter.Authenticator interface for NFS AUTH_UNIX
// credentials, providing architectural symmetry with the SMB Authenticator.
//
// NFS AUTH_UNIX is a single-round authentication mechanism: the client sends
// its Unix credentials (UID, GID, supplementary GIDs, machine name) and the
// server resolves them to a DittoFS user identity.
//
// Note: The full NFS auth refactoring to wire this into the dispatch pipeline
// is deferred to a future phase. Currently, NFS uses the middleware-based
// auth extraction in internal/adapter/nfs/middleware/auth.go.
package auth

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// UnixAuthenticator implements adapter.Authenticator for NFS AUTH_UNIX credentials.
//
// It parses AUTH_UNIX credential bytes (UID, GID, GIDs, machine name) and
// resolves the UID to a DittoFS control plane user. If no user is found for
// the given UID, a synthetic user is returned to allow the operation to proceed
// with the raw Unix credentials.
//
// AUTH_UNIX is always single-round: Authenticate never returns
// ErrMoreProcessingRequired.
type UnixAuthenticator struct {
	// identityStore provides user lookup by UID for resolving AUTH_UNIX
	// credentials to DittoFS users.
	identityStore models.IdentityStore
}

// Compile-time interface check.
var _ adapter.Authenticator = (*UnixAuthenticator)(nil)

// NewUnixAuthenticator creates a new UnixAuthenticator.
//
// Parameters:
//   - identityStore: Identity store for UID-based user lookup.
//     May be nil, in which case all authentications produce synthetic users.
func NewUnixAuthenticator(identityStore models.IdentityStore) *UnixAuthenticator {
	return &UnixAuthenticator{
		identityStore: identityStore,
	}
}

// Authenticate processes NFS AUTH_UNIX credential bytes and returns an AuthResult.
//
// The token must be XDR-encoded AUTH_UNIX credentials as defined in RFC 1831:
//   - stamp (4 bytes, big-endian)
//   - machine name (XDR string: length + data + padding)
//   - uid (4 bytes, big-endian)
//   - gid (4 bytes, big-endian)
//   - gids (XDR array: count + elements)
//
// AUTH_UNIX is always single-round: this method never returns
// ErrMoreProcessingRequired. The challenge return value is always nil.
//
// If the UID maps to a known DittoFS user, that user is returned in the
// AuthResult. Otherwise, a synthetic user with the raw Unix credentials
// is created (allowing NFS to operate with unknown UIDs as historically expected).
func (a *UnixAuthenticator) Authenticate(ctx context.Context, token []byte) (*adapter.AuthResult, []byte, error) {
	if len(token) == 0 {
		return nil, nil, fmt.Errorf("auth_unix: empty credentials")
	}

	// Parse AUTH_UNIX credentials
	unixAuth, err := parseUnixAuth(token)
	if err != nil {
		return nil, nil, fmt.Errorf("auth_unix: %w", err)
	}

	// Try to resolve UID to a DittoFS user
	if a.identityStore != nil {
		user, err := a.identityStore.GetUserByUID(ctx, unixAuth.UID)
		if err == nil && user != nil {
			return &adapter.AuthResult{
				User:    user,
				IsGuest: false,
			}, nil, nil
		}
		// UID not found in control plane - fall through to synthetic user
	}

	// Create synthetic user for unknown UIDs.
	// This preserves NFS's traditional behavior where any Unix client can
	// access the server using its local credentials.
	syntheticUser := &models.User{
		Username: fmt.Sprintf("unix:%d", unixAuth.UID),
		Enabled:  true,
		UID:      &unixAuth.UID,
		GID:      &unixAuth.GID,
	}

	return &adapter.AuthResult{
		User:    syntheticUser,
		IsGuest: false,
	}, nil, nil
}

// unixAuthResult holds parsed AUTH_UNIX credential fields.
type unixAuthResult struct {
	Stamp       uint32
	MachineName string
	UID         uint32
	GID         uint32
	GIDs        []uint32
}

// parseUnixAuth parses AUTH_UNIX credentials from XDR-encoded bytes.
//
// This is a standalone parser that mirrors rpc.ParseUnixAuth but lives in
// the auth package to avoid an import dependency on the RPC package.
// Both implementations parse the same wire format defined in RFC 1831.
func parseUnixAuth(body []byte) (*unixAuthResult, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty auth body")
	}

	auth := &unixAuthResult{}
	reader := bytes.NewReader(body)

	// Read stamp (4 bytes, big-endian)
	if err := binary.Read(reader, binary.BigEndian, &auth.Stamp); err != nil {
		return nil, fmt.Errorf("read stamp: %w", err)
	}

	// Read machine name (XDR string: length + data + padding)
	var nameLen uint32
	if err := binary.Read(reader, binary.BigEndian, &nameLen); err != nil {
		return nil, fmt.Errorf("read machine name length: %w", err)
	}
	if nameLen > 255 {
		return nil, fmt.Errorf("machine name too long: %d", nameLen)
	}

	nameBytes := make([]byte, nameLen)
	if _, err := reader.Read(nameBytes); err != nil {
		return nil, fmt.Errorf("read machine name: %w", err)
	}
	auth.MachineName = string(nameBytes)

	// Skip XDR padding to 4-byte boundary
	padding := (4 - (nameLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		_, _ = reader.ReadByte()
	}

	// Read UID
	if err := binary.Read(reader, binary.BigEndian, &auth.UID); err != nil {
		return nil, fmt.Errorf("read uid: %w", err)
	}

	// Read GID
	if err := binary.Read(reader, binary.BigEndian, &auth.GID); err != nil {
		return nil, fmt.Errorf("read gid: %w", err)
	}

	// Read supplementary GIDs
	var gidsLen uint32
	if err := binary.Read(reader, binary.BigEndian, &gidsLen); err != nil {
		return nil, fmt.Errorf("read gids length: %w", err)
	}
	if gidsLen > 16 {
		return nil, fmt.Errorf("too many gids: %d", gidsLen)
	}

	auth.GIDs = make([]uint32, gidsLen)
	for i := uint32(0); i < gidsLen; i++ {
		if err := binary.Read(reader, binary.BigEndian, &auth.GIDs[i]); err != nil {
			return nil, fmt.Errorf("read gid[%d]: %w", i, err)
		}
	}

	return auth, nil
}
