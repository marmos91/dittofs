package storetest

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func runLocalIndexOps(t *testing.T, store metadata.Store) {
	t.Helper()

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

	// WalkEnumeratesAll guards the restart-seed path: FSStore.Walk /
	// ListUnsynced discover logblob-resident chunks by type-asserting the
	// LocalChunkIndex to this exact walker capability. A production backend
	// that fails to implement it silently drops crash-stranded unsynced chunks
	// from re-seeding, so this FAILS (not skips) when the method is absent.
	//
	// The suite shares one store across subtests, so the walk legitimately
	// contains entries from earlier cases — this asserts SUPERSET semantics, not
	// exact-set equality: every location we Put here is enumerated with the
	// correct value, each hash is yielded at most once, and a deleted entry is
	// never surfaced. It does not couple to any other subtest's contents.
	t.Run("WalkEnumeratesAll", func(t *testing.T) {
		walker, ok := store.(interface {
			WalkLocalLocations(context.Context, func(block.ContentHash, block.LocalChunkLocation) error) error
		})
		if !ok {
			t.Fatalf("%T does not implement WalkLocalLocations — restart-seed of unsynced logblob chunks is broken on this backend", store)
		}

		want := map[block.ContentHash]block.LocalChunkLocation{
			{0xA1}: {LogBlobID: "walk-blob-1", RawOffset: 0, RawLength: 128},
			{0xA2}: {LogBlobID: "walk-blob-2", RawOffset: 128, RawLength: 256},
			{0xA3}: {LogBlobID: "walk-blob-3", RawOffset: 384, RawLength: 512},
		}
		for h, l := range want {
			if err := store.PutLocalLocation(ctx, h, l); err != nil {
				t.Fatalf("PutLocalLocation(%x) error = %v", h, err)
			}
		}

		// Sentinel: put then delete — the walk must NOT surface it (proves Walk
		// honors deletes, not just accumulates every hash ever written).
		deleted := block.ContentHash{0xA4}
		if err := store.PutLocalLocation(ctx, deleted, block.LocalChunkLocation{LogBlobID: "walk-blob-deleted", RawLength: 64}); err != nil {
			t.Fatalf("PutLocalLocation(sentinel) error = %v", err)
		}
		if err := store.DeleteLocalLocation(ctx, deleted); err != nil {
			t.Fatalf("DeleteLocalLocation(sentinel) error = %v", err)
		}

		got := make(map[block.ContentHash]block.LocalChunkLocation)
		seen := make(map[block.ContentHash]int)
		if err := walker.WalkLocalLocations(ctx, func(h block.ContentHash, l block.LocalChunkLocation) error {
			got[h] = l
			seen[h]++
			return nil
		}); err != nil {
			t.Fatalf("WalkLocalLocations() error = %v", err)
		}

		for h, n := range seen {
			if n > 1 {
				t.Errorf("WalkLocalLocations() enumerated hash %x %d times, want at most 1", h, n)
			}
		}
		if _, ok := got[deleted]; ok {
			t.Errorf("WalkLocalLocations() surfaced deleted hash %x", deleted)
		}
		for h, l := range want {
			g, ok := got[h]
			if !ok {
				t.Errorf("WalkLocalLocations() did not enumerate hash %x", h)
				continue
			}
			if g != l {
				t.Errorf("WalkLocalLocations()[%x] = %+v, want %+v", h, g, l)
			}
		}
	})
}
