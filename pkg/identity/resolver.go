package identity

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"
)

// Resolver chains identity providers and caches results.
// It is the primary entry point for identity resolution, used by both
// NFS and SMB protocol adapters.
//
// Thread safety: safe for concurrent use. Uses singleflight to prevent
// thundering herd on concurrent cache misses for the same credential.
type Resolver struct {
	providers []IdentityProvider
	cache     *cache
	group     singleflight.Group
}

// ResolverOption configures a Resolver.
type ResolverOption func(*Resolver)

// WithProvider registers an identity provider with the Resolver.
// Providers are tried in registration order; first Found=true wins.
func WithProvider(p IdentityProvider) ResolverOption {
	return func(r *Resolver) {
		r.providers = append(r.providers, p)
	}
}

// WithCacheTTL sets the positive cache TTL.
func WithCacheTTL(ttl time.Duration) ResolverOption {
	return func(r *Resolver) {
		r.cache.positiveTTL = ttl
	}
}

// WithNegativeCacheTTL sets the TTL for caching Found=false results.
func WithNegativeCacheTTL(ttl time.Duration) ResolverOption {
	return func(r *Resolver) {
		r.cache.negativeTTL = ttl
	}
}

// WithErrorCacheTTL sets the TTL for caching infrastructure errors.
func WithErrorCacheTTL(ttl time.Duration) ResolverOption {
	return func(r *Resolver) {
		r.cache.errorTTL = ttl
	}
}

