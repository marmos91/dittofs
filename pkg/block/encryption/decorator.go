package encryption

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// EncryptedRemote wraps a remote.RemoteStore and transparently encrypts
// block bodies on Put while decrypting on Get. The plaintext BLAKE3
// remains the CAS key — dedup, GC, and verification semantics are
// unchanged from the perspective of callers above the decorator.
type EncryptedRemote struct {
	remote.Passthrough
	// inner is the wrapped store, held separately because Passthrough keeps
	// its own copy unexported to stay off this type's public surface.
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
	return &EncryptedRemote{
		Passthrough: remote.NewPassthrough(inner),
		inner:       inner,
		aead:        policy.AEAD,
		provider:    provider,
	}, nil
}

// SealChunk encrypts one chunk's plaintext into a frame and delegates inward so
// a decorated chain produces the fully-transformed wire bytes for a packed
// block. Implements remote.ChunkSealer (#1414). hash is bound as AEAD AAD,
// matching the standalone Put scheme. Symmetric with ReadChunk, which decrypts
// the ranged frame with the same AAD.
func (d *EncryptedRemote) SealChunk(ctx context.Context, hash block.ContentHash, plaintext []byte) ([]byte, error) {
	wire, err := d.sealLayer(ctx, hash, plaintext)
	if err != nil {
		return nil, err
	}
	sealer, ok := d.inner.(remote.ChunkSealer)
	if !ok {
		return nil, remote.ErrChunkReadUnsupported
	}
	return sealer.SealChunk(ctx, hash, wire)
}

// sealLayer is the single source of this decorator's encryption transform,
// shared by Put and SealChunk. It generates a fresh per-chunk block key + nonce,
// AEAD-seals data with hash as AAD, wraps the block key, and returns the encoded
// frame.
func (d *EncryptedRemote) sealLayer(ctx context.Context, hash block.ContentHash, data []byte) ([]byte, error) {
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

// ReadChunk reads the chunk's encrypted wire bytes from the inner store's
// block object and decrypts them against hash as the AEAD AAD, returning the
// plaintext (for the next layer up / the engine). A block stores each chunk's
// full self-framed encryption blob (header||nonce||ciphertext||tag) verbatim, so
// decrypting the chunk's [offset, length) slice is identical to decrypting its
// standalone object. No verification here — the engine verifies the BLAKE3 after
// the full stack. Implements remote.ChunkReader (#1414).
func (d *EncryptedRemote) ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error) {
	pcr, ok := d.inner.(remote.ChunkReader)
	if !ok {
		return nil, remote.ErrChunkReadUnsupported
	}
	raw, err := pcr.ReadChunk(ctx, blockID, offset, length, hash)
	if err != nil {
		return nil, err
	}
	return d.decrypt(ctx, hash, raw)
}

// Close releases inner resources and the provider.
func (d *EncryptedRemote) Close() error {
	innerErr := d.Passthrough.Close()
	provErr := d.provider.Close()
	if innerErr != nil {
		return innerErr
	}
	return provErr
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
	_ remote.RemoteStore       = (*EncryptedRemote)(nil)
	_ remote.RemoteBlockStore  = (*EncryptedRemote)(nil)
	_ remote.ChunkReader       = (*EncryptedRemote)(nil)
	_ remote.ChunkSealer       = (*EncryptedRemote)(nil)
	_ block.DurabilityReporter = (*EncryptedRemote)(nil)
)
