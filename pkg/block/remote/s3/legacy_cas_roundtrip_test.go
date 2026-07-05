package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// countPrefix returns how many objects in the mock live under keyPrefix.
func countPrefix(mock *mockS3, keyPrefix string) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	n := 0
	for k := range mock.objects {
		if strings.HasPrefix(k, keyPrefix) {
			n++
		}
	}
	return n
}

// TestLegacyCAS_MigrationRoundTrip is the real-release cas→blocks migration
// round-trip (#1557). It drives the S3 backend's remote.LegacyCASStore
// surface — the exact operations Store.Start's one-shot migration
// (migrateLegacyCAS → Syncer.migrateLegacyCASRemote) performs against a
// remote: WalkLegacyChunks (Phase R enumerate) → ReadLegacyChunkVerified
// (Phase R read) → DeleteLegacyChunk (Phase P purge) — over an on-disk
// fixture that is byte-identical to what a released v0.23.2 binary wrote.
//
// Fixture faithfulness: the objects are planted via Store.Put, whose key
// derivation (block.FormatCASKey → "cas/{hh}/{hh}/{hex}", two-level shard
// fanout), verbatim body (the base store's SealChunk is the identity
// transform, so wire bytes == plaintext), and "content-hash" user-metadata
// header are byte-for-byte identical to the v0.23.2 mirror write path — the
// released binary's mirror uploaded chunks through this same Put. Verified by
// diffing Store.Put and FormatCASKey against the v0.23.2 tag: both unchanged.
// So this is the genuine last-release serialized layout — not a develop-shaped
// stand-in like the in-package memory fixtures, which key objects by a Go map
// and never exercise the real cas/ key, shard fanout, LIST pagination, or
// metadata header.
func TestLegacyCAS_MigrationRoundTrip(t *testing.T) {
	store, mock := newTestStore(t)
	mock.mu.Lock()
	mock.listPageSize = 2 // force the migration's Walk across paginator pages
	mock.mu.Unlock()
	ctx := context.Background()

	// The migration binds to this interface, not the concrete Store.
	var legacy remote.LegacyCASStore = store

	// Plant a faithful pre-flip cas/ namespace: several standalone chunks,
	// each keyed by content hash exactly as v0.23.2's mirror wrote them.
	chunks := map[block.ContentHash][]byte{}
	var aPurgedHash block.ContentHash // any planted (and later purged) hash, for the idempotency probe
	for i := 0; i < 5; i++ {
		data := []byte(fmt.Sprintf("v0.23.2 standalone chunk %02d — pre-flip cas/ body", i))
		h := mustHash(data)
		if err := store.Put(ctx, h, data); err != nil {
			t.Fatalf("plant cas object %d: %v", i, err)
		}
		chunks[h] = data
		aPurgedHash = h
	}

	// A co-resident migrated block object. Phase P's cas/ purge must never
	// touch the blocks/ namespace — deleting a migrated block is data loss.
	const survivorBlockID = "0000000000000000-survivor"
	if err := store.PutBlock(ctx, survivorBlockID, strings.NewReader("packed block payload")); err != nil {
		t.Fatalf("plant blocks/ object: %v", err)
	}

	// --- Read-back: WalkLegacyChunks enumerates the cas/ namespace (the LIST
	// the migration's purge sweep performs) and ReadLegacyChunkVerified reads
	// each chunk through the same verified path the repack uses to fetch
	// standalone bytes. The read-back must be byte-identical. ---
	walked := map[block.ContentHash]int64{}
	if err := legacy.WalkLegacyChunks(ctx, func(h block.ContentHash, size int64) error {
		walked[h] = size
		return nil
	}); err != nil {
		t.Fatalf("WalkLegacyChunks: %v", err)
	}
	if len(walked) != len(chunks) {
		t.Fatalf("WalkLegacyChunks enumerated %d objects, want %d (blocks/ leaking into cas/ scan?)",
			len(walked), len(chunks))
	}
	for h, want := range chunks {
		if size, ok := walked[h]; !ok || size != int64(len(want)) {
			t.Fatalf("chunk %s: walked size=%d ok=%v, want %d", h, size, ok, len(want))
		}
		got, err := legacy.ReadLegacyChunkVerified(ctx, h)
		if err != nil {
			t.Fatalf("ReadLegacyChunkVerified(%s): %v", h, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("chunk %s NOT byte-identical after migration read-back", h)
		}
	}

	// --- Phase P (purge): delete every cas/ object, as the migration does
	// once the chunks are repacked into blocks. Collect first, then delete:
	// the mock paginates by numeric list-index, so deleting inside the Walk
	// callback would shift indices and skip keys (an artifact of the mock;
	// real S3 pages by start-after-key and is unaffected). ---
	var toPurge []block.ContentHash
	if err := legacy.WalkLegacyChunks(ctx, func(h block.ContentHash, _ int64) error {
		toPurge = append(toPurge, h)
		return nil
	}); err != nil {
		t.Fatalf("enumerate cas/ for purge: %v", err)
	}
	for _, h := range toPurge {
		if err := legacy.DeleteLegacyChunk(ctx, h); err != nil {
			t.Fatalf("purge cas/ object %s: %v", h, err)
		}
	}

	// cas/ namespace is empty ...
	if n := countPrefix(mock, "cas/"); n != 0 {
		t.Fatalf("cas/ namespace not purged: %d objects remain", n)
	}
	// ... but the migrated blocks/ object survived the purge.
	if n := countPrefix(mock, block.FormatBlockKey(survivorBlockID)); n != 1 {
		t.Fatalf("cas/ purge destroyed the migrated blocks/ object (found %d)", n)
	}

	// Idempotent: re-running the migration over an already-migrated remote is
	// a clean no-op — Walk over an empty cas/ namespace and DeleteLegacyChunk
	// on absent keys both succeed, and purged chunks are gone for good.
	if err := legacy.WalkLegacyChunks(ctx, func(block.ContentHash, int64) error {
		t.Fatal("re-run Walk visited a purged object")
		return nil
	}); err != nil {
		t.Fatalf("idempotent re-walk: %v", err)
	}
	if err := legacy.DeleteLegacyChunk(ctx, aPurgedHash); err != nil {
		t.Fatalf("idempotent re-delete of absent object: %v", err)
	}
	if _, err := legacy.ReadLegacyChunkVerified(ctx, aPurgedHash); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("purged chunk still readable: want ErrChunkNotFound, got %v", err)
	}
}
