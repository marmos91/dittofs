package identity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockProvider struct {
	name     string
	canCheck func(*Credential) bool
	results  map[string]*ResolvedIdentity
	err      error
	mu       sync.Mutex
	calls    int
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) CanResolve(cred *Credential) bool {
	if m.canCheck != nil {
		return m.canCheck(cred)
	}
	return true
}

func (m *mockProvider) Resolve(_ context.Context, cred *Credential) (*ResolvedIdentity, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}
	if r, ok := m.results[cred.ExternalID]; ok {
		return r, nil
	}
	return &ResolvedIdentity{Found: false}, nil
}

func (m *mockProvider) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func TestResolver_DirectProviderRouting(t *testing.T) {
	krb := &mockProvider{
		name:    "kerberos",
		results: map[string]*ResolvedIdentity{"alice@EXAMPLE.COM": {Username: "alice", UID: 1000, Found: true}},
	}
	oidc := &mockProvider{
		name:    "oidc",
		results: map[string]*ResolvedIdentity{"iss|sub1": {Username: "bob", UID: 2000, Found: true}},
	}

	r := NewResolver(WithProvider(krb), WithProvider(oidc))

	result, err := r.Resolve(context.Background(), &Credential{Provider: "kerberos", ExternalID: "alice@EXAMPLE.COM"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Username != "alice" {
		t.Fatalf("expected alice, got %+v", result)
	}

	result, err = r.Resolve(context.Background(), &Credential{Provider: "oidc", ExternalID: "iss|sub1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Username != "bob" {
		t.Fatalf("expected bob, got %+v", result)
	}
}

func TestResolver_ChainFirstMatchWins(t *testing.T) {
	first := &mockProvider{
		name:    "first",
		results: map[string]*ResolvedIdentity{"shared": {Username: "from-first", Found: true}},
	}
	second := &mockProvider{
		name:    "second",
		results: map[string]*ResolvedIdentity{"shared": {Username: "from-second", Found: true}},
	}

	r := NewResolver(WithProvider(first), WithProvider(second))
	result, _ := r.Resolve(context.Background(), &Credential{ExternalID: "shared"})

	if result.Username != "from-first" {
		t.Fatalf("expected first provider to win, got %s", result.Username)
	}
	if second.callCount() != 0 {
		t.Fatal("second provider should not have been called")
	}
}

// TestResolver_PreferredProviderFallsThroughOnNotFound models the production
// Kerberos→LDAP chain: a credential tagged Provider="kerberos" is tried against
// the Kerberos provider first, but when that returns Found=false (the principal
// is not a local DittoFS user) resolution must FALL THROUGH to the directory
// provider that claims the principal by shape — not stop at the preferred
// provider. Before the fix a Provider-tagged not-found ended resolution, so
// every domain user authenticating over Kerberos resolved to nobody.
func TestResolver_PreferredProviderFallsThroughOnNotFound(t *testing.T) {
	krb := &mockProvider{name: "kerberos", results: map[string]*ResolvedIdentity{}}
	dir := &mockProvider{
		name:     "ldap",
		canCheck: func(c *Credential) bool { return strings.HasSuffix(c.ExternalID, "@AD") },
		results:  map[string]*ResolvedIdentity{"alice@AD": {Username: "alice", UID: 10001, GID: 10000, Found: true}},
	}

	r := NewResolver(WithProvider(krb), WithProvider(dir))
	result, err := r.Resolve(context.Background(), &Credential{Provider: "kerberos", ExternalID: "alice@AD"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.UID != 10001 || result.GID != 10000 {
		t.Fatalf("expected directory fall-through to resolve alice@AD to 10001:10000, got %+v", result)
	}
	if krb.callCount() != 1 {
		t.Fatalf("preferred kerberos provider should be tried exactly once, got %d", krb.callCount())
	}
	if dir.callCount() != 1 {
		t.Fatalf("directory provider should be tried once on fall-through, got %d", dir.callCount())
	}
}

// TestResolver_PreferredProviderErrorDoesNotFallThrough pins that an
// infrastructure error from the preferred provider is surfaced, not masked by
// falling through to other providers (a transient KDC/DB error must not be
// silently treated as "unmapped").
func TestResolver_PreferredProviderErrorDoesNotFallThrough(t *testing.T) {
	krb := &mockProvider{name: "kerberos", err: errors.New("kdc unreachable")}
	dir := &mockProvider{
		name:    "ldap",
		results: map[string]*ResolvedIdentity{"alice@AD": {Username: "alice", UID: 10001, Found: true}},
	}

	r := NewResolver(WithProvider(krb), WithProvider(dir))
	_, err := r.Resolve(context.Background(), &Credential{Provider: "kerberos", ExternalID: "alice@AD"})
	if err == nil {
		t.Fatal("expected preferred-provider error to be surfaced, got nil")
	}
	if dir.callCount() != 0 {
		t.Fatalf("directory provider must not be tried after a preferred-provider error, got %d", dir.callCount())
	}
}

func TestResolver_ChainFallthrough(t *testing.T) {
	first := &mockProvider{name: "first", results: map[string]*ResolvedIdentity{}}
	second := &mockProvider{
		name:    "second",
		results: map[string]*ResolvedIdentity{"alice": {Username: "alice", Found: true}},
	}

	r := NewResolver(WithProvider(first), WithProvider(second))
	result, _ := r.Resolve(context.Background(), &Credential{ExternalID: "alice"})

	if !result.Found || result.Username != "alice" {
		t.Fatalf("expected second provider to resolve, got %+v", result)
	}
}

func TestResolver_AllMissReturnsNotFound(t *testing.T) {
	p := &mockProvider{name: "empty", results: map[string]*ResolvedIdentity{}}
	r := NewResolver(WithProvider(p))

	result, err := r.Resolve(context.Background(), &Credential{ExternalID: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Found {
		t.Fatal("expected Found=false")
	}
}

func TestResolver_UnknownProviderReturnsNotFound(t *testing.T) {
	p := &mockProvider{name: "kerberos", results: map[string]*ResolvedIdentity{}}
	r := NewResolver(WithProvider(p))

	result, err := r.Resolve(context.Background(), &Credential{Provider: "nonexistent", ExternalID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Found {
		t.Fatal("expected Found=false for unknown provider")
	}
}

func TestResolver_CanResolveFalseSkipsProvider(t *testing.T) {
	skipped := &mockProvider{
		name:     "skip-me",
		canCheck: func(*Credential) bool { return false },
		results:  map[string]*ResolvedIdentity{"x": {Username: "nope", Found: true}},
	}
	fallback := &mockProvider{
		name:    "fallback",
		results: map[string]*ResolvedIdentity{"x": {Username: "yes", Found: true}},
	}

	r := NewResolver(WithProvider(skipped), WithProvider(fallback))
	result, _ := r.Resolve(context.Background(), &Credential{ExternalID: "x"})

	if result.Username != "yes" {
		t.Fatalf("expected fallback, got %s", result.Username)
	}
	if skipped.callCount() != 0 {
		t.Fatal("skipped provider should not have been called")
	}
}

func TestResolver_CachePositive(t *testing.T) {
	p := &mockProvider{
		name:    "p",
		results: map[string]*ResolvedIdentity{"alice": {Username: "alice", UID: 1000, Found: true}},
	}
	r := NewResolver(WithProvider(p), WithCacheTTL(time.Minute))

	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})

	if p.callCount() != 1 {
		t.Fatalf("expected 1 provider call (cached), got %d", p.callCount())
	}
	if r.Stats().Hits != 1 {
		t.Fatalf("expected 1 cache hit, got %d", r.Stats().Hits)
	}
}

func TestResolver_CacheNegative(t *testing.T) {
	p := &mockProvider{name: "p", results: map[string]*ResolvedIdentity{}}
	r := NewResolver(WithProvider(p), WithNegativeCacheTTL(time.Minute))

	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "unknown"})
	result, _ := r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "unknown"})

	if result.Found {
		t.Fatal("expected negative cache hit")
	}
	if p.callCount() != 1 {
		t.Fatalf("expected 1 provider call (negative cached), got %d", p.callCount())
	}
}

func TestResolver_CacheError(t *testing.T) {
	dbErr := errors.New("db down")
	p := &mockProvider{name: "p", err: dbErr}
	r := NewResolver(WithProvider(p), WithErrorCacheTTL(time.Minute))

	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "x"})
	_, err := r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "x"})

	if !errors.Is(err, dbErr) {
		t.Fatalf("expected cached error, got %v", err)
	}
	if p.callCount() != 1 {
		t.Fatalf("expected 1 provider call (error cached), got %d", p.callCount())
	}
}

