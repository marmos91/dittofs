package engine_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"lukechampine.com/blake3"
)

// newEngineOverMetadataStore builds an engine.Store with a MEMORY local store
// (so the eager small-file dedup path's bs.local.GetFileSize gate works after a
// plain WriteAt — the fs local store only refreshes that size during recovery)
// wired to the supplied REAL metadata store as the FileBlockStore, plus the
// faithful testCoordinator. This lets the eager dedup hit route through the real
// Remote-gated GetByHash, which is what triggers issue #1245 Bug A on a Pending
// donor.
func newEngineOverMetadataStore(t *testing.T, ms metadata.Store) *engine.Store {
	t.Helper()
	localStore := memory.New()
	coord := &testCoordinator{store: ms}
	syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          localStore,
		Remote:         nil,
		Syncer:         syncer,
		FileBlockStore: ms,
		Coordinator:    coord,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// seedPendingDonor installs a donor file into the metadata store that mimics the
// real rollup-persister's intermediate state for a single-chunk small file:
//
//   - A per-chunk FileBlock row in BlockStatePending (so the Remote-gated
//     GetByHash returns nil — the donor is not yet durable).
//   - A File row whose FileAttr.ObjectID is the single-block Merkle root, so the
//     object-id index is published and FindByObjectID HITS.
//
// This is exactly the window the rollup persister leaves open: engine.go writes
// the FileBlock rows as Pending and PersistFileBlocks publishes the ObjectID in
// the same pass, and over a slow remote those rows stay Pending for seconds or
// minutes. Returns the published ObjectID and the donor's content hash.
func seedPendingDonor(t *testing.T, ms metadata.Store, shareName, name string, rootHandle metadata.FileHandle, content []byte) (block.ObjectID, block.ContentHash) {
	t.Helper()
	ctx := context.Background()

	var hash block.ContentHash
	sum := blake3.Sum256(content)
	copy(hash[:], sum[:])
	blockRef := block.BlockRef{Hash: hash, Offset: 0, Size: uint32(len(content))}
	objectID := block.ComputeObjectID([]block.BlockRef{blockRef})

	path := "/" + name
	handle, err := ms.GenerateHandle(ctx, shareName, path)
	if err != nil {
		t.Fatalf("GenerateHandle donor: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle donor: %v", err)
	}
	payloadID := shareNamePrefix(shareName) + "/" + id.String()

	// FileBlock row in Pending state — GetByHash is Remote-gated, so this row
	// will NOT resolve via the coordinator's IncrementRefCount path.
	fb := &block.FileBlock{
		ID:       payloadID + "/0",
		Hash:     hash,
		DataSize: uint32(len(content)),
		State:    block.BlockStatePending,
		RefCount: 1,
	}
	if err := ms.Put(ctx, fb); err != nil {
		t.Fatalf("Put donor FileBlock: %v", err)
	}

	// File row with the published ObjectID + Blocks manifest so FindByObjectID
	// resolves to this donor.
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			UID:       1000,
			GID:       1000,
			Size:      uint64(len(content)),
			PayloadID: metadata.PayloadID(payloadID),
			ObjectID:  objectID,
			Blocks:    []block.BlockRef{blockRef},
		},
	}
	if err := ms.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile donor: %v", err)
	}
	if err := ms.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent donor: %v", err)
	}
	if err := ms.SetChild(ctx, rootHandle, name, handle); err != nil {
		t.Fatalf("SetChild donor: %v", err)
	}
	if err := ms.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount donor: %v", err)
	}

	// Sanity: FindByObjectID must hit, and GetByHash must be gated to nil
	// (Pending donor). If either of these breaks the repro is invalid.
	if got, err := ms.FindByObjectID(ctx, objectID); err != nil || len(got) == 0 {
		t.Fatalf("seedPendingDonor: FindByObjectID must hit (err=%v, blocks=%d)", err, len(got))
	}
	if fbGot, err := ms.GetByHash(ctx, hash); err != nil || fbGot != nil {
		t.Fatalf("seedPendingDonor: GetByHash must be Remote-gated to nil for a Pending donor (err=%v, fb=%v)", err, fbGot)
	}
	return objectID, hash
}

