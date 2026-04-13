package adapter

import (
	"context"
	"errors"
	"strings"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/identity"
	identitykerberos "github.com/marmos91/dittofs/pkg/identity/kerberos"
)

// ExtractRealm extracts the Kerberos realm from a service principal
// (e.g., "nfs/host@EXAMPLE.COM" returns "EXAMPLE.COM").
func ExtractRealm(principal string) string {
	if idx := strings.LastIndex(principal, "@"); idx >= 0 && idx < len(principal)-1 {
		return principal[idx+1:]
	}
	return ""
}

// BuildIdentityResolver creates a centralized identity Resolver backed by
// the control plane's identity mapping store and user store. Both the NFS
// and SMB adapters use this to map Kerberos principals to DittoFS users.
func BuildIdentityResolver(rt *runtime.Runtime, realm string) *identity.Resolver {
	cpStore := rt.Store()

	userLookup := func(ctx context.Context, username string) (*identity.ResolvedIdentity, error) {
		user, err := cpStore.GetUser(ctx, username)
		if err != nil {
			if errors.Is(err, models.ErrUserNotFound) {
				return &identity.ResolvedIdentity{Found: false}, nil
			}
			return nil, err
		}
		uid := uint32(1000)
		if user.UID != nil {
			uid = *user.UID
		}
		gid := uint32(1000)
		if user.GID != nil {
			gid = *user.GID
		}
		var gids []uint32
		for _, g := range user.Groups {
			if g.GID != nil {
				gids = append(gids, *g.GID)
			}
		}
		return &identity.ResolvedIdentity{
			Username: user.Username,
			UID:      uid,
			GID:      gid,
			GIDs:     gids,
			Found:    true,
		}, nil
	}

	var linkStore identity.LinkStore
	if ims, ok := cpStore.(store.IdentityMappingStore); ok {
		linkStore = &identity.FuncLinkStore{
			GetLinkFn: func(ctx context.Context, provider, externalID string) (string, bool, error) {
				m, err := ims.GetIdentityMapping(ctx, provider, externalID)
				if err != nil {
					if errors.Is(err, models.ErrMappingNotFound) {
						return "", false, nil
					}
					return "", false, err
				}
				return m.Username, true, nil
			},
			ListLinksFn: func(ctx context.Context, provider string) ([]identity.IdentityLink, error) {
				mappings, err := ims.ListIdentityMappings(ctx, provider)
				if err != nil {
					return nil, err
				}
				links := make([]identity.IdentityLink, len(mappings))
				for i, m := range mappings {
					links[i] = identity.IdentityLink{ProviderName: m.ProviderName, ExternalID: m.Principal, Username: m.Username}
				}
				return links, nil
			},
			// CreateLinkFn and DeleteLinkFn left nil — writes go through the API handler, not the resolver.
			// Calling them returns identity.ErrNotConfigured.
			ListLinksForUserFn: func(ctx context.Context, username string) ([]identity.IdentityLink, error) {
				mappings, err := ims.ListIdentityMappingsForUser(ctx, username)
				if err != nil {
					return nil, err
				}
				links := make([]identity.IdentityLink, len(mappings))
				for i, m := range mappings {
					links[i] = identity.IdentityLink{ProviderName: m.ProviderName, ExternalID: m.Principal, Username: m.Username}
				}
				return links, nil
			},
		}
	}

	krbProvider := identitykerberos.New(realm, linkStore, userLookup)
	return identity.NewResolver(
		identity.WithProvider(krbProvider),
		identity.WithCacheTTL(identity.DefaultCacheTTL),
	)
}
