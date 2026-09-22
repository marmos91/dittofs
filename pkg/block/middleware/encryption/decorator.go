// Package encryption — the encryption stage of a block middleware pipeline.
// See README.md.
package encryption

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/middleware"
	"github.com/marmos91/dittofs/pkg/block/middleware/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// Transform is the encryption stage of a middleware pipeline. It AEAD-seals a
// chunk body on the way out and authenticated-decrypts it on the way back in,
// binding the plaintext content hash as additional authenticated data.
//
// The plaintext BLAKE3 remains the CAS key, so dedup, GC and verification are
// unchanged above this stage. Because the hash is bound as AAD, a block swapped
// at the inner store fails authentication on read.
type Transform struct {
	aead     AEAD
	provider keyprovider.KeyProvider
}

// NewTransform builds the encryption stage. policy.AEAD must be a recognised
// algorithm; provider must be non-nil and already initialised.
func NewTransform(policy EncryptionPolicy, provider keyprovider.KeyProvider) (*Transform, error) {
	if provider == nil {
		return nil, fmt.Errorf("encryption: keyprovider is nil")
	}
	if _, err := newAEAD(policy.AEAD, make([]byte, 32)); err != nil {
		return nil, err
	}
	return &Transform{aead: policy.AEAD, provider: provider}, nil
}

// NewRemote wraps inner in a pipeline whose only stage is encryption.
func NewRemote(inner remote.RemoteStore, policy EncryptionPolicy, provider keyprovider.KeyProvider) (*middleware.Pipeline, error) {
	if inner == nil {
		return nil, fmt.Errorf("encryption: inner RemoteStore is nil")
	}
	t, err := NewTransform(policy, provider)
	if err != nil {
		return nil, err
	}
	return middleware.New(inner, t)
}

// Close releases the key provider. The pipeline calls it because Transform
// implements io.Closer.
func (d *Transform) Close() error { return d.provider.Close() }

// Seal generates a fresh per-chunk block key and nonce, AEAD-seals data with
// hash as AAD, wraps the block key with the provider's master key, and returns
// the encoded frame.
func (d *Transform) Seal(ctx context.Context, hash block.ContentHash, data []byte) ([]byte, error) {
	blockKey := make([]byte, 32)
	if _, err := rand.Read(blockKey); err != nil {
		return nil, fmt.Errorf("encryption: read block key: %w", err)
	}
	aead, err := newAEAD(d.aead, blockKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("encryption: read nonce: %w", err)
	}

	wrappedKey, masterKeyID, err := d.provider.Wrap(ctx, blockKey)
	if err != nil {
		return nil, fmt.Errorf("encryption: wrap block key: %w", err)
	}
	// Seal straight onto the header: Seal appends the ciphertext into the space
	// the header reserved, so the wire frame costs one buffer instead of a
	// standalone ciphertext plus a copy of it.
	wire, err := appendFrameHeader(nil, d.aead, masterKeyID, wrappedKey, nonce, len(data)+aead.Overhead())
	if err != nil {
		return nil, err
	}
	return aead.Seal(wire, nonce, data, hash[:]), nil
}

// Open parses the frame (header||nonce||ciphertext||tag), unwraps the block key,
// and authenticated-decrypts the ciphertext against hash as AAD. An unframed
// body is rejected: on an encryption-enabled share one means external mutation
// or a stale policy. No hash verification happens here — this stage never sees
// the plaintext hash domain, so the engine hashes the recovered plaintext.
func (d *Transform) Open(ctx context.Context, hash block.ContentHash, raw []byte) ([]byte, error) {
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
	_ middleware.Transform = (*Transform)(nil)
	_ io.Closer            = (*Transform)(nil)
)