func shareNamePrefix(shareName string) string {
	if len(shareName) > 0 && shareName[0] == '/' {
		return shareName[1:]
	}
	return shareName
}

// runPendingDonorEagerDedup is the shared body for issue #1245 Bug A through the
// EAGER small-file dedup path.
//
// A donor with a published ObjectID but Pending (non-durable) FileBlock rows is
// seeded. A second, byte-identical small file B is written and Flushed: the
// eager small-file dedup fast-path computes the same single-block ObjectID,
// FindByObjectID HITS the Pending donor, and applyFileLevelDedupHit tries to
// IncrementRefCount on it.
//
// BEFORE the fix: IncrementRefCount → GetByHash (Remote-gated) → nil →
// metadata.ErrFileBlockNotFound → applyFileLevelDedupHit returns an error →
// Flush returns "eager small-file dedup: …no FileBlock…" → EIO, wedging the
// SMB/NFS session on CLOSE/COMMIT.
//
// AFTER the fix: the Pending donor is treated as a clean MISS — Flush succeeds,
// falls through to the normal per-block upload of B's own blocks, and B is
// fully readable.
func runPendingDonorEagerDedup(t *testing.T, ms metadata.Store, sharePrefix string) {
	t.Helper()
	ctx := context.Background()

	shareName := sharePrefix + "-pending-donor"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverMetadataStore(t, ms)

	// <= chunker.MinChunkSize so a single chunk drives the eager fast-path.
	content := distinctContent(0x42, 64*1024)

	// Seed donor A: published ObjectID + Pending FileBlock row.
	objectID, donorHash := seedPendingDonor(t, ms, shareName, "donor.bin", rootHandle, content)
	_ = objectID
	_ = donorHash

	// File B: identical content, backed by a REAL metadata File row (so the
	// pending-donor MISS falls through to a faithful per-block upload +
	// PersistFileBlocks against B's own row). Write then Flush — the eager
	// path fires.
	pidB, _ := createRealFile(t, ms, shareName, "clone.bin", rootHandle)
	if _, err := bs.WriteAt(ctx, pidB, nil, content, 0); err != nil {
		t.Fatalf("WriteAt clone: %v", err)
	}

	// THE failing assertion before the fix: Flush must NOT error on a Pending
	// donor — it must treat the eager hit as a miss and fall through.
	if _, err := bs.Flush(ctx, pidB); err != nil {
		t.Fatalf("Flush clone against Pending donor returned error (Bug A — should be a clean MISS): %v", err)
	}

	// After the miss, B must be fully readable from its own (locally rolled-up)
	// blocks — no partial / wedged state.
	readBack := make([]byte, len(content))
	n, err := bs.ReadAt(ctx, pidB, nil, readBack, 0)
	if err != nil {
		t.Fatalf("ReadAt clone after Pending-donor miss: %v", err)
	}
	if n != len(content) {
		t.Fatalf("ReadAt clone short read: got %d, want %d", n, len(content))
	}
	for i := range content {
		if readBack[i] != content[i] {
			t.Fatalf("clone content mismatch at byte %d after Pending-donor miss", i)
		}
	}
}

// TestMemoryDedup_PendingDonor_TreatedAsMiss locks in issue #1245 Bug A on the
// in-memory metadata backend through the REAL engine eager-dedup Flush path.
func TestMemoryDedup_PendingDonor_TreatedAsMiss(t *testing.T) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	runPendingDonorEagerDedup(t, ms, "mem")
}

// TestBadgerDedup_PendingDonor_TreatedAsMiss locks in issue #1245 Bug A on the
// Badger metadata backend through the REAL engine eager-dedup Flush path.
func TestBadgerDedup_PendingDonor_TreatedAsMiss(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	runPendingDonorEagerDedup(t, ms, "badger")
}
