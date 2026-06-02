package encryption

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// EncryptedRemote wraps a remote.RemoteStore and transparently encrypts
// block bodies on Put while decrypting on Get. The plaintext BLAKE3
// remains the CAS key — dedup, GC, and verification semantics are
// unchanged from the perspective of callers above the decorator.
type EncryptedRemote struct {
	inner    remote.RemoteStore
	aead     AEAD
	provider keyprovider.KeyProvider
}

// NewRemote wraps inner with the encryption decorator. policy.AEAD must
// be a recognised algorithm; provider must be non-nil and already
// initialised.
func NewRemote(inner remote.RemoteStore, policy EncryptionPolicy, provider keyprovider.KeyProvider) (*EncryptedRemote, error) {
	if inner == nil {
		return nil, fmt.Errorf("encryption: inner RemoteStore is nil")
	}
	if provider == nil {
		return nil, fmt.Errorf("encryption: keyprovider is nil")
	}
	if _, err := newAEAD(policy.AEAD, make([]byte, 32)); err != nil {
		return nil, err
	}
	return &EncryptedRemote{inner: inner, aead: policy.AEAD, provider: provider}, nil
}

// AEAD returns the algorithm this decorator emits on Put. Reads accept
// any algorithm encoded in the wire frame regardless.
func (d *EncryptedRemote) AEAD() AEAD { return d.aead }

// Put encrypts data and stores the framed result under hash. The block
// key is fresh per call; the plaintext hash is bound into the AEAD's
// additional data so a swapped block fails authentication on Get.
func (d *EncryptedRemote) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	blockKey := make([]byte, 32)
	if _, err := rand.Read(blockKey); err != nil {
		return fmt.Errorf("encryption: read block key: %w", err)
	}
	aead, err := newAEAD(d.aead, blockKey)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("encryption: read nonce: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, data, hash[:])

	wrappedKey, masterKeyID, err := d.provider.Wrap(ctx, blockKey)
	if err != nil {
		return fmt.Errorf("encryption: wrap block key: %w", err)
	}
	wire, err := encodeFrame(d.aead, masterKeyID, wrappedKey, nonce, ciphertext)
	if err != nil {
		return err
	}
	return d.inner.Put(ctx, hash, wire)
}

// Get returns the plaintext for the block identified by hash.
func (d *EncryptedRemote) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	raw, err := d.inner.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	return d.decrypt(ctx, hash, raw)
}

// GetRange returns a byte sub-range of the plaintext. For encrypted
// blocks this materialises the full plaintext and slices — there is no
// random access into ciphertext.
func (d *EncryptedRemote) GetRange(ctx context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, fmt.Errorf("%w: length %d", block.ErrInvalidSize, length)
	}
	full, err := d.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset > int64(len(full)) {
		return nil, fmt.Errorf("%w: offset %d out of bounds (size %d)", block.ErrInvalidOffset, offset, len(full))
	}
	end := min(offset+length, int64(len(full)))
	out := make([]byte, end-offset)
	copy(out, full[offset:end])
	return out, nil
}

// Has reports presence by probing inner.Head. NotFound errors map to
// (false, nil); any other backend error propagates.
func (d *EncryptedRemote) Has(ctx context.Context, hash block.ContentHash) (bool, error) {
	_, err := d.inner.Head(ctx, hash)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, block.ErrChunkNotFound) {
		return false, nil
	}
	return false, err
}

// Head returns Meta whose Size is the plaintext byte length, derived
// from the wire size via a short range-GET that parses the frame
// header — no full decrypt. AEAD output is plaintext-length plus a
// 16-byte authentication tag, so plaintext_size = wire_size -
// header_size - aeadTagSize.
func (d *EncryptedRemote) Head(ctx context.Context, hash block.ContentHash) (block.Meta, error) {
	m, err := d.inner.Head(ctx, hash)
	if err != nil {
		return m, err
	}
	size, err := d.plaintextSizeFor(ctx, hash, m.Size)
	if err != nil {
		return block.Meta{}, err
	}
	m.Size = size
	return m, nil
}

// Walk rewrites Meta.Size to plaintext size for each block via the same
// range-GET probe as Head. Per-block probe errors halt the walk.
func (d *EncryptedRemote) Walk(ctx context.Context, fn func(hash block.ContentHash, meta block.Meta) error) error {
	return d.inner.Walk(ctx, func(h block.ContentHash, m block.Meta) error {
		size, err := d.plaintextSizeFor(ctx, h, m.Size)
		if err != nil {
			return err
		}
		m.Size = size
		return fn(h, m)
	})
}

