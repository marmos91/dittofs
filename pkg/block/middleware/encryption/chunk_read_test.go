package encryption

import (
	"bytes"
	"context"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestEncryptedRemote_ReadChunk de-risks PR3b's per-chunk crypto boundary: a
// block concatenates each chunk's self-framed encryption blob verbatim, so
// reading one chunk's slice out of the block and decrypting it must yield the
// original plaintext — identical to decrypting that chunk's standalone object.
func TestEncryptedRemote_ReadChunk(t *testing.T) {
	ctx := context.Background()
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	// SealChunk emits the chunk's real wire blob (header||nonce||ciphertext||tag)
	// — the same bytes a block concatenates verbatim.
	target := bytes.Repeat([]byte{0x5A}, 8192)
	hash := block.ContentHash(blake3.Sum256(target))
	wire, err := d.SealChunk(ctx, hash, target)
	if err != nil {
		t.Fatalf("SealChunk: %v", err)
	}

	// Build a block: [filler][target wire blob] and stage it in the base store.
	filler := bytes.Repeat([]byte{0x01}, 37)
	blockData := append(append([]byte{}, filler...), wire...)
	const blockID = "enc-block-1"
	if err := inner.PutBlock(ctx, blockID, bytes.NewReader(blockData)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	got, err := d.ReadChunk(ctx, blockID, int64(len(filler)), int64(len(wire)), hash)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("decrypted block chunk != plaintext (got %d bytes)", len(got))
	}
	if block.ContentHash(blake3.Sum256(got)) != hash {
		t.Fatalf("decrypted block chunk hash mismatch")
	}

	// Wrong hash → AEAD AAD mismatch → decrypt fails (no silent corruption).
	wrong := block.ContentHash(blake3.Sum256([]byte("different")))
	if _, err := d.ReadChunk(ctx, blockID, int64(len(filler)), int64(len(wire)), wrong); err == nil {
		t.Fatalf("ReadChunk with wrong AAD hash: want error, got nil")
	}
}
