package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLocatorFetchSyncer builds a Syncer over an in-memory local store, remote
// store, and synced-hash store — the minimal wiring dispatchRemoteFetch needs to
// resolve a chunk locator and route the read.
func newLocatorFetchSyncer(t *testing.T) (*Syncer, *remotememory.Store, *metadatamemory.MemoryMetadataStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = ms.Close() })
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		LocalChunkIndex: ms,
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = localStore.Close() })

	rem := remotememory.New()
	syncer := NewSyncer(localStore, rem, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	return syncer, rem, ms
}

// TestDispatchRemoteFetch_StandaloneLocatorRefused pins the post-#1493
// fail-closed contract: the startup migration repacked every legacy standalone
// chunk into a block, so a synced hash whose recorded locator is still
// standalone (BlockID == "") is post-migration drift. dispatchRemoteFetch must
// REFUSE the read — even when the legacy cas/ object still exists on the
// remote — instead of silently falling back or returning zeros.
func TestDispatchRemoteFetch_StandaloneLocatorRefused(t *testing.T) {
	ctx := context.Background()
	syncer, rem, ms := newLocatorFetchSyncer(t)

	data := bytes.Repeat([]byte{0xAB}, 4096)
	hash := block.ContentHash(blake3.Sum256(data))
	// Plant the legacy standalone cas/ object so the refusal below is
	// provably a policy decision, not a missing-object accident.
	if err := rem.Put(ctx, hash, data); err != nil {
		t.Fatalf("remote Put: %v", err)
	}
	// A pre-#1493 write path recorded a standalone locator on MarkSynced.
	if err := ms.MarkSynced(ctx, hash, block.ChunkLocator{WireLength: int64(len(data))}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	loc, ok, err := ms.GetLocator(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("GetLocator: ok=%v err=%v", ok, err)
	}
	if !loc.IsStandalone() {
		t.Fatalf("standalone write resolved to block: %+v", loc)
	}

	key, got, err := syncer.dispatchRemoteFetch(ctx, &block.FileChunk{Hash: hash})
	if err == nil {
		t.Fatalf("dispatchRemoteFetch: want post-migration drift refusal, got nil (data=%d bytes)", len(got))
	}
	if !strings.Contains(err.Error(), "post-migration drift") {
		t.Fatalf("dispatchRemoteFetch err = %v, want post-migration drift refusal", err)
	}
	if got != nil {
		t.Fatalf("dispatchRemoteFetch data = %d bytes, want nil on refusal", len(got))
	}
	if key != "" {
		t.Fatalf("dispatchRemoteFetch key = %q, want empty on refusal", key)
	}
}

// TestDispatchRemoteFetch_UnsyncedChunkFallsBackToLocal verifies that a chunk
// with NO synced marker (not uploaded yet) is not treated as remote data: the
// dispatch returns ("", nil, nil) so the caller serves it from the local tier,
// rather than failing closed. A read racing the async carve must not error.
func TestDispatchRemoteFetch_UnsyncedChunkFallsBackToLocal(t *testing.T) {
	ctx := context.Background()
	syncer, _, _ := newLocatorFetchSyncer(t)

	data := bytes.Repeat([]byte{0xCD}, 2048)
	hash := block.ContentHash(blake3.Sum256(data))
	// Deliberately NOT MarkSynced — GetLocator returns (false).

	key, got, err := syncer.dispatchRemoteFetch(ctx, &block.FileChunk{Hash: hash})
	if err != nil {
		t.Fatalf("dispatchRemoteFetch for unsynced chunk: want no error, got %v", err)
	}
	if got != nil || key != "" {
		t.Fatalf("dispatchRemoteFetch for unsynced chunk: want empty result, got key=%q data=%d bytes", key, len(got))
	}
}

// TestDispatchRemoteFetch_SyncedStandaloneLocatorRefused verifies that a chunk
// that IS synced but whose recorded locator has an empty BlockID (post-#1493
// drift — the migration should have rewritten every standalone locator) is
// refused fail-closed.
func TestDispatchRemoteFetch_SyncedStandaloneLocatorRefused(t *testing.T) {
	ctx := context.Background()
	syncer, _, ms := newLocatorFetchSyncer(t)

	data := bytes.Repeat([]byte{0xCD}, 2048)
	hash := block.ContentHash(blake3.Sum256(data))
	// Synced marker with an empty-BlockID (standalone) locator = drift.
	if err := ms.MarkSynced(ctx, hash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	key, got, err := syncer.dispatchRemoteFetch(ctx, &block.FileChunk{Hash: hash})
	if err == nil {
		t.Fatalf("dispatchRemoteFetch: want post-migration drift refusal, got nil (data=%d bytes)", len(got))
	}
	if !strings.Contains(err.Error(), "post-migration drift") {
		t.Fatalf("dispatchRemoteFetch err = %v, want post-migration drift refusal", err)
	}
	if got != nil || key != "" {
		t.Fatalf("dispatchRemoteFetch: want empty result on refusal, got key=%q data=%d bytes", key, len(got))
	}
}

// TestDispatchRemoteFetch_CarvedChunkRoundTrip is the positive companion to the
// refusal tests above: a chunk seeded through the live carve path (StoreChunk →
// addPendingHash → carveFlush) gets a block locator, and dispatchRemoteFetch
// round-trips it byte-identical through the packed-block read path.
func TestDispatchRemoteFetch_CarvedChunkRoundTrip(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("carved-round-trip-"), 512)
	hash := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	loc, synced, err := f.ms.GetLocator(ctx, hash)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}
	if loc.IsStandalone() {
		t.Fatalf("carve must record a block locator, got standalone: %+v", loc)
	}

	key, got, err := f.syncer.dispatchRemoteFetch(ctx, &block.FileChunk{Hash: hash})
	if err != nil {
		t.Fatalf("dispatchRemoteFetch: %v", err)
	}
	if key != block.FormatBlockKey(loc.BlockID) {
		t.Fatalf("key = %q, want %q", key, block.FormatBlockKey(loc.BlockID))
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("carved chunk round-trip mismatch")
	}
}

