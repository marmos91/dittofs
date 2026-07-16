package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/compression"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// plantStandaloneChunk seeds the pre-flip state for one chunk: a sealed
// standalone object on the remote plus a standalone (zero-BlockID) synced
// marker — exactly what the legacy mirror left behind.
func plantStandaloneChunk(t *testing.T, ctx context.Context, f *carveFixture, base *remotememory.Store, stack remote.RemoteStore, data []byte) block.ContentHash {
	t.Helper()
	h := block.ContentHash(blake3.Sum256(data))
	sealed := data
	if sealer, ok := stack.(remote.ChunkSealer); ok {
		var err error
		sealed, err = sealer.SealChunk(ctx, h, data)
		if err != nil {
			t.Fatalf("seal fixture chunk: %v", err)
		}
	}
	if err := base.PutLegacyChunk(ctx, h, sealed); err != nil {
		t.Fatalf("plant standalone object: %v", err)
	}
	if err := f.ms.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
		t.Fatalf("plant standalone marker: %v", err)
	}
	return h
}

// assertMigrated asserts the post-migration invariants for one chunk: the
// locator is block-resident and the packed bytes read back byte-identical
// through the verified block-range path.
func assertMigrated(t *testing.T, ctx context.Context, f *carveFixture, h block.ContentHash, want []byte) {
	t.Helper()
	loc, ok, err := f.ms.GetLocator(ctx, h)
	if err != nil || !ok {
		t.Fatalf("GetLocator(%s): ok=%v err=%v", h, ok, err)
	}
	if loc.BlockID == "" {
		t.Fatalf("chunk %s still has a standalone locator after migration", h)
	}
	got, err := f.syncer.readChunkVerified(ctx, loc, h)
	if err != nil {
		t.Fatalf("readChunkVerified(%s): %v", h, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("chunk %s not byte-identical after migration", h)
	}
}

// sumLiveChunkCounts walks all block records and returns (records, sum).
func sumLiveChunkCounts(t *testing.T, ctx context.Context, f *carveFixture) (int, uint32) {
	t.Helper()
	var records int
	var sum uint32
	if err := f.ms.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
		records++
		sum += rec.LiveChunkCount
		return nil
	}); err != nil {
		t.Fatalf("WalkBlockRecords: %v", err)
	}
	return records, sum
}

// TestMigrateLegacyCAS_RepacksStandaloneChunks proves the core Phase R flow
// on a plain (undecorated) stack: standalone objects + markers in, packed
// blocks + rewritten locators + empty cas/ namespace out.
func TestMigrateLegacyCAS_RepacksStandaloneChunks(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	f := newCarveFixture(t, base, 96) // small target → multiple blocks

	chunks := map[string][]byte{}
	var hashes []block.ContentHash
	for i := 0; i < 7; i++ {
		data := []byte(fmt.Sprintf("standalone chunk %02d body ................", i))
		h := plantStandaloneChunk(t, ctx, f, base, base, data)
		chunks[h.String()] = data
		hashes = append(hashes, h)
	}

	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("migrateLegacyCASRemote: %v", err)
	}

	for _, h := range hashes {
		assertMigrated(t, ctx, f, h, chunks[h.String()])
	}

	if n, err := base.CountLegacyChunks(ctx); err != nil || n != 0 {
		t.Fatalf("cas/ namespace not empty after migration: n=%d err=%v", n, err)
	}
	records, live := sumLiveChunkCounts(t, ctx, f)
	if live != uint32(len(hashes)) {
		t.Fatalf("LiveChunkCount sum = %d, want %d", live, len(hashes))
	}
	if records < 2 {
		t.Fatalf("expected multiple carve-sized blocks, got %d record(s)", records)
	}
	blocksBefore := countRemoteBlocks(t, ctx, base)

	// Re-run: converged state is a strict no-op — no new blocks, no errors.
	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := countRemoteBlocks(t, ctx, base); got != blocksBefore {
		t.Fatalf("re-run minted new blocks: %d -> %d", blocksBefore, got)
	}
}

