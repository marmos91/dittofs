package gss

import (
	"sync"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/types"
)

// newTestContext creates a GSSContext with the given principal for testing.
func newTestContext(principal, realm string) *GSSContext {
	handle, err := generateHandle()
	if err != nil {
		panic(err)
	}
	now := time.Now()
	return &GSSContext{
		Handle:    handle,
		Principal: principal,
		Realm:     realm,
		SessionKey: types.EncryptionKey{
			KeyType:  17, // aes128-cts-hmac-sha1-96
			KeyValue: []byte("0123456789abcdef"),
		},
		SeqWindow: NewSeqWindow(128),
		Service:   RPCGSSSvcIntegrity,
		CreatedAt: now,
		LastUsed:  now,
	}
}

func TestContextStoreAndLookup(t *testing.T) {
	store := NewContextStore(100, 10*time.Minute)
	defer store.Stop()

	ctx := newTestContext("alice", "EXAMPLE.COM")
	store.Store(ctx)

	// Lookup should find the context
	found, ok := store.Lookup(ctx.Handle)
	if !ok {
		t.Fatal("expected context to be found")
	}
	if found.Principal != "alice" {
		t.Fatalf("expected principal alice, got %s", found.Principal)
	}
	if found.Realm != "EXAMPLE.COM" {
		t.Fatalf("expected realm EXAMPLE.COM, got %s", found.Realm)
	}
	if found.Service != RPCGSSSvcIntegrity {
		t.Fatalf("expected service %d, got %d", RPCGSSSvcIntegrity, found.Service)
	}
}

func TestContextLookupUnknownHandle(t *testing.T) {
	store := NewContextStore(100, 10*time.Minute)
	defer store.Stop()

	_, ok := store.Lookup([]byte("nonexistent-handle"))
	if ok {
		t.Fatal("expected context not to be found for unknown handle")
	}
}

func TestContextDelete(t *testing.T) {
	store := NewContextStore(100, 10*time.Minute)
	defer store.Stop()

	ctx := newTestContext("bob", "EXAMPLE.COM")
	store.Store(ctx)

	// Verify it exists
	if _, ok := store.Lookup(ctx.Handle); !ok {
		t.Fatal("expected context to be found before delete")
	}

	// Delete it
	store.Delete(ctx.Handle)

	// Verify it's gone
	if _, ok := store.Lookup(ctx.Handle); ok {
		t.Fatal("expected context to be removed after delete")
	}
}

func TestContextDeleteNonexistent(t *testing.T) {
	store := NewContextStore(100, 10*time.Minute)
	defer store.Stop()

	// Should not panic or error
	store.Delete([]byte("does-not-exist"))
}

func TestContextTTLCleanup(t *testing.T) {
	// Use a very short TTL so we can test cleanup
	store := NewContextStore(100, 10*time.Millisecond)
	defer store.Stop()

	ctx := newTestContext("charlie", "EXAMPLE.COM")
	store.Store(ctx)

	// Verify it exists
	if store.Count() != 1 {
		t.Fatalf("expected 1 context, got %d", store.Count())
	}

	// Wait for the context to expire
	time.Sleep(50 * time.Millisecond)

	// Trigger cleanup manually (don't wait for the 5-minute ticker)
	store.cleanup()

	// Verify it was cleaned up
	if store.Count() != 0 {
		t.Fatalf("expected 0 contexts after TTL cleanup, got %d", store.Count())
	}
}

func TestContextTTLKeepsFreshContexts(t *testing.T) {
	store := NewContextStore(100, 1*time.Second)
	defer store.Stop()

	ctx := newTestContext("dave", "EXAMPLE.COM")
	store.Store(ctx)

	// Trigger cleanup immediately - context should still be fresh
	store.cleanup()

	if store.Count() != 1 {
		t.Fatalf("expected 1 context (still fresh), got %d", store.Count())
	}
}

