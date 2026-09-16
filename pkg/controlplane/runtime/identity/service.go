package identity

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ShareInfo contains the share fields required for identity mapping.
type ShareInfo struct {
	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32
}

// ShareProvider looks up identity-relevant share fields by name.
type ShareProvider interface {
	GetShareIdentityInfo(shareName string) (*ShareInfo, error)
}

// Service applies share-level identity mapping rules.
type Service struct{}

func New() *Service { return &Service{} }

// ApplyIdentityMapping applies squash modes to the given identity.
// AUTH_NULL (nil UID) is always mapped to anonymous regardless of squash mode.
func (s *Service) ApplyIdentityMapping(shareName string, identity *metadata.Identity, provider ShareProvider) (*metadata.Identity, error) {
	info, err := provider.GetShareIdentityInfo(shareName)
	if err != nil {
		return nil, fmt.Errorf("share %q: %w", shareName, err)
	}

	effective := &metadata.Identity{
		UID:       identity.UID,
		GID:       identity.GID,
		GIDs:      identity.GIDs,
		SID:       identity.SID,
		GroupSIDs: identity.GroupSIDs,
		Username:  identity.Username,
		Domain:    identity.Domain,
	}

	if identity.UID == nil {
		ApplyAnonymousIdentity(effective, info.AnonymousUID, info.AnonymousGID)
		return effective, nil
	}

	// Normalize empty/unset squash to DefaultSquashMode (root_to_guest) so an
	// unconfigured share squashes root to anonymous instead of leaving it
	// unmapped.
	switch info.Squash.OrDefault() {
	case models.SquashNone, models.SquashRootToAdmin:
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

func ApplyAnonymousIdentity(identity *metadata.Identity, anonUID, anonGID uint32) {
	identity.UID = &anonUID
	identity.GID = &anonGID
	identity.GIDs = []uint32{anonGID}
	// Clear the Windows half too: a squash to anonymous must not leave a named
	// principal's SID or group SIDs behind, or ACE matching against them would
	// survive the squash.
	identity.SID = nil
	identity.GroupSIDs = nil
	identity.Domain = ""
	identity.Username = fmt.Sprintf("anonymous(%d)", anonUID)
}

func ApplyRootIdentity(identity *metadata.Identity) {
	rootUID, rootGID := uint32(0), uint32(0)
	identity.UID = &rootUID
	identity.GID = &rootGID
	identity.GIDs = []uint32{rootGID}
	identity.SID = nil
	identity.GroupSIDs = nil
	identity.Domain = ""
	identity.Username = "root"
}
