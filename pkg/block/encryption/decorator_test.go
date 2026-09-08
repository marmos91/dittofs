package encryption

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

const testPassphrase = "correct horse battery staple"

// newProvider stages a passphrase-protected key file in a tempdir and
// returns a live local provider. Tests use it to obtain a real provider
// without coupling to dfsctl.
func newProvider(t *testing.T) keyprovider.KeyProvider {
	t.Helper()
	raw, err := keyprovider.GenerateKeyFile(testPassphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "share.key")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", testPassphrase)
	p, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func factoryFor(aead AEAD) blockstoretest.RemoteBlockStoreFactory {
	return func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
		t.Helper()
		d, err := NewRemote(remotememory.New(), EncryptionPolicy{AEAD: aead}, newProvider(t))
		if err != nil {
			t.Fatalf("NewRemote: %v", err)
		}
		// Conformance factory contract: cleanup must close the store.
		// d.Close() releases inner + provider; the provider's t.Cleanup in
		// newProvider is a safe no-op second close (aesGCMKEK.Close is
		// idempotent).
		return d, func() { _ = d.Close() }
	}
}

func TestConformance_AES256GCM(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, factoryFor(AEADAES256GCM))
}

func TestConformance_ChaCha20Poly1305(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, factoryFor(AEADChaCha20Poly1305))
}

func TestConformance_XChaCha20Poly1305(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, factoryFor(AEADXChaCha20Poly1305))
}

// --- ciphertext-vs-plaintext separation ---------------------------------

func hashOf(payload []byte) block.ContentHash {
	sum := blake3.Sum256(payload)
	var h block.ContentHash
	copy(h[:], sum[:])
	return h
}

// sealInto seals payload and stages the sealed bytes as a one-chunk block,
// returning the wire bytes. The base store's SealChunk is the identity
// transform, so the wire bytes are exactly what this decorator emitted.
func sealInto(t *testing.T, d *EncryptedRemote, blockID string, payload []byte) []byte {
	t.Helper()
	ctx := context.Background()
	wire, err := d.SealChunk(ctx, hashOf(payload), payload)
	if err != nil {
		t.Fatalf("SealChunk: %v", err)
	}
	if err := d.PutBlock(ctx, blockID, bytes.NewReader(wire)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	return wire
}

func TestSealChunk_EmitsCiphertextNotPlaintext(t *testing.T) {
	d, err := NewRemote(remotememory.New(), EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("recognisable-plaintext-marker. "), 256)
	wire, err := d.SealChunk(context.Background(), hashOf(payload), payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("recognisable-plaintext-marker")) {
		t.Fatal("sealed bytes contain plaintext marker — ciphertext leak")
	}
	if !bytes.HasPrefix(wire, FrameMagic[:]) {
		t.Fatal("sealed bytes do not begin with DFENC frame magic")
	}
}

// --- AAD enforcement ----------------------------------------------------

func TestReadChunk_TamperFailsAuth(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("auth-me-please")
	const blockID = "tampered"
	wire := sealInto(t, d, blockID, payload)

	// Flip the last byte of the stored ciphertext and verify the read fails
	// authentication rather than returning corrupt plaintext.
	tampered := append([]byte(nil), wire...)
	tampered[len(tampered)-1] ^= 0xFF
	if err := inner.PutBlock(context.Background(), blockID, bytes.NewReader(tampered)); err != nil {
		t.Fatal(err)
	}
	_, err = d.ReadChunk(context.Background(), blockID, 0, int64(len(tampered)), hashOf(payload))
	if !errors.Is(err, ErrDecryptAuth) {
		t.Fatalf("want ErrDecryptAuth, got %v", err)
	}
}

// --- unframed-block rejection -------------------------------------------

func TestReadChunk_UnframedBlockRejected(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	// Stash an unsealed chunk directly in the inner store (simulating a
	// pre-encryption block or external tampering). The read must refuse it.
	plain := []byte("not encrypted by us")
	const blockID = "unframed"
	if err := inner.PutBlock(context.Background(), blockID, bytes.NewReader(plain)); err != nil {
		t.Fatal(err)
	}
	_, err = d.ReadChunk(context.Background(), blockID, 0, int64(len(plain)), hashOf(plain))
	if !errors.Is(err, ErrCiphertextWithoutFrame) {
		t.Fatalf("want ErrCiphertextWithoutFrame, got %v", err)
	}
}

// TestSealChunk_ConcurrentNonceUniqueness fires N concurrent seals (distinct
// payloads, distinct hashes) and asserts that every emitted frame carries a
// unique nonce. crypto/rand is safe for concurrent use, but this pins the
// contract — a nonce collision under load would silently weaken AES-GCM
// authentication for the colliding pair.
func TestSealChunk_ConcurrentNonceUniqueness(t *testing.T) {
	const writers = 256
	d, err := NewRemote(remotememory.New(), EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(writers)
	errCh := make(chan error, writers)
	wires := make([][]byte, writers)
	for i := range writers {
		go func(idx int) {
			defer wg.Done()
			payload := fmt.Appendf(nil, "concurrent-payload-%04d-%s", idx, bytes.Repeat([]byte{'x'}, 256))
			wire, err := d.SealChunk(context.Background(), hashOf(payload), payload)
			if err != nil {
				errCh <- err
				return
			}
			wires[idx] = wire
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent SealChunk: %v", err)
	}

	nonceCount := make(map[string]int, writers)
	for _, wire := range wires {
		view, framed, err := tryDecodeFrame(wire)
		if !framed || err != nil {
			t.Fatalf("tryDecodeFrame: framed=%v err=%v", framed, err)
		}
		nonceCount[string(view.nonce)]++
	}
	for nonce, count := range nonceCount {
		if count > 1 {
			t.Fatalf("nonce %x collided %d times across concurrent seals", []byte(nonce), count)
		}
	}
	if len(nonceCount) != writers {
		t.Fatalf("expected %d unique nonces, got %d", writers, len(nonceCount))
	}
}

func TestNewRemote_RejectsNilInputs(t *testing.T) {
	_, err := NewRemote(nil, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err == nil {
		t.Fatal("want error for nil inner")
	}
	_, err = NewRemote(remotememory.New(), EncryptionPolicy{AEAD: AEADAES256GCM}, nil)
	if err == nil {
		t.Fatal("want error for nil provider")
	}
	_, err = NewRemote(remotememory.New(), EncryptionPolicy{AEAD: 0xFF}, newProvider(t))
	if err == nil {
		t.Fatal("want error for unknown AEAD")
	}
}