// TestDispatchRemoteFetch_BlockLocator covers the indirection PR3b will exploit: a
// synthetic block locator routes a ranged read into the enclosing block object and
// verifies the chunk's BLAKE3. This branch is never taken on the live PR3a path.
func TestDispatchRemoteFetch_BlockLocator(t *testing.T) {
	ctx := context.Background()
	syncer, rem, ms := newLocatorFetchSyncer(t)

	// A block with a leading filler chunk so the target sits at a non-zero
	// offset, exercising the range request.
	filler := bytes.Repeat([]byte{0x11}, 100)
	target := bytes.Repeat([]byte{0x22}, 4096)
	blockData := append(append([]byte{}, filler...), target...)
	const blockID = "block-test-0001"
	if err := rem.PutBlock(ctx, blockID, bytes.NewReader(blockData)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	hash := block.ContentHash(blake3.Sum256(target))
	loc := block.ChunkLocator{BlockID: blockID, WireOffset: int64(len(filler)), WireLength: int64(len(target))}
	if err := ms.MarkSynced(ctx, hash, loc); err != nil {
		t.Fatalf("MarkSynced block: %v", err)
	}

	key, got, err := syncer.dispatchRemoteFetch(ctx, &block.FileChunk{Hash: hash})
	if err != nil {
		t.Fatalf("dispatchRemoteFetch (block): %v", err)
	}
	if key != block.FormatBlockKey(blockID) {
		t.Fatalf("block key = %q, want %q", key, block.FormatBlockKey(blockID))
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("block chunk data mismatch")
	}
}

// TestDispatchRemoteFetch_BlockLocatorVerifyMismatch proves the block read path
// fails closed when the bytes at the located range do not hash to the expected
// chunk (corruption / wrong offset).
func TestDispatchRemoteFetch_BlockLocatorVerifyMismatch(t *testing.T) {
	ctx := context.Background()
	syncer, rem, ms := newLocatorFetchSyncer(t)

	target := bytes.Repeat([]byte{0x33}, 4096)
	hash := block.ContentHash(blake3.Sum256(target))
	// Store a block whose bytes do NOT match the claimed hash.
	corrupt := bytes.Repeat([]byte{0x34}, 4096)
	const blockID = "block-corrupt"
	if err := rem.PutBlock(ctx, blockID, bytes.NewReader(corrupt)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	loc := block.ChunkLocator{BlockID: blockID, WireOffset: 0, WireLength: int64(len(target))}
	if err := ms.MarkSynced(ctx, hash, loc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	_, _, err := syncer.dispatchRemoteFetch(ctx, &block.FileChunk{Hash: hash})
	if !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("block verify mismatch: got %v, want ErrChunkContentMismatch", err)
	}
}
