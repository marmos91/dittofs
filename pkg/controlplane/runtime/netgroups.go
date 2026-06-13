package runtime

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// Default DNS cache TTLs.
const (
	DefaultDNSCacheTTL    = 5 * time.Minute // Positive result cache
	DefaultDNSCacheNegTTL = 1 * time.Minute // Negative result (error) cache
)

// Package-level DNS cache for netgroup hostname matching.
// This was moved from Runtime struct fields to avoid NFS-specific state in the generic runtime.
var (
	pkgDNSCache     *dnsCache
	pkgDNSCacheOnce sync.Once
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
	hostnames []string  // reverse DNS results (or forward-lookup addresses for "fwd:" keys)
	err       error     // cached error (negative cache)
	expiresAt time.Time // when this entry expires
}

// dnsResolver abstracts the DNS lookups used by netgroup hostname matching so
// the matcher can be unit-tested without real network calls. *dnsCache is the
// production implementation.
type dnsResolver interface {
	lookupAddr(ip string) ([]string, error)       // PTR lookup (reverse)
	lookupHost(hostname string) ([]string, error) // A/AAAA lookup (forward)
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

// lookupHost performs a forward DNS lookup (A/AAAA) with caching.
// Used by FCrDNS verification: a PTR-derived hostname is only trusted if it
// resolves forward back to the original client IP. Cache entries are namespaced
// under "fwd:" to avoid colliding with reverse-lookup entries keyed by IP.
func (c *dnsCache) lookupHost(hostname string) ([]string, error) {
	now := time.Now()
	key := "fwd:" + hostname

	// Check cache under read lock
	c.mu.RLock()
	entry, exists := c.entries[key]
	c.mu.RUnlock()

	if exists && now.Before(entry.expiresAt) {
		return entry.hostnames, entry.err
	}

	// Cache miss or expired - perform lookup
	addrs, err := net.LookupHost(hostname)

	// Store result
	var ttl time.Duration
	if err != nil {
		ttl = c.negTTL
	} else {
		ttl = c.ttl
	}

	c.mu.Lock()
	c.entries[key] = &dnsCacheEntry{
		hostnames: addrs,
		err:       err,
		expiresAt: now.Add(ttl),
	}
	// Piggyback cleanup of expired entries
	c.cleanExpiredLocked(now)
	c.mu.Unlock()

	return addrs, err
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
	// 1. Get the share's netgroup association from runtime state.
	//
	// Read NetgroupName via the locked accessor rather than off a *Share
	// returned by GetShare: GetShare hands back the shared registry pointer
	// with the lock already dropped, so reading the field there would race
	// SetShareNetgroup's write under the same lock.
	//
	// A lookup miss here (unknown / renamed / partially-loaded share) is NOT a
	// legitimate netgroup deny: it signals config drift between the requested
	// share and the runtime registry. Returning a bare (false, nil) would be
	// indistinguishable from "client not in allowlist" and silently invisible.
	// Surface it as a wrapped ErrShareNotFound so it is diagnosable while still
	// denying access (the mount handler treats any error as fail-closed).
	netgroupName, exists := r.sharesSvc.GetShareNetgroupName(shareName)
	if !exists {
		err := fmt.Errorf("%w: %q", shares.ErrShareNotFound, shareName)
		logger.Warn("Netgroup access denied: share not found in runtime registry",
			"share", shareName, "error", err)
		return false, err
	}

	// 2. If share has no netgroup -> allow all
	if netgroupName == "" {
		return true, nil
	}

	// 3. Get netgroup members from store (requires NetgroupStore interface)
	ns, ok := r.store.(store.NetgroupStore)
	if !ok {
		logger.Warn("Store does not implement NetgroupStore, denying access",
			"share", shareName,
			"netgroup", netgroupName)
		return false, fmt.Errorf("store does not support netgroup operations")
	}

	members, err := ns.GetNetgroupMembers(ctx, netgroupName)
	if err != nil {
		logger.Warn("Failed to get netgroup members, denying access",
			"share", shareName,
			"netgroup", netgroupName,
			"error", err)
		return false, err
	}

	if len(members) == 0 {
		// Netgroup exists but has no members - deny access
		// (This is different from no netgroup at all, which allows all)
		return false, nil
	}

	// Initialize DNS cache lazily
	ensureDNSCache()

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
			if matchHostname(pkgDNSCache, ipString, member.Value) {
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
//
// Each PTR-derived candidate hostname is verified with Forward-Confirmed reverse
// DNS (FCrDNS): the candidate is only trusted for pattern matching if a forward
// lookup of that hostname resolves back to the original client IP. This defeats
// PTR-spoofing, where an attacker who controls the reverse zone for their own IP
// points it at a trusted hostname they do not actually own.
func matchHostname(r dnsResolver, clientIP string, pattern string) bool {
	hostnames, err := r.lookupAddr(clientIP)
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

		// FCrDNS: the candidate hostname must resolve forward back to the
		// client IP, otherwise a spoofed PTR record could impersonate any
		// trusted hostname. Skip candidates that do not round-trip.
		if !forwardConfirms(r, hostname, clientIP) {
			logger.Debug("FCrDNS verification failed, skipping PTR candidate",
				"client_ip", clientIP,
				"candidate", hostname)
			continue
		}

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

// forwardConfirms reports whether a forward lookup of hostname yields an address
// equal to clientIP, completing the Forward-Confirmed reverse DNS check.
func forwardConfirms(r dnsResolver, hostname, clientIP string) bool {
	addrs, err := r.lookupHost(hostname)
	if err != nil {
		return false
	}
	want := net.ParseIP(clientIP)
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && want != nil && ip.Equal(want) {
			return true
		}
		// Fall back to string comparison if either side is unparseable.
		if addr == clientIP {
			return true
		}
	}
	return false
}

// ensureDNSCache lazily initializes the package-level DNS cache.
func ensureDNSCache() {
	pkgDNSCacheOnce.Do(func() {
		pkgDNSCache = newDNSCache(DefaultDNSCacheTTL, DefaultDNSCacheNegTTL)
	})
}