// TestMigrateLegacyCAS_DecoratedStack proves seal-fidelity end to end: legacy
// objects sealed by the compression decorator are unsealed on the legacy read,
// re-sealed into the packed block, and read back byte-identical through the
// decorated block-range path.
func TestMigrateLegacyCAS_DecoratedStack(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	stack, err := compression.NewRemote(base, compression.CompressionPolicy{Algo: compression.AlgoZstd})
	if err != nil {
		t.Fatalf("compression.NewRemote: %v", err)
	}
	f := newCarveFixture(t, stack, 1<<20)

	data := bytes.Repeat([]byte("compressible payload "), 512)
	h := plantStandaloneChunk(t, ctx, f, base, stack, data)

	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("migrateLegacyCASRemote: %v", err)
	}
	assertMigrated(t, ctx, f, h, data)
	if n, _ := base.CountLegacyChunks(ctx); n != 0 {
		t.Fatalf("cas/ namespace not empty: %d", n)
	}
}

// TestMigrateLegacyCAS_LostChunkDropsMarker proves the pre-existing-data-loss
// path: a marker with no bytes anywhere is dropped (loudly) and does not
// abort the rest of the migration.
func TestMigrateLegacyCAS_LostChunkDropsMarker(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	f := newCarveFixture(t, base, 1<<20)

	lost := block.ContentHash(blake3.Sum256([]byte("vanished")))
	if err := f.ms.MarkSynced(ctx, lost, block.ChunkLocator{}); err != nil {
		t.Fatal(err)
	}
	data := []byte("survivor chunk")
	h := plantStandaloneChunk(t, ctx, f, base, base, data)

	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("migrateLegacyCASRemote: %v", err)
	}
	assertMigrated(t, ctx, f, h, data)

	if synced, err := f.ms.IsSynced(ctx, lost); err != nil || synced {
		t.Fatalf("lost chunk's marker should be dropped: synced=%v err=%v", synced, err)
	}
}

// TestMigrateLegacyCAS_PurgesUnreferencedObjects proves Phase P: standalone
// objects with no marker (pre-flip Put-then-Mark crash orphans) are deleted
// even when nothing needs repacking.
func TestMigrateLegacyCAS_PurgesUnreferencedObjects(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	f := newCarveFixture(t, base, 1<<20)

	orphan := block.ContentHash(blake3.Sum256([]byte("orphan object")))
	if err := base.PutLegacyChunk(ctx, orphan, []byte("orphan object")); err != nil {
		t.Fatal(err)
	}

	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("migrateLegacyCASRemote: %v", err)
	}
	if n, _ := base.CountLegacyChunks(ctx); n != 0 {
		t.Fatalf("orphan cas object not purged: %d left", n)
	}
	if records, _ := sumLiveChunkCounts(t, ctx, f); records != 0 {
		t.Fatalf("purge must not mint block records, got %d", records)
	}
}

// TestMigrateLegacyCAS_AlreadyRewrittenObjectPurged proves the
// crash-after-commit-before-delete window: a chunk whose locator was already
// rewritten but whose standalone object still exists is NOT repacked — the
// stale object is simply purged.
func TestMigrateLegacyCAS_AlreadyRewrittenObjectPurged(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	f := newCarveFixture(t, base, 1<<20)

	// Full migration for one chunk first.
	data := []byte("chunk migrated in a previous run")
	h := plantStandaloneChunk(t, ctx, f, base, base, data)
	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatal(err)
	}
	blocksAfterFirst := countRemoteBlocks(t, ctx, base)

	// Simulate the crash window: the standalone object reappears (as if the
	// post-commit delete never ran) while the locator already points at a block.
	if err := base.PutLegacyChunk(ctx, h, data); err != nil {
		t.Fatal(err)
	}

	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n, _ := base.CountLegacyChunks(ctx); n != 0 {
		t.Fatal("stale standalone object should be purged")
	}
	if got := countRemoteBlocks(t, ctx, base); got != blocksAfterFirst {
		t.Fatalf("already-migrated chunk was repacked: blocks %d -> %d", blocksAfterFirst, got)
	}
	assertMigrated(t, ctx, f, h, data)
}

