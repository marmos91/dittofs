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
	if cred.Provider != "" {
		for _, p := range r.providers {
			if p.Name() == cred.Provider {
				return p.Resolve(ctx, cred)
			}
		}
		return &ResolvedIdentity{Found: false}, nil
	}

	for _, p := range r.providers {
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
