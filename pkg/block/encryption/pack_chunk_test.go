package encryption

import (
	"bytes"
	"context"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestEncryptedRemote_GetPackChunk de-risks PR3b's per-chunk crypto boundary: a
// pack concatenates each chunk's self-framed encryption blob verbatim, so
// reading one chunk's slice out of the pack and decrypting it must yield the
// original plaintext — identical to decrypting that chunk's standalone object.
func TestEncryptedRemote_GetPackChunk(t *testing.T) {
	ctx := context.Background()
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	// Encrypt the target chunk via the decorator's Put so inner holds its real
	// wire blob (header||nonce||ciphertext||tag), then read that blob back out.
	target := bytes.Repeat([]byte{0x5A}, 8192)
	hash := block.ContentHash(blake3.Sum256(target))
	if err := d.Put(ctx, hash, target); err != nil {
		t.Fatalf("Put: %v", err)
	}
	wire, err := inner.Get(ctx, hash)
	if err != nil {
		t.Fatalf("inner Get wire: %v", err)
	}

	// Build a pack: [filler][target wire blob] and stage it in the base store.
	filler := bytes.Repeat([]byte{0x01}, 37)
	pack := append(append([]byte{}, filler...), wire...)
	const packID = "enc-pack-1"
	if err := inner.PutPack(packID, pack); err != nil {
		t.Fatalf("PutPack: %v", err)
	}

	got, err := d.GetPackChunk(ctx, packID, int64(len(filler)), int64(len(wire)), hash)
	if err != nil {
		t.Fatalf("GetPackChunk: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("decrypted pack chunk != plaintext (got %d bytes)", len(got))
	}
	if block.ContentHash(blake3.Sum256(got)) != hash {
		t.Fatalf("decrypted pack chunk hash mismatch")
	}

	// Wrong hash → AEAD AAD mismatch → decrypt fails (no silent corruption).
	wrong := block.ContentHash(blake3.Sum256([]byte("different")))
	if _, err := d.GetPackChunk(ctx, packID, int64(len(filler)), int64(len(wire)), wrong); err == nil {
		t.Fatalf("GetPackChunk with wrong AAD hash: want error, got nil")
	}
}