// failingPutBlockRemote fails PutBlock after allowing `allow` successful
// calls — the kill-mid-migration fault injector.
type failingPutBlockRemote struct {
	*remotememory.Store
	allow int
	calls int
}

var errInjectedPutBlock = errors.New("injected PutBlock failure")

func (f *failingPutBlockRemote) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	f.calls++
	if f.calls > f.allow {
		return errInjectedPutBlock
	}
	return f.Store.PutBlock(ctx, blockID, r)
}

// TestMigrateLegacyCAS_ResumeAfterFailure proves resumability: a migration
// killed after committing its first block (PutBlock fails on the second)
// converges on re-run — every chunk block-resident, cas/ empty, and the
// LiveChunkCount ledger exactly accounts for every chunk with no
// partially-pointed records.
func TestMigrateLegacyCAS_ResumeAfterFailure(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	wrapper := &failingPutBlockRemote{Store: base, allow: 1}
	f := newCarveFixture(t, wrapper, 64) // tiny target → several blocks needed

	chunks := map[string][]byte{}
	var hashes []block.ContentHash
	for i := 0; i < 6; i++ {
		data := []byte(fmt.Sprintf("resumable chunk %02d ......................", i))
		h := plantStandaloneChunk(t, ctx, f, base, base, data)
		chunks[h.String()] = data
		hashes = append(hashes, h)
	}

	err := f.syncer.migrateLegacyCASRemote(ctx)
	if !errors.Is(err, errInjectedPutBlock) {
		t.Fatalf("expected injected failure, got %v", err)
	}

	// Some chunks migrated (first block committed), some still standalone.
	migrated := 0
	for _, h := range hashes {
		if loc, ok, _ := f.ms.GetLocator(ctx, h); ok && loc.BlockID != "" {
			migrated++
		}
	}
	if migrated == 0 || migrated == len(hashes) {
		t.Fatalf("fault injection should leave a partial state, migrated=%d/%d", migrated, len(hashes))
	}

	// Heal the remote and re-run: must converge.
	wrapper.calls = 0
	wrapper.allow = 1 << 30
	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("re-run after heal: %v", err)
	}

	for _, h := range hashes {
		assertMigrated(t, ctx, f, h, chunks[h.String()])
	}
	if n, _ := base.CountLegacyChunks(ctx); n != 0 {
		t.Fatalf("cas/ not empty after converged migration: %d", n)
	}
	// Ledger check: no record leaks — every live count is backed by a chunk
	// whose locator points at that record's block.
	pointedAt := map[string]uint32{}
	for _, h := range hashes {
		loc, _, _ := f.ms.GetLocator(ctx, h)
		pointedAt[loc.BlockID]++
	}
	if err := f.ms.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
		if rec.LiveChunkCount != pointedAt[rec.BlockID] {
			return fmt.Errorf("record %s LiveChunkCount=%d but %d chunks point at it",
				rec.BlockID, rec.LiveChunkCount, pointedAt[rec.BlockID])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateLegacyCAS_NoRemoteIsNoOp proves local-only shares skip cleanly.
func TestMigrateLegacyCAS_NoRemoteIsNoOp(t *testing.T) {
	ctx := context.Background()
	f := newCarveFixture(t, remotememory.New(), 1<<20)
	f.syncer.mu.Lock()
	f.syncer.remoteStore = nil
	f.syncer.mu.Unlock()
	if err := f.syncer.migrateLegacyCASRemote(ctx); err != nil {
		t.Fatalf("local-only migration should be a no-op: %v", err)
	}
}
