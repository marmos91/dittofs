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

// Put encrypts data and stores the framed result under hash. The block
// key is fresh per call; the plaintext hash is bound into the AEAD's
// additional data so a swapped block fails authentication on Get.
// Put encrypts data into a self-describing frame and stores it on the inner
// store. Put and SealChunk share the same single-layer transform (sealLayer)
// so the standalone-object and packed-block write paths never drift.
func (d *EncryptedRemote) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	wire, err := d.sealLayer(ctx, hash, data)
	if err != nil {
		return err
	}
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return err
	}
	return cs.Put(ctx, hash, wire)
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
	ciphertext := aead.Seal(nil, nonce, data, hash[:])

	wrappedKey, masterKeyID, err := d.provider.Wrap(ctx, blockKey)
	if err != nil {
		return nil, fmt.Errorf("encryption: wrap block key: %w", err)
	}
	wire, err := encodeFrame(d.aead, masterKeyID, wrappedKey, nonce, ciphertext)
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// Get returns the plaintext for the block identified by hash.
func (d *EncryptedRemote) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return nil, err
	}
	raw, err := cs.Get(ctx, hash)
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
	return remote.SliceRange(full, offset, length)
}

// Head returns Meta whose Size is the plaintext byte length, derived
// from the wire size via a short range-GET that parses the frame
// header — no full decrypt. AEAD output is plaintext-length plus a
// 16-byte authentication tag, so plaintext_size = wire_size -
// header_size - aeadTagSize.
func (d *EncryptedRemote) Head(ctx context.Context, hash block.ContentHash) (block.Meta, error) {
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return block.Meta{}, err
	}
	m, err := cs.Head(ctx, hash)
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
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return err
	}
	return cs.Walk(ctx, func(h block.ContentHash, m block.Meta) error {
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
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return 0, err
	}
	probe, err := cs.GetRange(ctx, hash, 0, probeLen)
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
	_ block.Store              = (*EncryptedRemote)(nil)
	_ remote.RemoteStore       = (*EncryptedRemote)(nil)
	_ remote.RemoteBlockStore  = (*EncryptedRemote)(nil)
	_ remote.ChunkReader       = (*EncryptedRemote)(nil)
	_ remote.ChunkSealer       = (*EncryptedRemote)(nil)
	_ block.DurabilityReporter = (*EncryptedRemote)(nil)
)
