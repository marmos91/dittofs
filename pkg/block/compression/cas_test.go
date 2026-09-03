package compression

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// Migration-only tests for the legacy standalone-CAS read path (#1493 PR4):
// the compression decorator's ReadBlockVerified. Delete with the migration.

func TestReadBlockVerified_RoundTrip(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("verify-me-please. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	out, err := d.ReadBlockVerified(context.Background(), h, h)
	if err != nil {
		t.Fatalf("ReadBlockVerified: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatal("plaintext mismatch")
	}
}

func TestReadBlockVerified_MismatchTrips(t *testing.T) {
	inner := remotememory.New()
	d, err := NewRemote(inner, CompressionPolicy{Algo: AlgoZstd})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("trip-me. "), 4096)
	h := hashOf(payload)
	if err := d.Put(context.Background(), h, payload); err != nil {
		t.Fatal(err)
	}
	bogus := h
	bogus[0] ^= 0xff
	_, err = d.ReadBlockVerified(context.Background(), h, bogus)
	if !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("err: got %v want wraps ErrChunkContentMismatch", err)
	}
}