// plaintextSizeFor returns the plaintext byte length of the block.
// Reads at most maxFrameHeaderSize bytes off the wire to parse the
// header, then derives plaintext size from the total wire size. Returns
// ErrCiphertextWithoutFrame for an unframed inner block.
func (d *EncryptedRemote) plaintextSizeFor(ctx context.Context, hash block.ContentHash, wireSize int64) (int64, error) {
	probeLen := min(int64(maxFrameHeaderSize), wireSize)
	if probeLen <= 0 {
		return 0, ErrCiphertextWithoutFrame
	}
	probe, err := d.inner.GetRange(ctx, hash, 0, probeLen)
	if err != nil {
		return 0, fmt.Errorf("encryption: plaintext-size probe: %w", err)
	}
	headerLen, framed, err := frameHeaderSize(probe)
	if !framed {
		return 0, ErrCiphertextWithoutFrame
	}
	if err != nil {
		return 0, err
	}
	plain := wireSize - int64(headerLen) - aeadTagSize
	if plain < 0 {
		return 0, fmt.Errorf("%w: wire size %d smaller than header %d + tag %d", ErrEncryptedFrameCorrupt, wireSize, headerLen, aeadTagSize)
	}
	return plain, nil
}

// Delete is a straight passthrough.
func (d *EncryptedRemote) Delete(ctx context.Context, hash block.ContentHash) error {
	return d.inner.Delete(ctx, hash)
}

// ReadBlockVerified GETs the block, decrypts it, then re-verifies the
// BLAKE3 hash over the plaintext.
func (d *EncryptedRemote) ReadBlockVerified(ctx context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error) {
	plain, err := d.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	actual := blake3.Sum256(plain)
	var got block.ContentHash
	copy(got[:], actual[:])
	if got != expected {
		return nil, fmt.Errorf("%w: got %s want %s", block.ErrCASContentMismatch, got, expected)
	}
	return plain, nil
}

// Close releases inner resources and the provider.
func (d *EncryptedRemote) Close() error {
	innerErr := d.inner.Close()
	provErr := d.provider.Close()
	if innerErr != nil {
		return innerErr
	}
	return provErr
}

func (d *EncryptedRemote) HealthCheck(ctx context.Context) error { return d.inner.HealthCheck(ctx) }

func (d *EncryptedRemote) Healthcheck(ctx context.Context) health.Report {
	return d.inner.Healthcheck(ctx)
}

// decrypt parses the frame, unwraps the block key, and authenticated-
// decrypts the ciphertext against hash as AAD. An unframed block on an
// encryption-enabled share is rejected — it indicates external mutation
// or a stale policy.
func (d *EncryptedRemote) decrypt(ctx context.Context, hash block.ContentHash, raw []byte) ([]byte, error) {
	view, framed, err := tryDecodeFrame(raw)
	if !framed {
		return nil, ErrCiphertextWithoutFrame
	}
	if err != nil {
		return nil, err
	}
	blockKey, err := d.provider.Unwrap(ctx, view.wrappedKey, view.masterKeyID)
	if err != nil {
		return nil, fmt.Errorf("encryption: unwrap block key: %w", err)
	}
	aead, err := newAEAD(view.aead, blockKey)
	if err != nil {
		return nil, err
	}
	if len(view.nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: nonce length %d does not match aead %s (want %d)", ErrEncryptedFrameCorrupt, len(view.nonce), view.aead, aead.NonceSize())
	}
	plain, err := aead.Open(nil, view.nonce, view.ciphertext, hash[:])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptAuth, err)
	}
	return plain, nil
}

// newAEAD constructs the cipher.AEAD for the given algorithm and key.
// Key length must be 32 bytes (AES-256 + ChaCha20-Poly1305 both expect
// 256-bit keys).
func newAEAD(algo AEAD, key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption: key length %d, want 32", len(key))
	}
	switch algo {
	case AEADAES256GCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("encryption: aes.NewCipher: %w", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("encryption: cipher.NewGCM: %w", err)
		}
		return aead, nil
	case AEADChaCha20Poly1305:
		aead, err := chacha20poly1305.New(key)
		if err != nil {
			return nil, fmt.Errorf("encryption: chacha20poly1305.New: %w", err)
		}
		return aead, nil
	case AEADXChaCha20Poly1305:
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, fmt.Errorf("encryption: chacha20poly1305.NewX: %w", err)
		}
		return aead, nil
	default:
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedAEAD, algo)
	}
}

// Compile-time interface assertions.
var (
	_ block.Store        = (*EncryptedRemote)(nil)
	_ remote.RemoteStore = (*EncryptedRemote)(nil)
)
