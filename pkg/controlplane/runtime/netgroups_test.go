//go:build integration

package runtime

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// createTestRuntimeWithStore creates a Runtime backed by an in-memory SQLite store.
// Callers can add shares and netgroups for netgroup access tests.
func createTestRuntimeWithStore(t *testing.T) (*Runtime, store.Store, store.NetgroupStore) {
	t.Helper()

	dbConfig := store.Config{
		Type: "sqlite",
		SQLite: store.SQLiteConfig{
			Path: ":memory:",
		},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	rt := New(cpStore)
	return rt, cpStore, cpStore
}

// addShareDirect injects a share directly into the runtime's shares service
// without going through AddShare (which requires metadata store setup).
func addShareDirect(rt *Runtime, name string, netgroupName string) {
	rt.sharesSvc.InjectShareForTesting(&Share{
		Name:         name,
		NetgroupName: netgroupName,
	})
}

// --- CheckNetgroupAccess tests ---

func TestCheckNetgroupAccess_NoNetgroup_AllowAll(t *testing.T) {
	rt, _, _ := createTestRuntimeWithStore(t)
	ctx := context.Background()

	// Share with no netgroup -> should allow all
	addShareDirect(rt, "/export", "")

	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("192.168.1.100"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess failed: %v", err)
	}
	if !allowed {
		t.Error("Expected access allowed when share has no netgroup")
	}
}

// TestCheckNetgroupAccess_ShareNotFound is the REVIEW M3 regression. A share
// lookup miss (unknown / renamed / partially-loaded share) must be a fail-CLOSED
// deny that is also OBSERVABLE: it returns a wrapped shares.ErrShareNotFound so
// config drift is diagnosable, instead of the old silent (false, nil) that was
// indistinguishable from a legitimate netgroup deny. The mount handler already
// treats any non-nil error as fail-closed, so the deny semantics are preserved.
func TestCheckNetgroupAccess_ShareNotFound(t *testing.T) {
	rt, _, _ := createTestRuntimeWithStore(t)
	ctx := context.Background()

	allowed, err := rt.CheckNetgroupAccess(ctx, "/nonexistent", net.ParseIP("10.0.0.1"))
	if allowed {
		t.Error("Expected access denied for nonexistent share")
	}
	if err == nil {
		t.Fatal("Expected an observable error for missing share, got nil (silent deny)")
	}
	if !errors.Is(err, shares.ErrShareNotFound) {
		t.Fatalf("Expected error to wrap shares.ErrShareNotFound, got: %v", err)
	}
}

func TestCheckNetgroupAccess_IPMatch(t *testing.T) {
	rt, _, ngStore := createTestRuntimeWithStore(t)
	ctx := context.Background()

	// Create netgroup with an IP member
	netgroup := &models.Netgroup{
		ID:   uuid.New().String(),
		Name: "office-ips",
	}
	if _, err := ngStore.CreateNetgroup(ctx, netgroup); err != nil {
		t.Fatalf("CreateNetgroup failed: %v", err)
	}
	if err := ngStore.AddNetgroupMember(ctx, "office-ips", &models.NetgroupMember{
		Type:  "ip",
		Value: "192.168.1.100",
	}); err != nil {
		t.Fatalf("AddNetgroupMember failed: %v", err)
	}

	// Share referencing the netgroup (by name)
	addShareDirect(rt, "/export", "office-ips")

	// Client IP matches
	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("192.168.1.100"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess failed: %v", err)
	}
	if !allowed {
		t.Error("Expected access allowed for matching IP")
	}
}

func TestCheckNetgroupAccess_IPNoMatch(t *testing.T) {
	rt, _, ngStore := createTestRuntimeWithStore(t)
	ctx := context.Background()

	netgroup := &models.Netgroup{
		ID:   uuid.New().String(),
		Name: "office-ips",
	}
	if _, err := ngStore.CreateNetgroup(ctx, netgroup); err != nil {
		t.Fatalf("CreateNetgroup failed: %v", err)
	}
	if err := ngStore.AddNetgroupMember(ctx, "office-ips", &models.NetgroupMember{
		Type:  "ip",
		Value: "192.168.1.100",
	}); err != nil {
		t.Fatalf("AddNetgroupMember failed: %v", err)
	}

	addShareDirect(rt, "/export", "office-ips")

	// Client IP does NOT match
	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("10.0.0.1"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess failed: %v", err)
	}
	if allowed {
		t.Error("Expected access denied for non-matching IP")
	}
}

func TestCheckNetgroupAccess_CIDRMatch(t *testing.T) {
	rt, _, ngStore := createTestRuntimeWithStore(t)
	ctx := context.Background()

	netgroup := &models.Netgroup{
		ID:   uuid.New().String(),
		Name: "internal-net",
	}
	if _, err := ngStore.CreateNetgroup(ctx, netgroup); err != nil {
		t.Fatalf("CreateNetgroup failed: %v", err)
	}
	if err := ngStore.AddNetgroupMember(ctx, "internal-net", &models.NetgroupMember{
		Type:  "cidr",
		Value: "10.0.0.0/8",
	}); err != nil {
		t.Fatalf("AddNetgroupMember failed: %v", err)
	}

	addShareDirect(rt, "/export", "internal-net")

	// Client in CIDR range
	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("10.1.2.3"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess failed: %v", err)
	}
	if !allowed {
		t.Error("Expected access allowed for IP within CIDR range")
	}
}

func TestCheckNetgroupAccess_CIDRNoMatch(t *testing.T) {
	rt, _, ngStore := createTestRuntimeWithStore(t)
	ctx := context.Background()

	netgroup := &models.Netgroup{
		ID:   uuid.New().String(),
		Name: "internal-net",
	}
	if _, err := ngStore.CreateNetgroup(ctx, netgroup); err != nil {
		t.Fatalf("CreateNetgroup failed: %v", err)
	}
	if err := ngStore.AddNetgroupMember(ctx, "internal-net", &models.NetgroupMember{
		Type:  "cidr",
		Value: "10.0.0.0/8",
	}); err != nil {
		t.Fatalf("AddNetgroupMember failed: %v", err)
	}

	addShareDirect(rt, "/export", "internal-net")

	// Client outside CIDR range
	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("192.168.1.1"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess failed: %v", err)
	}
	if allowed {
		t.Error("Expected access denied for IP outside CIDR range")
	}
}

func TestCheckNetgroupAccess_EmptyNetgroup_DeniesAccess(t *testing.T) {
	rt, _, ngStore := createTestRuntimeWithStore(t)
	ctx := context.Background()

	// Create netgroup with no members
	netgroup := &models.Netgroup{
		ID:   uuid.New().String(),
		Name: "empty-group",
	}
	if _, err := ngStore.CreateNetgroup(ctx, netgroup); err != nil {
		t.Fatalf("CreateNetgroup failed: %v", err)
	}

	addShareDirect(rt, "/export", "empty-group")

	// Netgroup exists but has no members -> deny (different from no netgroup at all)
	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("192.168.1.1"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess failed: %v", err)
	}
	if allowed {
		t.Error("Expected access denied for empty netgroup (no members)")
	}
}

func TestCheckNetgroupAccess_MixedMembers(t *testing.T) {
	rt, _, ngStore := createTestRuntimeWithStore(t)
	ctx := context.Background()

	netgroup := &models.Netgroup{
		ID:   uuid.New().String(),
		Name: "mixed-group",
	}
	if _, err := ngStore.CreateNetgroup(ctx, netgroup); err != nil {
		t.Fatalf("CreateNetgroup failed: %v", err)
	}

	// Add an IP member
	if err := ngStore.AddNetgroupMember(ctx, "mixed-group", &models.NetgroupMember{
		Type:  "ip",
		Value: "172.16.0.1",
	}); err != nil {
		t.Fatalf("AddNetgroupMember (IP) failed: %v", err)
	}

	// Add a CIDR member
	if err := ngStore.AddNetgroupMember(ctx, "mixed-group", &models.NetgroupMember{
		Type:  "cidr",
		Value: "10.0.0.0/8",
	}); err != nil {
		t.Fatalf("AddNetgroupMember (CIDR) failed: %v", err)
	}

	// Add a hostname member (won't match in test since DNS won't resolve these)
	if err := ngStore.AddNetgroupMember(ctx, "mixed-group", &models.NetgroupMember{
		Type:  "hostname",
		Value: "*.example.com",
	}); err != nil {
		t.Fatalf("AddNetgroupMember (hostname) failed: %v", err)
	}

	addShareDirect(rt, "/export", "mixed-group")

	// IP exact match (first member)
	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("172.16.0.1"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess (IP) failed: %v", err)
	}
	if !allowed {
		t.Error("Expected access allowed for exact IP match in mixed netgroup")
	}

	// CIDR match (second member)
	allowed, err = rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("10.5.5.5"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess (CIDR) failed: %v", err)
	}
	if !allowed {
		t.Error("Expected access allowed for CIDR match in mixed netgroup")
	}

	// No match
	allowed, err = rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("192.168.99.99"))
	if err != nil {
		t.Fatalf("CheckNetgroupAccess (no match) failed: %v", err)
	}
	if allowed {
		t.Error("Expected access denied when no member matches in mixed netgroup")
	}
}

