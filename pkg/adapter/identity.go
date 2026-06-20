package adapter

import (
	"context"
	"errors"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/identity"
	identitykerberos "github.com/marmos91/dittofs/pkg/identity/kerberos"
	identityldap "github.com/marmos91/dittofs/pkg/identity/ldap"
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

	// sidMappingStore, when the control-plane store supports it, resolves
	// foreign (AD/LDAP) group SIDs to their durable GIDs so a user's Windows
	// group SIDs contribute Unix supplementary GIDs (AD-3 #1235).
	sidMappingStore, _ := cpStore.(store.SIDMappingStore)

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

		// Resolve persisted foreign group SIDs to durable GIDs and fold them
		// into the supplementary group set. A SID with no durable mapping is
		// skipped (the LDAP provider in AD-2 allocates the mapping at login);
		// never-remap guarantees a stable GID once allocated.
		if sidMappingStore != nil && len(user.GroupSIDs) > 0 {
			mappings, err := sidMappingStore.GetSIDMappingsByIDs(ctx, user.GroupSIDs)
			if err != nil {
				return nil, err
			}
			for _, gsid := range user.GroupSIDs {
				if m, ok := mappings[gsid]; ok && m.IsGroup {
					gids = append(gids, m.UnixID)
				}
			}
		}

		return &identity.ResolvedIdentity{
			Username:  user.Username,
			UID:       uid,
			GID:       gid,
			GIDs:      gids,
			Found:     true,
			SID:       user.SID,
			GroupSIDs: user.GroupSIDs,
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

	opts := []identity.ResolverOption{
		identity.WithProvider(krbProvider),
	}

	// Register the LDAP/AD provider when the server is configured with LDAP.
	// It is tried after Kerberos (which resolves via the local user store / PAC)
	// and before the chain falls through to Found=false, so an AD principal or
	// SID with no local mapping is resolved against the directory (RFC2307
	// UID/GID + nested groups). Construction failures are logged and skipped so a
	// misconfigured directory degrades to Kerberos-only rather than failing boot.
	if ldapCfg := rt.LDAPConfig(); ldapCfg != nil && ldapCfg.Enabled {
		ldapProvider, err := identityldap.New(ldapCfg, linkStore, userLookup)
		if err != nil {
			logger.Warn("LDAP identity provider not registered (invalid config)", "error", err)
		} else {
			opts = append(opts, identity.WithProvider(ldapProvider))
		}
	}

	opts = append(opts, identity.WithCacheTTL(identity.DefaultCacheTTL))
	return identity.NewResolver(opts...)
}
