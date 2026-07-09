package smb

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/pkg/identity"
)

// directorySIDBridge adapts the centralized identity.Resolver to the handlers
// package's DirectorySIDBridge, backing #1617: it lets the SMB security
// descriptor advertise a file owner/group as the account's REAL directory (AD)
// SID instead of the algorithmic machine-domain SID, and recover the UID/GID
// when Windows echoes that AD SID back through SET_INFO.
//
// Every method is best-effort: a miss (or any infrastructure error) returns
// ok=false, and the handlers layer falls back to the machine-domain SIDMapper.
// Because a hit requires the resolver's LDAP/AD provider to actually hold the
// account, a local-only or conformance deployment never hits — #1617 is a
// transparent no-op there.
type directorySIDBridge struct {
	resolver *identity.Resolver
	timeout  time.Duration
}

// directorySIDBridgeTimeout bounds a single directory round-trip triggered by a
// security-descriptor build/parse. Generous for one cached LDAP search, short
// enough that a hung directory never wedges the SMB handler.
const directorySIDBridgeTimeout = 5 * time.Second

// newDirectorySIDBridge builds a handlers.DirectorySIDBridge backed by the given
// identity.Resolver. Returns nil when the resolver is nil so callers can pass the
// result straight through to handlers.SetDirectorySIDBridge (which treats nil as
// "machine-domain SIDs only").
func newDirectorySIDBridge(resolver *identity.Resolver) handlers.DirectorySIDBridge {
	if resolver == nil {
		return nil
	}
	return &directorySIDBridge{resolver: resolver, timeout: directorySIDBridgeTimeout}
}

// SIDForUID resolves a POSIX UID to its directory user SID string (emit path).
func (b *directorySIDBridge) SIDForUID(uid uint32) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	return b.resolver.SIDForUID(ctx, uid)
}

// SIDForGID resolves a POSIX GID to its directory group SID string (emit path).
func (b *directorySIDBridge) SIDForGID(gid uint32) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	return b.resolver.SIDForGID(ctx, gid)
}

// UIDForSID resolves a directory SID string back to its POSIX UID (parse path).
// It issues a SID-keyed credential to the resolver, which routes it to the
// LDAP/AD provider's objectSid search.
func (b *directorySIDBridge) UIDForSID(sidStr string) (uint32, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	resolved, err := b.resolver.Resolve(ctx, &identity.Credential{ExternalID: sidStr})
	if err != nil || resolved == nil || !resolved.Found {
		return 0, false
	}
	return resolved.UID, true
}

// GIDForSID resolves a directory SID string back to its POSIX GID (parse path).
// Mirrors UIDForSID for a group SID.
func (b *directorySIDBridge) GIDForSID(sidStr string) (uint32, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	resolved, err := b.resolver.Resolve(ctx, &identity.Credential{ExternalID: sidStr})
	if err != nil || resolved == nil || !resolved.Found {
		return 0, false
	}
	return resolved.GID, true
}
