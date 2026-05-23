package encryption

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/blockstoretest"
	"github.com/marmos91/dittofs/pkg/blockstore/encryption/keyprovider"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
)

// countingInner counts Get calls so tests can assert that perf-sensitive
// paths (Head, Walk) do not trigger a full-payload fetch.
type countingInner struct {
	*remotememory.Store
	gets atomic.Int64
}

func (c *countingInner) Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, hash)
}

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

func factoryFor(aead AEAD) blockstoretest.Factory {
	return func(t *testing.T) (blockstore.BlockStore, func()) {
		t.Helper()
		inner := remotememory.New()
		// Note: provider lifetime is tied to the test via t.Cleanup
		// inside newProvider, so we do NOT close it via Decorator.Close
		// to avoid double-Close on the local provider.
		prov := newProvider(t)
		d, err := NewRemote(inner, EncryptionPolicy{AEAD: aead}, prov)
		if err != nil {
			t.Fatalf("NewRemote: %v", err)
		}
		// Cleanup closes the inner store only; provider close runs via
		// the t.Cleanup from newProvider above.
		return d, func() { _ = inner.Close() }
	}
}

func TestConformance_AES256GCM(t *testing.T) {
	blockstoretest.BlockStoreConformance(t, factoryFor(AEADAES256GCM))
}

func TestConformance_ChaCha20Poly1305(t *testing.T) {
	blockstoretest.BlockStoreConformance(t, factoryFor(AEADChaCha20Poly1305))
}

func TestConformance_XChaCha20Poly1305(t *testing.T) {
	blockstoretest.BlockStoreConformance(t, factoryFor(AEADXChaCha20Poly1305))
}

// --- ciphertext-vs-plaintext separation ---------------------------------

func hashOf(payload []byte) blockstore.ContentHash {
	sum := blake3.Sum256(payload)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

func TestPut_StoresCiphertextNotPlaintext(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("recognisable-plaintext-marker. "), 256)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	stored, err := inner.Get(context.Background(), h)
	if err != nil {
		t.Fatalf("inner Get: %v", err)
	}
	if bytes.Contains(stored, []byte("recognisable-plaintext-marker")) {
		t.Fatal("stored bytes contain plaintext marker — ciphertext leak")
	}
	if !bytes.HasPrefix(stored, FrameMagic[:]) {
		t.Fatal("stored bytes do not begin with DFENC frame magic")
	}
}

// --- AAD enforcement ----------------------------------------------------

func TestGet_TamperFailsAuth(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("auth-me-please")
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	// Read the stored frame, flip the last byte of the ciphertext, write
	// it back, and verify Get fails with ErrDecryptAuth.
	stored, err := inner.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	stored[len(stored)-1] ^= 0xFF
	if err := inner.Put(context.Background(), h, stored); err != nil {
		t.Fatal(err)
	}
	_, err = d.Get(context.Background(), h)
	if !errors.Is(err, ErrDecryptAuth) {
		t.Fatalf("want ErrDecryptAuth, got %v", err)
	}
}

// --- ReadBlockVerified --------------------------------------------------

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
	if !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("want ErrCASContentMismatch, got %v", err)
	}
}

// --- Head plaintext-size contract ---------------------------------------

func TestHead_ReportsPlaintextSize(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("size-me. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	m, err := d.Head(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != int64(len(payload)) {
		t.Fatalf("Head.Size: got %d want %d (plaintext)", m.Size, len(payload))
	}
}

// --- GetRange invalid input --------------------------------------------

func TestGetRange_InvalidLength(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("range. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	for _, length := range []int64{0, -1, -1024} {
		_, err := d.GetRange(context.Background(), h, 0, length)
		if !errors.Is(err, blockstore.ErrInvalidSize) {
			t.Errorf("length=%d: got %v, want wraps ErrInvalidSize", length, err)
		}
	}
}

// --- unframed-block rejection -------------------------------------------

func TestGet_UnframedBlockRejected(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	// Stash a plaintext block directly into the inner store (simulating a
	// pre-encryption block or external tampering). Get must refuse it.
	plain := []byte("not encrypted by us")
	h := hashOf(plain)
	if err := inner.Put(context.Background(), h, plain); err != nil {
		t.Fatal(err)
	}
	_, err = d.Get(context.Background(), h)
	if !errors.Is(err, ErrCiphertextWithoutFrame) {
		t.Fatalf("want ErrCiphertextWithoutFrame, got %v", err)
	}
}

// --- nil-input rejection ------------------------------------------------

// TestPut_ConcurrentNonceUniqueness fires N concurrent Puts (distinct
// payloads, distinct hashes) and asserts that every emitted frame
// carries a unique nonce. crypto/rand is safe for concurrent use, but
// this pins the contract — a nonce collision under load would silently
// weaken AES-GCM authentication for the colliding pair.
func TestPut_ConcurrentNonceUniqueness(t *testing.T) {
	const writers = 256
	inner := remotememory.New()
	prov := newProvider(t)
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, prov)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(writers)
	errCh := make(chan error, writers)
	hashes := make([]blockstore.ContentHash, writers)
	for i := range writers {
		go func(idx int) {
			defer wg.Done()
			payload := fmt.Appendf(nil, "concurrent-payload-%04d-%s", idx, bytes.Repeat([]byte{'x'}, 256))
			h := hashOf(payload)
			hashes[idx] = h
			if err := d.Put(context.Background(), h, payload); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent Put: %v", err)
	}

	nonceCount := make(map[string]int, writers)
	for _, h := range hashes {
		stored, err := inner.Get(context.Background(), h)
		if err != nil {
			t.Fatalf("read stored frame: %v", err)
		}
		view, framed, err := tryDecodeFrame(stored)
		if !framed || err != nil {
			t.Fatalf("tryDecodeFrame: framed=%v err=%v", framed, err)
		}
		nonceCount[string(view.nonce)]++
	}
	for nonce, count := range nonceCount {
		if count > 1 {
			t.Fatalf("nonce %x collided %d times across concurrent Puts", []byte(nonce), count)
		}
	}
	if len(nonceCount) != writers {
		t.Fatalf("expected %d unique nonces, got %d", writers, len(nonceCount))
	}
}

// TestHead_DoesNotDecryptFullPayload pins the perf-correctness contract:
// Head must NOT fetch and decrypt the full block to report Meta.Size.
// We instrument the inner store to count Get calls; Head should make
// zero Gets (it uses inner.Head + inner.GetRange for the header probe).
func TestHead_DoesNotDecryptFullPayload(t *testing.T) {
	inner := &countingInner{Store: remotememory.New()}
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("perf-probe. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	inner.gets.Store(0)
	m, err := d.Head(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != int64(len(payload)) {
		t.Fatalf("Head.Size: got %d want %d", m.Size, len(payload))
	}
	if got := inner.gets.Load(); got != 0 {
		t.Fatalf("Head triggered %d inner.Get calls (want 0 — should use range-GET probe)", got)
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
