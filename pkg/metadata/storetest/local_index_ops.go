package storetest

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// LocalChunkIndexProvider is implemented by stores with full LocalChunkIndex
// support. The conformance suite type-asserts to this and skips if unimplemented.
type LocalChunkIndexProvider interface {
	LocalChunkIndexEnabled() bool
}

func runLocalIndexOps(t *testing.T, store metadata.Store) {
	t.Helper()
	if p, ok := store.(LocalChunkIndexProvider); !ok || !p.LocalChunkIndexEnabled() {
		t.Skip("LocalChunkIndex not implemented by this backend")
	}

	ctx := context.Background()

	hash := block.ContentHash{0xAA, 0xBB, 0xCC}
	loc := block.LocalChunkLocation{LogBlobID: "logblob-001", RawOffset: 4096, RawLength: 512}

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		if err := store.PutLocalLocation(ctx, hash, loc); err != nil {
			t.Fatalf("PutLocalLocation() error = %v", err)
		}
		got, found, err := store.GetLocalLocation(ctx, hash)
		if err != nil {
			t.Fatalf("GetLocalLocation() error = %v", err)
		}
		if !found {
			t.Fatal("GetLocalLocation() found = false, want true")
		}
		if got != loc {
			t.Errorf("GetLocalLocation() = %+v, want %+v", got, loc)
		}
	})

	t.Run("UpsertOverwrites", func(t *testing.T) {
		h := block.ContentHash{0x11}
		first := block.LocalChunkLocation{LogBlobID: "blob-first", RawOffset: 0, RawLength: 100}
		second := block.LocalChunkLocation{LogBlobID: "blob-second", RawOffset: 100, RawLength: 200}

		if err := store.PutLocalLocation(ctx, h, first); err != nil {
			t.Fatalf("PutLocalLocation(first) error = %v", err)
		}
		if err := store.PutLocalLocation(ctx, h, second); err != nil {
			t.Fatalf("PutLocalLocation(second) error = %v", err)
		}

		got, found, err := store.GetLocalLocation(ctx, h)
		if err != nil {
			t.Fatalf("GetLocalLocation() error = %v", err)
		}
		if !found {
			t.Fatal("GetLocalLocation() found = false, want true")
		}
		if got != second {
			t.Errorf("GetLocalLocation() = %+v, want %+v (second write should win)", got, second)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		h := block.ContentHash{0x22}
		l := block.LocalChunkLocation{LogBlobID: "blob-del", RawOffset: 8, RawLength: 16}
		if err := store.PutLocalLocation(ctx, h, l); err != nil {
			t.Fatalf("PutLocalLocation() error = %v", err)
		}
		if err := store.DeleteLocalLocation(ctx, h); err != nil {
			t.Fatalf("DeleteLocalLocation() error = %v", err)
		}
		_, found, err := store.GetLocalLocation(ctx, h)
		if err != nil {
			t.Fatalf("GetLocalLocation(after delete) error = %v", err)
		}
		if found {
			t.Fatal("GetLocalLocation(after delete) found = true, want false")
		}
	})

	t.Run("MissingReturnsNotFound", func(t *testing.T) {
		var absent block.ContentHash
		absent[0] = 0xFF
		_, found, err := store.GetLocalLocation(ctx, absent)
		if err != nil {
			t.Fatalf("GetLocalLocation(missing) error = %v", err)
		}
		if found {
			t.Fatal("GetLocalLocation(missing) found = true, want false")
		}
	})
}