func TestResolver_CacheExpiry(t *testing.T) {
	p := &mockProvider{
		name:    "p",
		results: map[string]*ResolvedIdentity{"alice": {Username: "alice", Found: true}},
	}
	r := NewResolver(WithProvider(p), WithCacheTTL(time.Millisecond))

	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	time.Sleep(5 * time.Millisecond)
	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})

	if p.callCount() != 2 {
		t.Fatalf("expected 2 calls after TTL expiry, got %d", p.callCount())
	}
}

func TestResolver_InvalidateCache(t *testing.T) {
	p := &mockProvider{
		name:    "p",
		results: map[string]*ResolvedIdentity{"alice": {Username: "alice", Found: true}},
	}
	r := NewResolver(WithProvider(p))

	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	r.InvalidateCache()
	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})

	if p.callCount() != 2 {
		t.Fatalf("expected 2 calls after invalidation, got %d", p.callCount())
	}
}

func TestResolver_InvalidateKey(t *testing.T) {
	p := &mockProvider{
		name: "p",
		results: map[string]*ResolvedIdentity{
			"alice": {Username: "alice", Found: true},
			"bob":   {Username: "bob", Found: true},
		},
	}
	r := NewResolver(WithProvider(p))

	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "bob"})
	r.InvalidateKey("p", "alice")
	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	_, _ = r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "bob"})

	// alice: 2 calls (invalidated), bob: 1 call (still cached)
	if p.callCount() != 3 {
		t.Fatalf("expected 3 total calls, got %d", p.callCount())
	}
}

