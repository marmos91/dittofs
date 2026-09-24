package compression

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
)

// magicPrefixedIncompressible returns a chunk whose plaintext begins with the
// frame magic and a valid algorithm byte, followed by random bytes no codec can
// shrink, so Seal stores it without a frame and the stored body looks like one.
func magicPrefixedIncompressible(t *testing.T, embedded Algo) []byte {
	t.Helper()
	tail := make([]byte, 64<<10)
	if _, err := rand.Read(tail); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 0, FrameHeaderFixedSize+len(tail))
	p = append(p, FrameMagic[:]...)
	p = append(p, byte(embedded))
	return append(p, tail...)
}

// TestTransform_RoundTripsPlaintextThatLooksLikeAFrame pins that Open inverts
// Seal for every plaintext, including one whose leading bytes are a frame
// header: an incompressible chunk must come back exactly as written rather
// than be parsed as a frame it never was.
func TestTransform_RoundTripsPlaintextThatLooksLikeAFrame(t *testing.T) {
	ctx := context.Background()
	for _, sealAlgo := range []Algo{AlgoZstd, AlgoLZ4} {
		for _, embedded := range []Algo{AlgoZstd, AlgoLZ4} {
			t.Run(sealAlgo.String()+"/embedded_"+embedded.String(), func(t *testing.T) {
				tr, err := NewTransform(CompressionPolicy{Algo: sealAlgo})
				if err != nil {
					t.Fatalf("NewTransform: %v", err)
				}
				p := magicPrefixedIncompressible(t, embedded)
				h := hashOf(p)

				wire, err := tr.Seal(ctx, h, p)
				if err != nil {
					t.Fatalf("Seal: %v", err)
				}
				got, err := tr.Open(ctx, h, wire)
				if err != nil {
					t.Fatalf("Open(Seal(p)) failed: %v", err)
				}
				if !bytes.Equal(got, p) {
					t.Fatalf("Open(Seal(p)) returned %d bytes that differ from the %d written", len(got), len(p))
				}
			})
		}
	}
}
