package identity

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ShareInfo contains the share fields required for identity mapping.
// This avoids importing the shares sub-package (no circular dependency).
type ShareInfo struct {
	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32
}

// ShareProvider looks up the identity-relevant fields of a share by name.
type ShareProvider interface {
	GetShareIdentityInfo(shareName string) (*ShareInfo, error)
}

// Service applies share-level identity mapping rules.
type Service struct{}

// New creates a new identity mapping service.
func New() *Service {
	return &Service{}
}

// ApplyIdentityMapping applies share-level identity mapping rules.
//
// This implements Synology-style squash modes:
//   - none: No mapping, UIDs pass through unchanged
//   - root_to_admin: Root (UID 0) retains admin privileges (default)
//   - root_to_guest: Root (UID 0) is mapped to anonymous (root_squash)
//   - all_to_admin: All users are mapped to root (UID 0)
//   - all_to_guest: All users are mapped to anonymous (all_squash)
//
// AUTH_NULL (nil UID) is always mapped to anonymous regardless of squash mode.
func (s *Service) ApplyIdentityMapping(shareName string, identity *metadata.Identity, provider ShareProvider) (*metadata.Identity, error) {
	info, err := provider.GetShareIdentityInfo(shareName)
	if err != nil {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	// Create effective identity (copy of original)
	effective := &metadata.Identity{
		UID:      identity.UID,
		GID:      identity.GID,
		GIDs:     identity.GIDs,
		Username: identity.Username,
	}

	// Handle AUTH_NULL (anonymous access) - always map to anonymous
	if identity.UID == nil {
		ApplyAnonymousIdentity(effective, info.AnonymousUID, info.AnonymousGID)
		return effective, nil
	}

	// Apply squash based on mode
	switch info.Squash {
	case "", models.SquashNone, models.SquashRootToAdmin:
		// No mapping

	case models.SquashRootToGuest:
		if *identity.UID == 0 {
			ApplyAnonymousIdentity(effective, info.AnonymousUID, info.AnonymousGID)
		}

	case models.SquashAllToAdmin:
		ApplyRootIdentity(effective)

	case models.SquashAllToGuest:
		ApplyAnonymousIdentity(effective, info.AnonymousUID, info.AnonymousGID)
	}

	return effective, nil
}

// ApplyAnonymousIdentity sets the identity to anonymous with the given UID/GID.
func ApplyAnonymousIdentity(identity *metadata.Identity, anonUID, anonGID uint32) {
	identity.UID = &anonUID
	identity.GID = &anonGID
	identity.GIDs = []uint32{anonGID}
	identity.Username = fmt.Sprintf("anonymous(%d)", anonUID)
}

// ApplyRootIdentity sets the identity to root (UID/GID 0).
func ApplyRootIdentity(identity *metadata.Identity) {
	rootUID, rootGID := uint32(0), uint32(0)
	identity.UID = &rootUID
	identity.GID = &rootGID
	identity.GIDs = []uint32{rootGID}
	identity.Username = "root"
}
