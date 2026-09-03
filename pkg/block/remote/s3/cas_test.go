package s3

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// Migration-only tests for the legacy standalone-CAS surface (#1493 PR4):
// the S3 backend's ReadBlockVerified + Walk over the cas/ namespace. Delete
// with the migration.

// TestStore_ReadBlockVerified covers the happy path, the streaming-hash
// mismatch path, and the header pre-check fast-fail path.
func TestStore_ReadBlockVerified(t *testing.T) {
	store, mock := newTestStore(t)
	ctx := context.Background()

	data := []byte("verified read payload — must hash to the stored key")
	h := mustHash(data)
	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Happy path: body hashes to expected.
	got, err := store.ReadBlockVerified(ctx, h, h)
	if err != nil {
		t.Fatalf("ReadBlockVerified happy: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("ReadBlockVerified bytes mismatch")
	}

	// Header pre-check: the stored object carries content-hash == h, so
	// asking the verifier to expect a different hash trips the header
	// fast-fail before the body is read.
	wrongExpected := mustHash([]byte("different"))
	if _, err := store.ReadBlockVerified(ctx, h, wrongExpected); !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("ReadBlockVerified header pre-check: want ErrChunkContentMismatch, got %v", err)
	}

	// Streaming mismatch: store an object whose stamped header matches the
	// expected hash but whose body does NOT, so the header pre-check passes
	// and the streaming recompute catches the corruption.
	key := store.hashKey(h)
	mock.mu.Lock()
	mock.objects[key] = mockObject{
		data:         []byte("corrupted body that does not hash to h"),
		metadata:     map[string]string{"content-hash": h.CASKey()},
		lastModified: time.Now().UTC(),
	}
	mock.mu.Unlock()
	if _, err := store.ReadBlockVerified(ctx, h, h); !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("ReadBlockVerified streaming mismatch: want ErrChunkContentMismatch, got %v", err)
	}

	// Missing key maps to ErrChunkNotFound.
	if _, err := store.ReadBlockVerified(ctx, mustHash([]byte("gone")), mustHash([]byte("gone"))); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadBlockVerified missing: want ErrChunkNotFound, got %v", err)
	}
}

// TestStore_ReadBlockVerified_ClosedGuard pins the closed-store rejection for
// the legacy verified read.
func TestStore_ReadBlockVerified_ClosedGuard(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	h := mustHash([]byte("x"))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.ReadBlockVerified(ctx, h, h); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("ReadBlockVerified after Close: want ErrStoreClosed, got %v", err)
	}
}

// TestStore_Walk_Enumerate verifies Walk visits every CAS object exactly
// once with a non-zero LastModified, skips non-CAS keys, and spans
// multiple paginator pages.
func TestStore_Walk_Enumerate(t *testing.T) {
	store, mock := newTestStore(t)
	mock.mu.Lock()
	mock.listPageSize = 2 // force multi-page pagination
	mock.mu.Unlock()
	ctx := context.Background()

	want := make(map[block.ContentHash]int64)
	for i := 0; i < 5; i++ {
		p := []byte(fmt.Sprintf("walk-object-%d-payload", i))
		h := mustHash(p)
		want[h] = int64(len(p))
		if err := store.Put(ctx, h, p); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	// A non-CAS key under the bucket must be skipped by Walk.
	mock.mu.Lock()
	mock.objects["cas/zz/zz/not-a-valid-cas-key"] = mockObject{
		data: []byte("junk"), lastModified: time.Now().UTC(),
	}
	mock.mu.Unlock()

	seen := make(map[block.ContentHash]int)
	err := store.Walk(ctx, func(h block.ContentHash, m block.Meta) error {
		seen[h]++
		if m.LastModified.IsZero() {
			t.Errorf("Walk Meta.LastModified zero for %s", h)
		}
		if w, ok := want[h]; ok && m.Size != w {
			t.Errorf("Walk Size for %s = %d, want %d", h, m.Size, w)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("Walk visited %d objects, want %d", len(seen), len(want))
	}
	for h, n := range seen {
		if n != 1 {
			t.Errorf("Walk visited %s %d times, want 1", h, n)
		}
	}
}
