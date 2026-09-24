package middleware_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/middleware"
	"github.com/marmos91/dittofs/pkg/block/middleware/compression"
	"github.com/marmos91/dittofs/pkg/block/middleware/encryption"
	"github.com/marmos91/dittofs/pkg/block/middleware/encryption/keyprovider"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// compressibleBody is a highly redundant payload: any working compressor
// shrinks it a lot, and no compressor shrinks its AEAD ciphertext at all. The
// gap between those two is the whole signal this file tests.
func compressibleBody() []byte {
	return bytes.Repeat([]byte("the same sixteen"), 4096) // 64 KiB
}

// newProvider returns a live local key provider, built the way the encryption
// package's own tests build one.
func newProvider(t *testing.T) keyprovider.KeyProvider {
	t.Helper()
	const passphrase = "ordering-test-passphrase"
	raw, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "share.key")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	kp, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	return kp
}

func stages(t *testing.T) (*compression.Transform, *encryption.Transform) {
	t.Helper()
	c, err := compression.NewTransform(compression.CompressionPolicy{Algo: compression.AlgoZstd})
	if err != nil {
		t.Fatalf("compression.NewTransform: %v", err)
	}
	e, err := encryption.NewTransform(
		encryption.EncryptionPolicy{AEAD: encryption.AEADAES256GCM},
		newProvider(t),
	)
	if err != nil {
		t.Fatalf("encryption.NewTransform: %v", err)
	}
	return c, e
}

// sealedSize runs the stack over a compressible body and reports how many bytes
// reach the inner store.
func sealedSize(t *testing.T, st ...middleware.Transform) int {
	t.Helper()
	ctx := context.Background()
	body := compressibleBody()
	hash := block.ContentHash(blake3.Sum256(body))

	data := body
	for _, s := range st {
		out, err := s.Seal(ctx, hash, data)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		data = out
	}
	return len(data)
}

// TestPipeline_CompressBeforeEncrypt is the gate on the one ordering rule the
// pipeline cannot check for itself.
//
// Both arrangements produce a working store — they round-trip, they error on
// nothing, and no log line distinguishes them. The only observable difference
// is the byte count that reaches the remote, which is why this asserts on size
// rather than on an error: encrypt-then-compress silently pays full price for
// every block, forever, because AEAD output has near-maximum entropy and does
// not compress.
func TestPipeline_CompressBeforeEncrypt(t *testing.T) {
	plain := len(compressibleBody())

	c, e := stages(t)
	right := sealedSize(t, c, e)

	c2, e2 := stages(t)
	wrong := sealedSize(t, e2, c2)

	if right >= plain/2 {
		t.Errorf("compress-then-encrypt sealed %d bytes from %d: the fixture is no longer compressible, so this test cannot detect a swap", right, plain)
	}
	if wrong <= plain {
		t.Errorf("encrypt-then-compress sealed %d bytes from %d, want > %d: compressing ciphertext must not shrink it", wrong, plain, plain)
	}
	if right >= wrong {
		t.Errorf("compress-then-encrypt (%d bytes) is not smaller than encrypt-then-compress (%d): the stages are ordered wrongly", right, wrong)
	}
}

// TestPipeline_RoundTripsInReverse pins that Pipeline.ReadChunk inverts
// SealChunk by running the stages in reverse — the property that makes a swap
// silent rather than loud, and therefore the reason the size assertion above
// has to exist.
//
// The read side is driven through ReadChunk rather than by opening the stages
// by hand: a hand-rolled inverse asserts only that the two stages are each
// other's inverse, which stays true no matter which direction the pipeline
// walks them.
func TestPipeline_RoundTripsInReverse(t *testing.T) {
	ctx := context.Background()
	c, e := stages(t)

	inner := remotememory.New()
	t.Cleanup(func() { _ = inner.Close() })

	p, err := middleware.New(inner, c, e)
	if err != nil {
		t.Fatalf("middleware.New: %v", err)
	}

	body := compressibleBody()
	hash := block.ContentHash(blake3.Sum256(body))

	wire, err := p.SealChunk(ctx, hash, body)
	if err != nil {
		t.Fatalf("SealChunk: %v", err)
	}
	if bytes.Equal(wire, body) {
		t.Fatal("sealed bytes equal the plaintext: no stage ran")
	}

	// ReadChunk reads from the inner store, so the sealed bytes have to be
	// there first — a single-chunk block object is exactly those bytes.
	const blockID = "round-trip"
	if err := inner.PutBlock(ctx, blockID, bytes.NewReader(wire)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	got, err := p.ReadChunk(ctx, blockID, 0, int64(len(wire)), hash)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip lost bytes: got %d, want %d", len(got), len(body))
	}
}

// TestPipeline_RoundTripsPlaintextThatLooksLikeACompressionFrame drives the
// composed stacks the engine builds, compression alone and compression then
// encryption, over an incompressible chunk whose plaintext begins with a
// compression frame header, and asserts ReadChunk returns it byte-for-byte.
func TestPipeline_RoundTripsPlaintextThatLooksLikeACompressionFrame(t *testing.T) {
	tail := make([]byte, 64<<10)
	if _, err := rand.Read(tail); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 0, compression.FrameHeaderFixedSize+len(tail))
	body = append(body, compression.FrameMagic[:]...)
	body = append(body, byte(compression.AlgoZstd))
	body = append(body, tail...)
	hash := block.ContentHash(blake3.Sum256(body))

	cases := []struct {
		name   string
		stages func(t *testing.T) []middleware.Transform
	}{
		{"compression", func(t *testing.T) []middleware.Transform {
			c, _ := stages(t)
			return []middleware.Transform{c}
		}},
		{"compression+encryption", func(t *testing.T) []middleware.Transform {
			c, e := stages(t)
			return []middleware.Transform{c, e}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			inner := remotememory.New()
			t.Cleanup(func() { _ = inner.Close() })
			p, err := middleware.New(inner, tc.stages(t)...)
			if err != nil {
				t.Fatalf("middleware.New: %v", err)
			}

			wire, err := p.SealChunk(ctx, hash, body)
			if err != nil {
				t.Fatalf("SealChunk: %v", err)
			}
			const blockID = "magic-prefixed"
			if err := inner.PutBlock(ctx, blockID, bytes.NewReader(wire)); err != nil {
				t.Fatalf("PutBlock: %v", err)
			}
			got, err := p.ReadChunk(ctx, blockID, 0, int64(len(wire)), hash)
			if err != nil {
				t.Fatalf("ReadChunk of a sealed chunk failed: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("ReadChunk returned %d bytes that differ from the %d sealed", len(got), len(body))
			}
		})
	}
}
