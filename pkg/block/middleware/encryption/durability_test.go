package encryption

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestEncryptedRemote_Durable_DelegatesToInner verifies the encryption decorator
// delegates Durable() to the wrapped store (#1274): wrapping a durable backend
// keeps it durable; wrapping a non-durable one stays non-durable.
func TestEncryptedRemote_Durable_DelegatesToInner(t *testing.T) {
	inner := remotememory.New() // memory remote: NOT durable by default
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	var _ block.DurabilityReporter = d

	if d.Durable() {
		t.Fatal("encrypting a non-durable inner store must stay NOT durable")
	}

	inner.SetDurable(true) // simulate a durable remote (e.g. s3)
	if !d.Durable() {
		t.Fatal("encrypting a durable inner store must report durable (delegation)")
	}
}
