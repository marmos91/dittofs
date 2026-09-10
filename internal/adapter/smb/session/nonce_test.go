package session

import (
	"sync"
	"testing"
)

// TestNextNonce_MonotonicAndNonRepeating pins the per-session encrypt-nonce
// counter: nonces issued from the same crypto state are unique (the counter
// increments per call) — MS-SMB2 3.1.4.3 forbids AEAD nonce reuse under the
// same key.
func TestNextNonce_MonotonicAndNonRepeating(t *testing.T) {
	t.Parallel()

	cs := &SessionCryptoState{nonce: &nonceState{}}
	const nonceSize = 12
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		nonce, err := cs.NextNonce(nonceSize)
		if err != nil {
			t.Fatalf("NextNonce %d: %v", i, err)
		}
		if len(nonce) != nonceSize {
			t.Fatalf("nonce %d: len = %d, want %d", i, len(nonce), nonceSize)
		}
		key := string(nonce)
		if seen[key] {
			t.Fatalf("nonce %d repeats a previously issued nonce — AEAD nonce reuse", i)
		}
		seen[key] = true
	}
}

// TestNextNonce_CCMNonceSize pins NextNonce against AES-CCM's 11-byte nonce
// (the mandatory SMB 3.0/3.0.2 fallback cipher): the counter must encode into
// the 7-byte tail without panicking, and consecutive nonces must stay unique.
func TestNextNonce_CCMNonceSize(t *testing.T) {
	t.Parallel()

	cs := &SessionCryptoState{nonce: &nonceState{}}
	const ccmNonceSize = 11
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		nonce, err := cs.NextNonce(ccmNonceSize)
		if err != nil {
			t.Fatalf("NextNonce %d: %v", i, err)
		}
		if len(nonce) != ccmNonceSize {
			t.Fatalf("nonce %d: len = %d, want %d", i, len(nonce), ccmNonceSize)
		}
		key := string(nonce)
		if seen[key] {
			t.Fatalf("nonce %d repeats a previously issued nonce — AEAD nonce reuse", i)
		}
		seen[key] = true
	}
}

// TestNextNonce_ConcurrentFirstCall pins the eager nonce-state initialization:
// concurrent first calls from a freshly derived state must never panic or
// repeat a nonce (the lazy nil check raced two initializers onto separate
// counters under the same session key).
func TestNextNonce_ConcurrentFirstCall(t *testing.T) {
	t.Parallel()

	cs := &SessionCryptoState{nonce: &nonceState{}}
	const nonceSize = 11
	const goroutines = 16
	const perG = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[string]bool)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, perG)
			for i := 0; i < perG; i++ {
				nonce, err := cs.NextNonce(nonceSize)
				if err != nil {
					t.Errorf("NextNonce: %v", err)
					return
				}
				local = append(local, string(nonce))
			}
			mu.Lock()
			defer mu.Unlock()
			for _, n := range local {
				if seen[n] {
					t.Error("concurrent NextNonce repeated a nonce — AEAD nonce reuse")
					return
				}
				seen[n] = true
			}
		}()
	}
	wg.Wait()
}