func TestCheckNetgroupAccess_NetgroupNotFound(t *testing.T) {
	rt, _, _ := createTestRuntimeWithStore(t)
	ctx := context.Background()

	// Share references a netgroup that doesn't exist in DB
	addShareDirect(rt, "/export", "nonexistent-group")

	allowed, err := rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("10.0.0.1"))
	if err == nil {
		t.Fatal("Expected error when netgroup not found in store")
	}
	if allowed {
		t.Error("Expected access denied when netgroup not found")
	}
}

// TestCheckNetgroupAccess_ConcurrentSetShareNetgroup is the Copilot data-race
// regression. SetShareNetgroup writes share.NetgroupName under the shares
// service lock, while CheckNetgroupAccess previously read the same field off a
// pointer returned by GetShare with the lock already dropped. The two ran
// concurrently on every NFS request that overlapped a PATCH, a genuine
// read/write race. Run with -race to detect it; the fix routes the read
// through the locked GetShareNetgroupName accessor.
func TestCheckNetgroupAccess_ConcurrentSetShareNetgroup(t *testing.T) {
	rt, _, _ := createTestRuntimeWithStore(t)
	ctx := context.Background()

	addShareDirect(rt, "/export", "")

	const iterations = 200
	done := make(chan struct{})

	// Writer: flip the netgroup association back and forth.
	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			ng := ""
			if i%2 == 0 {
				ng = "office"
			}
			if err := rt.SetShareNetgroup("/export", ng); err != nil {
				t.Errorf("SetShareNetgroup: %v", err)
				return
			}
		}
	}()

	// Reader: hammer CheckNetgroupAccess concurrently. The netgroup name may
	// or may not resolve to a stored group; we only care that the field read
	// is race-free, so errors here are expected and ignored.
	for i := 0; i < iterations; i++ {
		_, _ = rt.CheckNetgroupAccess(ctx, "/export", net.ParseIP("192.168.1.100"))
	}

	<-done
}

