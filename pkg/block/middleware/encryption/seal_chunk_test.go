package encryption

import (
	"bytes"
	"context"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestSealChunk_SealedAndReadChunkInverse proves the encryption decorator's
// SealChunk produces ciphertext (never plaintext at rest) and is byte-symmetric
// with ReadChunk: sealing a chunk, framing it as a one-chunk block body,
// PutBlock, then ReadChunk over its [offset,len) returns the original plaintext
// and the right BLAKE3.
func TestSealChunk_SealedAndReadChunkInverse(t *testing.T) {
	ctx := context.Background()
	inner := remotememory.New()
	d, err := NewRemote(inner, EncryptionPolicy{AEAD: AEADAES256GCM}, newProvider(t))
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	var sealer remote.ChunkSealer = d

	plaintext := bytes.Repeat([]byte{0x5A}, 8192)
	hash := block.ContentHash(blake3.Sum256(plaintext))

	wire, err := sealer.SealChunk(ctx, hash, plaintext)
	if err != nil {
		t.Fatalf("SealChunk: %v", err)
	}
	// The sealed wire must not contain the plaintext run verbatim — it is a
	// framed AEAD ciphertext, larger than the plaintext (header + tag).
	if len(wire) <= len(plaintext) {
		t.Fatalf("sealed chunk should be larger than plaintext (frame+tag): wire=%d plaintext=%d", len(wire), len(plaintext))
	}
	if bytes.Contains(wire, plaintext) {
		t.Fatal("sealed wire must not contain plaintext verbatim")
	}

	if err := d.PutBlock(ctx, "enc-seal-1", bytes.NewReader(wire)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	got, err := d.ReadChunk(ctx, "enc-seal-1", 0, int64(len(wire)), hash)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch (got %d bytes)", len(got))
	}
	if block.ContentHash(blake3.Sum256(got)) != hash {
		t.Fatal("round-trip BLAKE3 mismatch")
	}
}
