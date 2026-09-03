package encryption

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// Migration-only tests for the legacy standalone-CAS read path (#1493 PR4):
// the encryption decorator's ReadBlockVerified. Delete with the migration.

func TestReadBlockVerified_RoundTrip(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("verify-me-please. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	out, err := d.ReadBlockVerified(context.Background(), h, h)
	if err != nil {
		t.Fatalf("ReadBlockVerified: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatal("plaintext mismatch")
	}
}

func TestReadBlockVerified_HashMismatch(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("trip-me-please")
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	bogus := h
	bogus[0] ^= 0xFF
	_, err = d.ReadBlockVerified(context.Background(), h, bogus)
	if !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("want ErrChunkContentMismatch, got %v", err)
	}
}