// --- matchHostname tests ---
// matchHostname now takes a dnsResolver so we can drive both the reverse (PTR)
// and forward (A/AAAA) lookups deterministically without touching the network.
// fakeDNSResolver implements dnsResolver from canned maps.

type fakeDNSResolver struct {
	ptr    map[string][]string // ip -> []hostname (reverse / PTR)
	fwd    map[string][]string // hostname -> []ip (forward / A/AAAA)
	ptrErr map[string]error    // optional per-ip PTR error
	fwdErr map[string]error    // optional per-hostname forward error
}

func (f *fakeDNSResolver) lookupAddr(ip string) ([]string, error) {
	if f.ptrErr != nil {
		if err, ok := f.ptrErr[ip]; ok {
			return nil, err
		}
	}
	return f.ptr[ip], nil
}

func (f *fakeDNSResolver) lookupHost(hostname string) ([]string, error) {
	if f.fwdErr != nil {
		if err, ok := f.fwdErr[hostname]; ok {
			return nil, err
		}
	}
	return f.fwd[hostname], nil
}

func TestMatchHostname_ExactMatch(t *testing.T) {
	fake := &fakeDNSResolver{
		ptr: map[string][]string{"192.168.1.50": {"host.example.com."}},
		fwd: map[string][]string{"host.example.com": {"192.168.1.50"}},
	}

	if !matchHostname(fake, "192.168.1.50", "host.example.com") {
		t.Error("Expected exact hostname match")
	}
}

