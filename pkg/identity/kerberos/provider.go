// Package kerberos provides a Kerberos identity provider for DittoFS.
//
// It resolves Kerberos principals (e.g., "alice@EXAMPLE.COM") to DittoFS
// users via two strategies in order:
//  1. Explicit mapping from the LinkStore (admin-configured principal → username)
//  2. Convention: strip the realm and look up the bare name as a DittoFS username
package kerberos

import (
	"context"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/pkg/identity"
)

const ProviderName = "kerberos"

// Provider resolves Kerberos principals to DittoFS users.
// Implements identity.IdentityProvider.
type Provider struct {
	realm      string
	store      identity.LinkStore
	userLookup identity.UserLookup
}

// New creates a Kerberos identity provider.
//
// Parameters:
//   - realm: expected Kerberos realm for convention matching (e.g., "EXAMPLE.COM")
//   - store: link store for explicit principal → username mappings (may be nil)
//   - userLookup: callback to resolve a DittoFS username to UID/GID
func New(realm string, store identity.LinkStore, userLookup identity.UserLookup) *Provider {
	return &Provider{
		realm:      realm,
		store:      store,
		userLookup: userLookup,
	}
}

func (p *Provider) Name() string { return ProviderName }

func (p *Provider) CanResolve(cred *identity.Credential) bool {
	if cred.Provider != "" {
		return cred.Provider == ProviderName
	}
	switch cred.ExternalID {
	case "OWNER@", "GROUP@", "EVERYONE@":
		return false
	}
	return strings.Contains(cred.ExternalID, "@")
}

// Resolve maps a Kerberos principal to a DittoFS user.
//
// Resolution order:
//  1. Explicit mapping in LinkStore: ("kerberos", "alice@EXAMPLE.COM") → username
//  2. Convention: if realm matches, strip realm and look up bare username
//  3. Numeric UID: "1000@EXAMPLE.COM" → UID=1000 (AUTH_SYS interop)
//  4. Found=false if unmapped
func (p *Provider) Resolve(ctx context.Context, cred *identity.Credential) (*identity.ResolvedIdentity, error) {
	if p.store != nil {
		username, found, err := p.store.GetLink(ctx, ProviderName, cred.ExternalID)
		if err != nil {
			return nil, err
		}
		if found {
			resolved, err := p.userLookup(ctx, username)
			if err != nil {
				return nil, err
			}
			if resolved != nil && resolved.Found {
				out := *resolved
				out.Domain = realmFrom(cred)
				return &out, nil
			}
		}
	}

	name, domain := parsePrincipal(cred.ExternalID)
	if domain == "" || !strings.EqualFold(domain, p.realm) {
		return &identity.ResolvedIdentity{Found: false}, nil
	}

	if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
		return &identity.ResolvedIdentity{
			Username: name,
			UID:      uint32(uid),
			GID:      uint32(uid),
			Domain:   domain,
			Found:    true,
		}, nil
	}

	resolved, err := p.userLookup(ctx, name)
	if err != nil {
		return nil, err
	}
	if resolved == nil || !resolved.Found {
		return &identity.ResolvedIdentity{Found: false}, nil
	}
	out := *resolved
	out.Domain = domain
	return &out, nil
}

func parsePrincipal(principal string) (name, domain string) {
	switch principal {
	case "OWNER@", "GROUP@", "EVERYONE@":
		return principal, ""
	}
	idx := strings.LastIndex(principal, "@")
	if idx < 0 || idx == len(principal)-1 {
		return principal, ""
	}
	return principal[:idx], principal[idx+1:]
}

func realmFrom(cred *identity.Credential) string {
	if r := cred.Attributes["realm"]; r != "" {
		return r
	}
	_, domain := parsePrincipal(cred.ExternalID)
	return domain
}