func TestResolver_ConcurrentAccess(t *testing.T) {
	p := &mockProvider{
		name:    "p",
		results: map[string]*ResolvedIdentity{"alice": {Username: "alice", UID: 1000, Found: true}},
	}
	r := NewResolver(WithProvider(p))

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	var errCount atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			result, err := r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
			if err != nil || !result.Found || result.UID != 1000 {
				errCount.Add(1)
			}
		}()
	}

	wg.Wait()
	if errCount.Load() != 0 {
		t.Fatalf("got %d errors in concurrent test", errCount.Load())
	}
	if p.callCount() > 5 {
		t.Fatalf("expected at most ~5 provider calls with caching, got %d", p.callCount())
	}
}

// reverseMockProvider is a mockProvider that also implements ReverseResolver,
// used to exercise the resolver's reverse uid/gid lookup chain.
type reverseMockProvider struct {
	mockProvider
	uids     map[uint32]string
	gids     map[uint32]string
	domain   string
	uidCalls atomic.Int32
}

func (m *reverseMockProvider) LookupUID(_ context.Context, uid uint32) (string, string, bool) {
	m.uidCalls.Add(1)
	if n, ok := m.uids[uid]; ok {
		return n, m.domain, true
	}
	return "", "", false
}

func (m *reverseMockProvider) LookupGID(_ context.Context, gid uint32) (string, string, bool) {
	if n, ok := m.gids[gid]; ok {
		return n, m.domain, true
	}
	return "", "", false
}

// TestResolver_ReverseLookup_Chain verifies LookupUID/LookupGID consult only
// providers that implement ReverseResolver, first hit wins, and that a
// non-reverse provider in the chain is skipped without panicking.
func TestResolver_ReverseLookup_Chain(t *testing.T) {
	plain := &mockProvider{name: "kerberos"} // no ReverseResolver
	dir := &reverseMockProvider{
		mockProvider: mockProvider{name: "ldap"},
		uids:         map[uint32]string{10001: "alice"},
		gids:         map[uint32]string{10000: "domain users"},
		domain:       "DITTOFS.AD",
	}
	r := NewResolver(WithProvider(plain), WithProvider(dir))

	name, domain, ok := r.LookupUID(context.Background(), 10001)
	if !ok || name != "alice" || domain != "DITTOFS.AD" {
		t.Fatalf("LookupUID = (%q, %q, %v), want (alice, DITTOFS.AD, true)", name, domain, ok)
	}

	gname, _, ok := r.LookupGID(context.Background(), 10000)
	if !ok || gname != "domain users" {
		t.Fatalf("LookupGID = (%q, %v), want (domain users, true)", gname, ok)
	}

	if _, _, ok := r.LookupUID(context.Background(), 99999); ok {
		t.Error("LookupUID(99999) should miss")
	}
}

// TestResolver_ReverseLookup_Cached verifies a repeated reverse lookup is served
// from cache (the provider is queried once for a hit and once for a miss).
func TestResolver_ReverseLookup_Cached(t *testing.T) {
	dir := &reverseMockProvider{
		mockProvider: mockProvider{name: "ldap"},
		uids:         map[uint32]string{42: "bob"},
		domain:       "AD",
	}
	r := NewResolver(WithProvider(dir))

	for i := 0; i < 3; i++ {
		if name, _, ok := r.LookupUID(context.Background(), 42); !ok || name != "bob" {
			t.Fatalf("iter %d: LookupUID(42) = (%q, %v)", i, name, ok)
		}
	}
	if got := dir.uidCalls.Load(); got != 1 {
		t.Errorf("provider LookupUID called %d times, want 1 (cached)", got)
	}

	// A miss is also cached.
	for i := 0; i < 3; i++ {
		if _, _, ok := r.LookupUID(context.Background(), 7); ok {
			t.Fatalf("iter %d: LookupUID(7) should miss", i)
		}
	}
	if got := dir.uidCalls.Load(); got != 2 {
		t.Errorf("provider LookupUID called %d times total, want 2 (one hit + one miss, both cached)", got)
	}
}
