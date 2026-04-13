package identity

import (
	"context"
	"errors"
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

	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})

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

	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "unknown"})
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

	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "x"})
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

	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	time.Sleep(5 * time.Millisecond)
	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})

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

	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	r.InvalidateCache()
	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})

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

	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "bob"})
	r.InvalidateKey("p", "alice")
	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "alice"})
	r.Resolve(context.Background(), &Credential{Provider: "p", ExternalID: "bob"})

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
