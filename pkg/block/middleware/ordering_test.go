package middleware_test

import (
	"bytes"
	"context"
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

// TestPipeline_RoundTripsInReverse pins that Open inverts Seal in reverse stage
// order — the property that makes a swap silent rather than loud, and therefore
// the reason the size assertion above has to exist.
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

	got := wire
	for i, s := range []middleware.Transform{e, c} {
		out, err := s.Open(ctx, hash, got)
		if err != nil {
			t.Fatalf("Open stage %d: %v", i, err)
		}
		got = out
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip lost bytes: got %d, want %d", len(got), len(body))
	}
}