// NewResolver creates a new identity resolver with the given options.
func NewResolver(opts ...ResolverOption) *Resolver {
	r := &Resolver{
		cache: newCache(DefaultCacheTTL, DefaultNegativeCacheTTL, DefaultErrorCacheTTL),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve maps an external credential to a DittoFS user.
//
// Resolution order:
//  1. Check cache
//  2. If cred.Provider is set, route to that specific provider
//  3. Otherwise, try each provider in order via CanResolve() → Resolve()
//  4. Cache and return first Found=true result
//  5. If all providers return Found=false, cache and return Found=false
//
// An infrastructure error from any provider aborts the chain immediately.
// Concurrent requests for the same credential are coalesced via singleflight.
func (r *Resolver) Resolve(ctx context.Context, cred *Credential) (*ResolvedIdentity, error) {
	key := cacheKey(cred.Provider, cred.ExternalID)

	if result, err, ok := r.cache.get(key); ok {
		return result, err
	}

	v, err, _ := r.group.Do(key, func() (any, error) {
		if result, cachedErr, ok := r.cache.get(key); ok {
			return &cacheResult{result: result, err: cachedErr}, nil
		}
		result, resolveErr := r.resolveUncached(context.WithoutCancel(ctx), cred)
		r.cache.put(key, result, resolveErr)
		return &cacheResult{result: result, err: resolveErr}, nil
	})
	if err != nil {
		return nil, err
	}
	cr := v.(*cacheResult)
	return cr.result, cr.err
}

type cacheResult struct {
	result *ResolvedIdentity
	err    error
}

func (r *Resolver) resolveUncached(ctx context.Context, cred *Credential) (*ResolvedIdentity, error) {
	// A credential tagged with an explicit Provider names the PREFERRED provider
	// (the auth source that produced it, e.g. "kerberos" for an RPCSEC_GSS /
	// SPNEGO principal). Try it first — but treat Found=false as "try the rest
	// of the chain", not a terminal answer. This is what realizes the documented
	// fallback design in BuildIdentityResolver: the Kerberos provider resolves a
	// principal that maps to a LOCAL DittoFS user, and a domain principal with no
	// local account falls through to the LDAP/AD directory provider (RFC2307
	// UID/GID + nested groups). Previously this branch returned the preferred
	// provider's Found=false directly, so every domain user authenticating over
	// Kerberos resolved to nobody and the directory was never consulted.
	preferred := -1
	if cred.Provider != "" {
		for i, p := range r.providers {
			if p.Name() == cred.Provider {
				result, err := p.Resolve(ctx, cred)
				if err != nil {
					return nil, err
				}
				if result.Found {
					return result, nil
				}
				preferred = i
				break
			}
		}
	}

	// Fall through to every other provider that claims the credential by shape.
	for i, p := range r.providers {
		if i == preferred {
			continue // already tried above
		}
		if !p.CanResolve(cred) {
			continue
		}
		result, err := p.Resolve(ctx, cred)
		if err != nil {
			return nil, err
		}
		if result.Found {
			return result, nil
		}
	}

	return &ResolvedIdentity{Found: false}, nil
}

// ReverseResolver is an optional capability a Provider may implement to resolve
// a POSIX UID/GID back to a directory account/group name. It backs the LSARPC
// owner/group SID display path: a local algorithmic SID decodes to a UID/GID
// that has no local DittoFS account (an AD-only user), so the name is recovered
// from the directory instead.
//
// A miss returns ok=false and is never an error: the SID then stays unmapped
// (raw) rather than faulting. Infrastructure failures are folded into ok=false
// by the implementation.
type ReverseResolver interface {
	// LookupUID resolves a POSIX UID to an account name + domain/realm.
	LookupUID(ctx context.Context, uid uint32) (name, domain string, ok bool)
	// LookupGID resolves a POSIX GID to a group name + domain/realm.
	LookupGID(ctx context.Context, gid uint32) (name, domain string, ok bool)
}

// reverseUIDKey / reverseGIDKey are synthetic cache keys for reverse lookups.
// They share the Resolver's cache (positive + negative TTL) with forward
// resolution but cannot collide with a forward key, which always embeds a
// provider name and an ExternalID via cacheKey.
func reverseUIDKey(uid uint32) string { return fmt.Sprintf("ruid:%d", uid) }
func reverseGIDKey(gid uint32) string { return fmt.Sprintf("rgid:%d", gid) }

// LookupUID resolves a POSIX UID to a directory account name + domain by
// consulting every registered provider that implements ReverseResolver, in
// registration order. The first hit wins. Results (hits and misses) are cached
// with the same TTLs as forward resolution so a repeated Explorer Security-tab
// lookup of the same owner does not re-query the directory each time.
func (r *Resolver) LookupUID(ctx context.Context, uid uint32) (name, domain string, ok bool) {
	return r.reverseLookup(ctx, reverseUIDKey(uid), func(rr ReverseResolver) (string, string, bool) {
		return rr.LookupUID(ctx, uid)
	})
}

// LookupGID resolves a POSIX GID to a directory group name + domain. Mirrors
// LookupUID for the GROUP SID on a file.
func (r *Resolver) LookupGID(ctx context.Context, gid uint32) (name, domain string, ok bool) {
	return r.reverseLookup(ctx, reverseGIDKey(gid), func(rr ReverseResolver) (string, string, bool) {
		return rr.LookupGID(ctx, gid)
	})
}

// reverseLookup runs a cached reverse lookup against the provider chain. The
// hit/miss is encoded into a ResolvedIdentity (Username/Domain/Found) so it can
// ride the existing identity cache.
func (r *Resolver) reverseLookup(ctx context.Context, key string, query func(ReverseResolver) (string, string, bool)) (string, string, bool) {
	if cached, _, hit := r.cache.get(key); hit && cached != nil {
		return cached.Username, cached.Domain, cached.Found
	}

	for _, p := range r.providers {
		rr, capable := p.(ReverseResolver)
		if !capable {
			continue
		}
		if name, domain, ok := query(rr); ok {
			r.cache.put(key, &ResolvedIdentity{Username: name, Domain: domain, Found: true}, nil)
			return name, domain, true
		}
	}

	r.cache.put(key, &ResolvedIdentity{Found: false}, nil)
	return "", "", false
}

// InvalidateCache clears all cached entries.
func (r *Resolver) InvalidateCache() {
	r.cache.invalidateAll()
}

// InvalidateKey removes a specific entry from the cache.
func (r *Resolver) InvalidateKey(provider, externalID string) {
	r.cache.invalidate(cacheKey(provider, externalID))
}

// Stats returns cache statistics.
func (r *Resolver) Stats() CacheStats {
	return r.cache.stats()
}

func cacheKey(provider, externalID string) string {
	return fmt.Sprintf("%d:%s|%s", len(provider), provider, externalID)
}
