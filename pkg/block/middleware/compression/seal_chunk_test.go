package compression

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestSealChunk_RoundTripThroughBlock proves the compression decorator's
// SealChunk is byte-symmetric with ReadChunk: sealing a chunk, framing it as a
// one-chunk block body, PutBlock, then ReadChunk over its [offset,len) returns
// the original plaintext. A highly-compressible payload also asserts the wire
// bytes are smaller than the plaintext, confirming the layer actually ran.
func TestSealChunk_RoundTripThroughBlock(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	d, err := NewRemote(base, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	var sealer remote.ChunkSealer = d

	var hash block.ContentHash
	hash[0] = 0x22
	plaintext := []byte(strings.Repeat("compressible-payload-", 4096))

	wire, err := sealer.SealChunk(ctx, hash, plaintext)
	if err != nil {
		t.Fatalf("SealChunk: %v", err)
	}
	if len(wire) >= len(plaintext) {
		t.Fatalf("compressible payload should shrink: wire=%d plaintext=%d", len(wire), len(plaintext))
	}

	if err := d.PutBlock(ctx, "blk-c", bytes.NewReader(wire)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	got, err := d.ReadChunk(ctx, "blk-c", 0, int64(len(wire)), hash)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch (len got=%d want=%d)", len(got), len(plaintext))
	}
}
