package common

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// recordingInvalidator captures InvalidateFile calls so tests can assert
// invocation count + arguments. Used by Task 1/Task 2 wiring tests.
type recordingInvalidator struct {
	calls []invalidatorCall
}

type invalidatorCall struct {
	payloadID metadata.PayloadID
	removed   []blockstore.ContentHash
}

func (r *recordingInvalidator) InvalidateFile(payloadID metadata.PayloadID, removed []blockstore.ContentHash) {
	r.calls = append(r.calls, invalidatorCall{payloadID: payloadID, removed: removed})
}

// TestCacheInvalidatorInterface_Compiles is a compile-time + behavioural
// guarantee that the CacheInvalidator interface (Plan 12-08 seam for
// CACHE-05 post-txn invalidation) accepts a payloadID + removedHashes
// pair. The interface is consumed by Plan 09 cache rewrite when the
// engine.Cache type implements it; this plan defines the surface.
func TestCacheInvalidatorInterface_Compiles(t *testing.T) {
	var inv CacheInvalidator = &recordingInvalidator{}
	hashes := []blockstore.ContentHash{{0x01}, {0x02}}
	inv.InvalidateFile(metadata.PayloadID("test"), hashes)

	got := inv.(*recordingInvalidator)
	if len(got.calls) != 1 {
		t.Fatalf("expected 1 InvalidateFile call, got %d", len(got.calls))
	}
	if got.calls[0].payloadID != metadata.PayloadID("test") {
		t.Errorf("got payloadID %q, want %q", got.calls[0].payloadID, "test")
	}
	if len(got.calls[0].removed) != 2 {
		t.Errorf("got %d removed hashes, want 2", len(got.calls[0].removed))
	}
}

// TestDiffRemovedHashes_SubsetRemoved asserts diffRemovedHashes returns the
// hashes that disappeared between an old BlockRef list and a new one. The
// helper backs Plan 12-08's surgical CACHE-05 invalidation contract — only
// hashes present in old but absent from new are reported.
func TestDiffRemovedHashes_SubsetRemoved(t *testing.T) {
	h1 := blockstore.ContentHash{0x01}
	h2 := blockstore.ContentHash{0x02}
	h3 := blockstore.ContentHash{0x03}

	oldBlocks := []blockstore.BlockRef{{Hash: h1}, {Hash: h2}, {Hash: h3}}
	newBlocks := []blockstore.BlockRef{{Hash: h1}, {Hash: h3}} // h2 dropped

	got := diffRemovedHashes(oldBlocks, newBlocks)
	if len(got) != 1 {
		t.Fatalf("got %d removed hashes, want 1", len(got))
	}
	if got[0] != h2 {
		t.Errorf("got removed hash %v, want %v", got[0], h2)
	}
}

// TestDiffRemovedHashes_AllRemoved exercises the legacy → empty case.
func TestDiffRemovedHashes_AllRemoved(t *testing.T) {
	h1 := blockstore.ContentHash{0x01}
	h2 := blockstore.ContentHash{0x02}
	oldBlocks := []blockstore.BlockRef{{Hash: h1}, {Hash: h2}}
	newBlocks := []blockstore.BlockRef{} // empty

	got := diffRemovedHashes(oldBlocks, newBlocks)
	if len(got) != 2 {
		t.Fatalf("got %d removed hashes, want 2", len(got))
	}
}

// TestDiffRemovedHashes_NothingRemoved exercises the no-op case.
func TestDiffRemovedHashes_NothingRemoved(t *testing.T) {
	h1 := blockstore.ContentHash{0x01}
	oldBlocks := []blockstore.BlockRef{{Hash: h1}}
	newBlocks := []blockstore.BlockRef{{Hash: h1}}

	got := diffRemovedHashes(oldBlocks, newBlocks)
	if len(got) != 0 {
		t.Fatalf("got %d removed hashes, want 0", len(got))
	}
}

// TestDiffRemovedHashes_DuplicateOldHash asserts that a hash present multiple
// times in oldBlocks (legitimate when the same chunk repeats in the file)
// reports each removal only when the hash is fully absent from newBlocks.
func TestDiffRemovedHashes_DuplicateOldHash(t *testing.T) {
	h1 := blockstore.ContentHash{0x01}
	h2 := blockstore.ContentHash{0x02}

	// h1 appears twice in old; in new it disappears entirely.
	oldBlocks := []blockstore.BlockRef{{Hash: h1}, {Hash: h2}, {Hash: h1}}
	newBlocks := []blockstore.BlockRef{{Hash: h2}}

	got := diffRemovedHashes(oldBlocks, newBlocks)
	// Both occurrences of h1 reported (caller treats this as multiplicity
	// hint for refcount-aware invalidation). Per the simple set-diff
	// contract, we report duplicates as-is so callers preserve refcount
	// arithmetic.
	if len(got) != 2 {
		t.Fatalf("got %d removed hashes, want 2 (h1 ×2)", len(got))
	}
}
