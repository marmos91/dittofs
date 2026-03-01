package smb

import (
	"crypto/sha512"
	"sync"
	"testing"
)

func TestNewConnectionCryptoState(t *testing.T) {
	cs := NewConnectionCryptoState()
	if cs == nil {
		t.Fatal("NewConnectionCryptoState returned nil")
	}

	// H(0) should be all zeros
	hash := cs.GetPreauthHash()
	for i, b := range hash {
		if b != 0 {
			t.Errorf("H(0) byte %d: expected 0, got 0x%02X", i, b)
			break
		}
	}
}

func TestCryptoStateUpdatePreauthHash(t *testing.T) {
	cs := NewConnectionCryptoState()

	message := []byte("test message for preauth hash")
	cs.UpdatePreauthHash(message)

	hash := cs.GetPreauthHash()

	// Verify H(1) = SHA-512(H(0) || message)
	// H(0) = 64 zero bytes
	h := sha512.New()
	h.Write(make([]byte, 64)) // H(0) = zeros
	h.Write(message)
	expected := h.Sum(nil)

	for i := range hash {
		if hash[i] != expected[i] {
			t.Errorf("H(1) byte %d: expected 0x%02X, got 0x%02X", i, expected[i], hash[i])
			break
		}
	}
}

func TestCryptoStateHashChain(t *testing.T) {
	cs := NewConnectionCryptoState()

	msg1 := []byte("negotiate request")
	msg2 := []byte("negotiate response")

	cs.UpdatePreauthHash(msg1)
	cs.UpdatePreauthHash(msg2)

	hash := cs.GetPreauthHash()

	// Manually compute the chain:
	// H(1) = SHA-512(H(0) || msg1)
	h := sha512.New()
	h.Write(make([]byte, 64)) // H(0)
	h.Write(msg1)
	h1 := h.Sum(nil)

	// H(2) = SHA-512(H(1) || msg2)
	h.Reset()
	h.Write(h1)
	h.Write(msg2)
	expected := h.Sum(nil)

	for i := range hash {
		if hash[i] != expected[i] {
			t.Errorf("H(2) byte %d: expected 0x%02X, got 0x%02X", i, expected[i], hash[i])
			break
		}
	}
}

func TestCryptoStateGetPreauthHashRetursCopy(t *testing.T) {
	cs := NewConnectionCryptoState()
	cs.UpdatePreauthHash([]byte("test"))

	hash1 := cs.GetPreauthHash()
	hash2 := cs.GetPreauthHash()

	// Modify hash1, should not affect hash2 or internal state
	hash1[0] = 0xFF

	hash3 := cs.GetPreauthHash()
	if hash2[0] != hash3[0] {
		t.Error("GetPreauthHash did not return a copy")
	}
}

func TestCryptoStateConcurrentAccess(t *testing.T) {
	cs := NewConnectionCryptoState()

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent writes
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			msg := []byte{byte(n)}
			cs.UpdatePreauthHash(msg)
		}(i)
	}

	// Concurrent reads
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cs.GetPreauthHash()
		}()
	}

	wg.Wait()

	// Just verify we didn't panic or deadlock
	hash := cs.GetPreauthHash()
	allZero := true
	for _, b := range hash {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("hash should not be all zeros after updates")
	}
}

func TestCryptoStateZeroDialect(t *testing.T) {
	cs := NewConnectionCryptoState()
	if cs.Dialect != 0 {
		t.Errorf("expected Dialect 0, got 0x%04X", cs.Dialect)
	}
	if cs.CipherId != 0 {
		t.Errorf("expected CipherId 0, got 0x%04X", cs.CipherId)
	}
	if cs.PreauthIntegrityHashId != 0 {
		t.Errorf("expected PreauthIntegrityHashId 0, got 0x%04X", cs.PreauthIntegrityHashId)
	}
}
