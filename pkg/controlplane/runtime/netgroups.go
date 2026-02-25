package runtime

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// Default DNS cache TTLs.
const (
	DefaultDNSCacheTTL    = 5 * time.Minute // Positive result cache
	DefaultDNSCacheNegTTL = 1 * time.Minute // Negative result (error) cache
)

// dnsCache provides a thread-safe cache for reverse DNS lookups.
// Per Pitfall 3 (DNS blocking): cache prevents DNS from blocking NFS operations.
type dnsCache struct {
	mu      sync.RWMutex
	entries map[string]*dnsCacheEntry
	ttl     time.Duration // positive result TTL (default 5 minutes)
	negTTL  time.Duration // negative result TTL (default 1 minute)
}

// dnsCacheEntry holds a cached DNS lookup result.
type dnsCacheEntry struct {
	hostnames []string  // reverse DNS results
	err       error     // cached error (negative cache)
	expiresAt time.Time // when this entry expires
}

// newDNSCache creates a new DNS cache with the given TTLs.
func newDNSCache(ttl, negTTL time.Duration) *dnsCache {
	if ttl <= 0 {
		ttl = DefaultDNSCacheTTL
	}
	if negTTL <= 0 {
		negTTL = DefaultDNSCacheNegTTL
	}
	return &dnsCache{
		entries: make(map[string]*dnsCacheEntry),
		ttl:     ttl,
		negTTL:  negTTL,
	}
}

// lookupAddr performs a reverse DNS lookup with caching.
// Returns cached results on hit; performs net.LookupAddr on miss.
func (c *dnsCache) lookupAddr(ip string) ([]string, error) {
	now := time.Now()

	// Check cache under read lock
	c.mu.RLock()
	entry, exists := c.entries[ip]
	c.mu.RUnlock()

	if exists && now.Before(entry.expiresAt) {
		return entry.hostnames, entry.err
	}

	// Cache miss or expired - perform lookup
	hostnames, err := net.LookupAddr(ip)

	// Store result
	var ttl time.Duration
	if err != nil {
		ttl = c.negTTL
	} else {
		ttl = c.ttl
	}

	c.mu.Lock()
	c.entries[ip] = &dnsCacheEntry{
		hostnames: hostnames,
		err:       err,
		expiresAt: now.Add(ttl),
	}
	// Piggyback cleanup of expired entries
	c.cleanExpiredLocked(now)
	c.mu.Unlock()

	return hostnames, err
}

// cleanExpiredLocked removes expired entries. Must be called with write lock held.
func (c *dnsCache) cleanExpiredLocked(now time.Time) {
	for ip, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, ip)
		}
	}
}

// CheckNetgroupAccess checks if a client IP is allowed to access a share
// based on the share's netgroup configuration.
//
// Algorithm:
//  1. Get share from runtime state (not DB -- use cached share config)
//  2. If share has no NetgroupName -> return true (empty allowlist = allow all)
//  3. Get netgroup members from store
//  4. Match client IP against each member (IP, CIDR, or hostname)
//  5. Return false if no member matches
//
// Per Pitfall 3 (DNS blocking): hostname matching uses cached reverse DNS with
// 5-minute positive TTL and 1-minute negative TTL. Falls back to IP matching
// if DNS lookup fails (does not block).
func (r *Runtime) CheckNetgroupAccess(ctx context.Context, shareName string, clientIP net.IP) (bool, error) {
	// 1. Get share from runtime state
	r.mu.RLock()
	share, exists := r.shares[shareName]
	r.mu.RUnlock()

	if !exists {
		return false, nil
	}

	// 2. If share has no netgroup -> allow all
	if share.NetgroupName == "" {
		return true, nil
	}

	// 3. Get netgroup members from store (requires NetgroupStore interface)
	ns, ok := r.store.(store.NetgroupStore)
	if !ok {
		logger.Warn("Store does not implement NetgroupStore, denying access",
			"share", shareName,
			"netgroup", share.NetgroupName)
		return false, fmt.Errorf("store does not support netgroup operations")
	}

	members, err := ns.GetNetgroupMembers(ctx, share.NetgroupName)
	if err != nil {
		logger.Warn("Failed to get netgroup members, denying access",
			"share", shareName,
			"netgroup", share.NetgroupName,
			"error", err)
		return false, err
	}

	if len(members) == 0 {
		// Netgroup exists but has no members - deny access
		// (This is different from no netgroup at all, which allows all)
		return false, nil
	}

	// Initialize DNS cache lazily
	r.ensureDNSCache()

	// 4. Match client IP against each member
	ipString := clientIP.String()
	for _, member := range members {
		switch member.Type {
		case "ip":
			memberIP := net.ParseIP(member.Value)
			if memberIP != nil && memberIP.Equal(clientIP) {
				return true, nil
			}

		case "cidr":
			_, network, err := net.ParseCIDR(member.Value)
			if err == nil && network.Contains(clientIP) {
				return true, nil
			}

		case "hostname":
			if r.matchHostname(ipString, member.Value) {
				return true, nil
			}
		}
	}

	// 5. No member matches
	return false, nil
}

// matchHostname checks if a client IP's reverse DNS matches a hostname pattern.
// Supports wildcards: "*.example.com" matches any hostname ending in ".example.com".
// Falls back gracefully if DNS lookup fails (returns false, does not block).
func (r *Runtime) matchHostname(clientIP string, pattern string) bool {
	hostnames, err := r.dnsCache.lookupAddr(clientIP)
	if err != nil {
		// DNS lookup failed - fall back to no match
		logger.Debug("Reverse DNS lookup failed, falling back to IP matching",
			"client_ip", clientIP,
			"error", err)
		return false
	}

	for _, hostname := range hostnames {
		// Normalize: remove trailing dot from PTR records
		hostname = strings.TrimSuffix(hostname, ".")

		if strings.HasPrefix(pattern, "*.") {
			// Wildcard pattern: *.example.com matches any.example.com
			suffix := pattern[1:] // ".example.com"
			if strings.HasSuffix(strings.ToLower(hostname), strings.ToLower(suffix)) {
				return true
			}
		} else {
			// Exact match (case-insensitive)
			if strings.EqualFold(hostname, pattern) {
				return true
			}
		}
	}

	return false
}

// ensureDNSCache lazily initializes the DNS cache.
func (r *Runtime) ensureDNSCache() {
	r.dnsCacheOnce.Do(func() {
		r.dnsCache = newDNSCache(DefaultDNSCacheTTL, DefaultDNSCacheNegTTL)
	})
}
