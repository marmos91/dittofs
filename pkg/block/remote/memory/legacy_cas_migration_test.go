package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// Migration-only tests for the legacy standalone-CAS read path (#1493 PR4):
// the memory backend's ReadBlockVerified. Delete with the migration.

func TestStore_ReadBlockVerified(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("verified read")
	hash := hashOf(t, data)
	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.ReadBlockVerified(ctx, hash, hash)
	if err != nil {
		t.Fatalf("ReadBlockVerified happy path: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("ReadBlockVerified bytes mismatch: got %q, want %q", got, data)
	}

	// Mismatched expected => ErrChunkContentMismatch
	wrong := hash
	wrong[0] ^= 0xFF
	if _, err := s.ReadBlockVerified(ctx, hash, wrong); !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("ReadBlockVerified mismatch err = %v, want wrapped ErrChunkContentMismatch", err)
	}

	// Not found
	missing := hashOf(t, []byte("missing"))
	if _, err := s.ReadBlockVerified(ctx, missing, missing); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadBlockVerified on missing hash = %v, want wrapped ErrChunkNotFound", err)
	}
}