func TestContextMaxContextsEviction(t *testing.T) {
	store := NewContextStore(3, 10*time.Minute)
	defer store.Stop()

	// Create 3 contexts with staggered LastUsed
	ctx1 := newTestContext("first", "EXAMPLE.COM")
	ctx1.LastUsed = time.Now().Add(-3 * time.Minute) // oldest
	store.Store(ctx1)

	ctx2 := newTestContext("second", "EXAMPLE.COM")
	ctx2.LastUsed = time.Now().Add(-2 * time.Minute)
	store.Store(ctx2)

	ctx3 := newTestContext("third", "EXAMPLE.COM")
	ctx3.LastUsed = time.Now().Add(-1 * time.Minute)
	store.Store(ctx3)

	if store.Count() != 3 {
		t.Fatalf("expected 3 contexts, got %d", store.Count())
	}

	// Store a 4th context - should evict the oldest (ctx1)
	ctx4 := newTestContext("fourth", "EXAMPLE.COM")
	store.Store(ctx4)

	if store.Count() != 3 {
		t.Fatalf("expected 3 contexts after eviction, got %d", store.Count())
	}

	// The oldest (ctx1) should have been evicted
	if _, ok := store.Lookup(ctx1.Handle); ok {
		t.Fatal("expected oldest context (first) to be evicted")
	}

	// Others should still exist
	if _, ok := store.Lookup(ctx2.Handle); !ok {
		t.Fatal("expected second context to still exist")
	}
	if _, ok := store.Lookup(ctx3.Handle); !ok {
		t.Fatal("expected third context to still exist")
	}
	if _, ok := store.Lookup(ctx4.Handle); !ok {
		t.Fatal("expected fourth context to still exist")
	}
}

func TestContextConcurrentAccess(t *testing.T) {
	store := NewContextStore(1000, 10*time.Minute)
	defer store.Stop()

	// Pre-create some contexts
	var contexts []*GSSContext
	for i := 0; i < 50; i++ {
		ctx := newTestContext("concurrent", "EXAMPLE.COM")
		contexts = append(contexts, ctx)
	}

	var wg sync.WaitGroup

	// Concurrent Store
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			store.Store(contexts[idx])
		}(i)
	}

	wg.Wait()

	// Concurrent Lookup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			store.Lookup(contexts[idx].Handle)
		}(i)
	}

	wg.Wait()

	// Concurrent Delete
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			store.Delete(contexts[idx].Handle)
		}(i)
	}

	wg.Wait()

	// Verify remaining count
	count := store.Count()
	if count != 25 {
		t.Fatalf("expected 25 contexts after concurrent ops, got %d", count)
	}
}

func TestContextLookupUpdatesLastUsed(t *testing.T) {
	store := NewContextStore(100, 10*time.Minute)
	defer store.Stop()

	ctx := newTestContext("eve", "EXAMPLE.COM")
	originalTime := time.Now().Add(-5 * time.Minute)
	ctx.LastUsed = originalTime
	store.Store(ctx)

	// Lookup should update LastUsed
	found, ok := store.Lookup(ctx.Handle)
	if !ok {
		t.Fatal("expected context to be found")
	}

	lastUsed := found.GetLastUsed()
	if !lastUsed.After(originalTime) {
		t.Fatalf("expected LastUsed to be updated after lookup, was %v (original %v)", lastUsed, originalTime)
	}
}

func TestContextCount(t *testing.T) {
	store := NewContextStore(100, 10*time.Minute)
	defer store.Stop()

	if store.Count() != 0 {
		t.Fatalf("expected 0 contexts in empty store, got %d", store.Count())
	}

	ctx1 := newTestContext("a", "EXAMPLE.COM")
	ctx2 := newTestContext("b", "EXAMPLE.COM")
	store.Store(ctx1)
	store.Store(ctx2)

	if store.Count() != 2 {
		t.Fatalf("expected 2 contexts, got %d", store.Count())
	}

	store.Delete(ctx1.Handle)
	if store.Count() != 1 {
		t.Fatalf("expected 1 context after delete, got %d", store.Count())
	}
}

func TestGenerateHandle(t *testing.T) {
	handle1, err := generateHandle()
	if err != nil {
		t.Fatalf("generateHandle failed: %v", err)
	}
	if len(handle1) != 16 {
		t.Fatalf("expected 16-byte handle, got %d bytes", len(handle1))
	}

	// Two handles should be different (crypto/rand)
	handle2, err := generateHandle()
	if err != nil {
		t.Fatalf("generateHandle failed: %v", err)
	}

	if string(handle1) == string(handle2) {
		t.Fatal("two generated handles should not be identical")
	}
}

func TestContextUnlimitedMaxContexts(t *testing.T) {
	// maxContexts=0 means unlimited
	store := NewContextStore(0, 10*time.Minute)
	defer store.Stop()

	// Store many contexts without eviction
	for i := 0; i < 100; i++ {
		ctx := newTestContext("unlimited", "EXAMPLE.COM")
		store.Store(ctx)
	}

	if store.Count() != 100 {
		t.Fatalf("expected 100 contexts with unlimited max, got %d", store.Count())
	}
}
