package memory

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// TestStore_SealChunk_Identity pins the base in-memory store's SealChunk as the
// identity transform: the wire bytes equal the plaintext, and a PutBlock of
// those wire bytes round-trips through ReadChunk byte-identical. The base store
// is the innermost layer of the carve transform stack, so any non-identity here
// would corrupt every block.
func TestStore_SealChunk_Identity(t *testing.T) {
	ctx := context.Background()
	s := New()

	var sealer remote.ChunkSealer = s // compile-time: base store implements ChunkSealer

	var hash block.ContentHash
	hash[0] = 0x11
	plaintext := []byte("the quick brown fox jumps over the lazy dog")

	wire, err := sealer.SealChunk(ctx, hash, plaintext)
	if err != nil {
		t.Fatalf("SealChunk: %v", err)
	}
	if !bytes.Equal(wire, plaintext) {
		t.Fatalf("base SealChunk must be identity: got %q want %q", wire, plaintext)
	}

	// The wire bytes are what a single-chunk block body would contain. Put them
	// and read them back via ReadChunk; the base read must invert the base seal.
	if err := s.PutBlock(ctx, "blk-id", bytes.NewReader(wire)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	got, err := s.ReadChunk(ctx, "blk-id", 0, int64(len(wire)), hash)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plaintext)
	}
}