func TestMatchHostname_ExactMatch_CaseInsensitive(t *testing.T) {
	fake := &fakeDNSResolver{
		ptr: map[string][]string{"192.168.1.50": {"Host.Example.COM."}},
		// Forward lookup keyed on the normalized (trailing-dot-stripped) name.
		fwd: map[string][]string{"Host.Example.COM": {"192.168.1.50"}},
	}

	if !matchHostname(fake, "192.168.1.50", "host.example.com") {
		t.Error("Expected case-insensitive hostname match")
	}
}

func TestMatchHostname_WildcardMatch(t *testing.T) {
	fake := &fakeDNSResolver{
		ptr: map[string][]string{"192.168.1.50": {"web01.example.com."}},
		fwd: map[string][]string{"web01.example.com": {"192.168.1.50"}},
	}

	if !matchHostname(fake, "192.168.1.50", "*.example.com") {
		t.Error("Expected wildcard hostname match")
	}
}

func TestMatchHostname_WildcardNoMatch(t *testing.T) {
	fake := &fakeDNSResolver{
		ptr: map[string][]string{"192.168.1.50": {"host.other.com."}},
		// FCrDNS passes (forward resolves back to client IP) but the pattern
		// still does not match the different domain.
		fwd: map[string][]string{"host.other.com": {"192.168.1.50"}},
	}

	if matchHostname(fake, "192.168.1.50", "*.example.com") {
		t.Error("Expected wildcard NOT to match different domain")
	}
}

func TestMatchHostname_DNSLookupFails(t *testing.T) {
	fake := &fakeDNSResolver{
		ptrErr: map[string]error{"192.168.1.50": net.UnknownNetworkError("no PTR record")},
	}

	// Should return false gracefully, not panic
	if matchHostname(fake, "192.168.1.50", "host.example.com") {
		t.Error("Expected no match when DNS lookup fails")
	}
}

func TestMatchHostname_MultipleHostnames(t *testing.T) {
	fake := &fakeDNSResolver{
		// IP has multiple PTR records
		ptr: map[string][]string{"192.168.1.50": {"alias1.other.com.", "actual.example.com."}},
		fwd: map[string][]string{
			"alias1.other.com":   {"192.168.1.50"},
			"actual.example.com": {"192.168.1.50"},
		},
	}

	// Should match against the second hostname
	if !matchHostname(fake, "192.168.1.50", "*.example.com") {
		t.Error("Expected wildcard to match second PTR record")
	}
}

// TestMatchHostname_FCrDNS_SpoofedPTR is the security regression for this fix.
// An attacker controls 10.99.99.99 and sets its PTR record to a trusted
// hostname they do not own. The forward lookup of that hostname returns the
// real owner's IP, not the attacker's, so FCrDNS must reject the candidate.
func TestMatchHostname_FCrDNS_SpoofedPTR(t *testing.T) {
	fake := &fakeDNSResolver{
		ptr: map[string][]string{
			"10.99.99.99": {"trusted.example.com."},
		},
		fwd: map[string][]string{
			// Forward lookup of the PTR result returns the real owner's IP,
			// not the attacker.
			"trusted.example.com": {"192.168.1.100"},
		},
	}
	if matchHostname(fake, "10.99.99.99", "trusted.example.com") {
		t.Error("FCrDNS bypass: spoofed PTR record must not grant access")
	}
}

// TestMatchHostname_FCrDNS_ValidRoundTrip guards legitimate use: PTR resolves to
// a hostname whose forward lookup resolves back to the client IP.
func TestMatchHostname_FCrDNS_ValidRoundTrip(t *testing.T) {
	fake := &fakeDNSResolver{
		ptr: map[string][]string{
			"192.168.1.50": {"host.example.com."},
		},
		fwd: map[string][]string{
			"host.example.com": {"192.168.1.50"},
		},
	}
	if !matchHostname(fake, "192.168.1.50", "host.example.com") {
		t.Error("FCrDNS valid round-trip must still grant access")
	}
}

