package session

import "testing"

// TestNextNonce_MonotonicAndNonRepeating pins the per-session encrypt-nonce
// counter: nonces issued from the same crypto state are unique (the counter
// increments per call) — MS-SMB2 3.1.4.3 forbids AEAD nonce reuse under the
// same key.
func TestNextNonce_MonotonicAndNonRepeating(t *testing.T) {
	t.Parallel()

	cs := &SessionCryptoState{}
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