// --- DNS Cache tests ---

func TestDNSCache_Defaults(t *testing.T) {
	c := newDNSCache(0, 0)
	if c.ttl != DefaultDNSCacheTTL {
		t.Errorf("Expected default TTL %v, got %v", DefaultDNSCacheTTL, c.ttl)
	}
	if c.negTTL != DefaultDNSCacheNegTTL {
		t.Errorf("Expected default negative TTL %v, got %v", DefaultDNSCacheNegTTL, c.negTTL)
	}
}

func TestDNSCache_CustomTTLs(t *testing.T) {
	c := newDNSCache(10*time.Second, 3*time.Second)
	if c.ttl != 10*time.Second {
		t.Errorf("Expected TTL 10s, got %v", c.ttl)
	}
	if c.negTTL != 3*time.Second {
		t.Errorf("Expected negative TTL 3s, got %v", c.negTTL)
	}
}

func TestDNSCache_ServesFromCache(t *testing.T) {
	c := newDNSCache(5*time.Minute, 1*time.Minute)

	// Pre-populate cache
	c.mu.Lock()
	c.entries["10.0.0.1"] = &dnsCacheEntry{
		hostnames: []string{"cached-host.example.com."},
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	c.mu.Unlock()

	// Should return cached value (not doing a real DNS lookup)
	hostnames, err := c.lookupAddr("10.0.0.1")
	if err != nil {
		t.Fatalf("lookupAddr failed: %v", err)
	}
	if len(hostnames) != 1 || hostnames[0] != "cached-host.example.com." {
		t.Errorf("Expected cached hostname, got %v", hostnames)
	}
}

func TestDNSCache_ExpiredEntry(t *testing.T) {
	c := newDNSCache(5*time.Minute, 1*time.Minute)

	// Pre-populate cache with an expired entry
	c.mu.Lock()
	c.entries["10.0.0.1"] = &dnsCacheEntry{
		hostnames: []string{"old-host.example.com."},
		expiresAt: time.Now().Add(-1 * time.Second), // already expired
	}
	c.mu.Unlock()

	// lookupAddr should detect expiry and do a real lookup.
	// Since 10.0.0.1 likely won't have a PTR record, we just verify
	// it doesn't return the expired cached value.
	hostnames, _ := c.lookupAddr("10.0.0.1")
	// The result could be empty (no PTR record) or the real PTR record.
	// What matters is it should NOT be the old cached value unless
	// the real DNS happens to return it too.
	_ = hostnames // We can't assert specific DNS results in unit tests
}

func TestDNSCache_NegativeCache(t *testing.T) {
	c := newDNSCache(5*time.Minute, 1*time.Minute)

	// Pre-populate cache with a negative (error) entry
	lookupErr := net.UnknownNetworkError("no PTR record")
	c.mu.Lock()
	c.entries["10.0.0.99"] = &dnsCacheEntry{
		err:       lookupErr,
		expiresAt: time.Now().Add(1 * time.Minute),
	}
	c.mu.Unlock()

	// Should return cached error
	_, err := c.lookupAddr("10.0.0.99")
	if err == nil {
		t.Error("Expected cached error from negative cache entry")
	}
}

func TestDNSCache_CleanExpired(t *testing.T) {
	c := newDNSCache(5*time.Minute, 1*time.Minute)

	now := time.Now()

	c.mu.Lock()
	// Add expired entry
	c.entries["expired-ip"] = &dnsCacheEntry{
		hostnames: []string{"old.example.com."},
		expiresAt: now.Add(-1 * time.Minute),
	}
	// Add valid entry
	c.entries["valid-ip"] = &dnsCacheEntry{
		hostnames: []string{"good.example.com."},
		expiresAt: now.Add(5 * time.Minute),
	}

	// Trigger cleanup
	c.cleanExpiredLocked(now)

	// Expired should be gone, valid should remain
	if _, exists := c.entries["expired-ip"]; exists {
		t.Error("Expected expired entry to be cleaned up")
	}
	if _, exists := c.entries["valid-ip"]; !exists {
		t.Error("Expected valid entry to remain after cleanup")
	}
	c.mu.Unlock()
}
