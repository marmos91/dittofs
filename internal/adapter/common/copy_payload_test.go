package common

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeCoordinator records IncrementRefCount/DecrementRefCount/PersistFileBlocks
// invocations and lets tests inject failure on the Nth IncrementRefCount call
// for the rollback contract.
type fakeCoordinator struct {
	incrementCalls    []block.ContentHash
	decrementCalls    []block.ContentHash
	reapIDs           []string
	persistCalls      []persistCall
	failOnNthIncrErr  error
	failOnNthIncrTrip int // 1-based; 0 = never fail
}

type persistCall struct {
	payloadID string
	blocks    []block.BlockRef
	objectID  block.ObjectID
}

func (f *fakeCoordinator) IncrementRefCount(_ context.Context, hash block.ContentHash) error {
	f.incrementCalls = append(f.incrementCalls, hash)
	if f.failOnNthIncrTrip > 0 && len(f.incrementCalls) == f.failOnNthIncrTrip {
		return f.failOnNthIncrErr
	}
	return nil
}

func (f *fakeCoordinator) DecrementRefCount(_ context.Context, hash block.ContentHash) (uint32, error) {
	f.decrementCalls = append(f.decrementCalls, hash)
	return 0, nil
}

// DecrementRefCountAndReap is the engine Delete/Truncate reclaim path, keyed by
// EXACT ID "{payloadID}/{offset}" (never by hash). CopyPayload — the only path
// these tests exercise — never reaps, so this just satisfies the interface.
func (f *fakeCoordinator) DecrementRefCountAndReap(_ context.Context, payloadID string, offset uint64) (uint32, error) {
	f.reapIDs = append(f.reapIDs, fmt.Sprintf("%s/%d", payloadID, offset))
	return 0, nil
}

func (f *fakeCoordinator) PersistFileBlocks(_ context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error {
	f.persistCalls = append(f.persistCalls, persistCall{payloadID: payloadID, blocks: blocks, objectID: objectID})
	return nil
}

func (f *fakeCoordinator) GetPersistedBlocks(_ context.Context, _ string) ([]block.BlockRef, error) {
	return nil, nil
}

// FindByObjectID stub. Adapter-common tests don't exercise short-circuit
// lookups (those live in pkg/blockstore/engine and pkg/metadata/storetest);
// satisfy the interface so the fake satisfies engine.MetadataCoordinator.
func (f *fakeCoordinator) FindByObjectID(_ context.Context, _ block.ObjectID) ([]block.BlockRef, error) {
	return nil, nil
}

// GetFileObjectID stub. Adapter-common tests do not drive the
// Syncer.Flush short-circuit path; returning the zero ObjectID + nil is
// the "never quiesced" disposition that keeps the interface satisfied
// without affecting any assertions.
func (f *fakeCoordinator) GetFileObjectID(_ context.Context, _ string) (block.ObjectID, error) {
	return block.ObjectID{}, nil
}

// putTestFile creates a file with the given Blocks list in the metadata
// store and returns its handle. Used to seed src/dst before CopyPayload.
func putTestFile(t *testing.T, ms metadata.Store, path string, payloadID metadata.PayloadID, blocks []block.BlockRef, size uint64) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()

	now := time.Now()
	file := &metadata.File{
		ID:        uuid.New(),
		ShareName: "test-share",
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeRegular,
			Mode:         0o644,
			UID:          1000,
			GID:          1000,
			Nlink:        1,
			Size:         size,
			Atime:        now,
			Mtime:        now,
			Ctime:        now,
			CreationTime: now,
			PayloadID:    payloadID,
			Blocks:       blocks,
		},
	}

	if err := ms.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile(%s) failed: %v", path, err)
	}

	// Memory store derives the handle via EncodeShareHandle(ShareName, ID).
	// The same encoding is used in tx.PutFile, so a re-encoded handle here
	// matches the key the file was stored under.
	handle, err := metadata.EncodeShareHandle(file.ShareName, file.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle failed: %v", err)
	}
	return handle
}

// TestCopyPayload_AtomicSuccess seeds src with 3 distinct BlockRefs and
// asserts:
//   - dst's FileAttr.Blocks matches src's BlockRefs
//   - each unique hash got exactly one IncrementRefCount call
//   - PutFile(dst) was invoked through the metadataStore (visible via
//     a GetFile call after the helper returns)
//   - cache.InvalidateFile fired POST-txn with payloadID = dstPayloadID
func TestCopyPayload_AtomicSuccess(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	// Seed src with 3 distinct BlockRefs.
	srcBlocks := []block.BlockRef{
		{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096},
		{Hash: block.ContentHash{0x02}, Offset: 4096, Size: 4096},
		{Hash: block.ContentHash{0x03}, Offset: 8192, Size: 4096},
	}
	srcHandle := putTestFile(t, ms, "/src.bin", "src-pid", srcBlocks, 12288)

	// Seed empty dst.
	dstHandle := putTestFile(t, ms, "/dst.bin", "dst-pid", nil, 0)

	cache := &recordingInvalidator{}

	err := CopyPayload(ctx, bs, ms, cache, srcHandle, dstHandle, "src-pid", "dst-pid")
	if err != nil {
		t.Fatalf("CopyPayload failed: %v", err)
	}

	// Coordinator: each unique hash incremented exactly once.
	if len(coord.incrementCalls) != 3 {
		t.Fatalf("got %d IncrementRefCount calls, want 3", len(coord.incrementCalls))
	}

	// dst persisted with src's BlockRefs and src's Size.
	dstFile, err := ms.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatalf("GetFile(dst) after CopyPayload failed: %v", err)
	}
	if len(dstFile.Blocks) != 3 {
		t.Fatalf("dst has %d Blocks, want 3", len(dstFile.Blocks))
	}
	for i := range srcBlocks {
		if dstFile.Blocks[i].Hash != srcBlocks[i].Hash {
			t.Errorf("dst.Blocks[%d].Hash = %v, want %v", i, dstFile.Blocks[i].Hash, srcBlocks[i].Hash)
		}
	}
	if dstFile.Size != 12288 {
		t.Errorf("dst.Size = %d, want 12288", dstFile.Size)
	}

	// Cache: InvalidateFile fired POST-txn for dst.
	if len(cache.calls) != 1 {
		t.Fatalf("got %d InvalidateFile calls, want 1", len(cache.calls))
	}
	if cache.calls[0].payloadID != metadata.PayloadID("dst-pid") {
		t.Errorf("InvalidateFile payloadID = %q, want %q", cache.calls[0].payloadID, "dst-pid")
	}
}

// TestCopyPayload_RollsBackOnIncrementError pins the rollback contract:
// mid-loop IncrementRefCount failure rolls back ALL writes (no PutFile(dst),
// no partial dstFileAttr persisted, no InvalidateFile call).
func TestCopyPayload_RollsBackOnIncrementError(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	injErr := errors.New("synthetic increment failure")
	coord := &fakeCoordinator{
		failOnNthIncrErr:  injErr,
		failOnNthIncrTrip: 2, // fail on the 2nd Increment
	}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.BlockRef{
		{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096},
		{Hash: block.ContentHash{0x02}, Offset: 4096, Size: 4096},
		{Hash: block.ContentHash{0x03}, Offset: 8192, Size: 4096},
	}
	srcHandle := putTestFile(t, ms, "/src2.bin", "src2-pid", srcBlocks, 12288)
	dstHandle := putTestFile(t, ms, "/dst2.bin", "dst2-pid", nil, 0)

	cache := &recordingInvalidator{}

	err := CopyPayload(ctx, bs, ms, cache, srcHandle, dstHandle, "src2-pid", "dst2-pid")
	if err == nil {
		t.Fatal("CopyPayload returned nil error; want propagated increment failure")
	}
	if !errors.Is(err, injErr) {
		t.Errorf("CopyPayload error = %v, want wrapping of %v", err, injErr)
	}

	// dst.Blocks must be unchanged (still nil/empty).
	dstFile, _ := ms.GetFile(ctx, dstHandle)
	if dstFile != nil && len(dstFile.Blocks) != 0 {
		t.Errorf("dst.Blocks = %v, want empty (txn must roll back)", dstFile.Blocks)
	}

	// dst.Size unchanged (0).
	if dstFile != nil && dstFile.Size != 0 {
		t.Errorf("dst.Size = %d, want 0 (txn must roll back)", dstFile.Size)
	}

	// Cache: NO InvalidateFile call on failure.
	if len(cache.calls) != 0 {
		t.Errorf("got %d InvalidateFile calls, want 0 on failure path", len(cache.calls))
	}
}

// TestCopyPayload_LegacyEmptyBlocks asserts the no-work path: src has no
// FileAttr.Blocks (legacy file). engine.CopyPayload returns empty newBlocks
// (no work, no IncrementRefCount calls). Helper still persists dstFileAttr
// (empty Blocks) and fires post-txn InvalidateFile.
func TestCopyPayload_LegacyEmptyBlocks(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	// Legacy src: no Blocks.
	srcHandle := putTestFile(t, ms, "/legacy-src.bin", "legacy-src-pid", nil, 1024)
	dstHandle := putTestFile(t, ms, "/legacy-dst.bin", "legacy-dst-pid", nil, 0)

	cache := &recordingInvalidator{}

	if err := CopyPayload(ctx, bs, ms, cache, srcHandle, dstHandle, "legacy-src-pid", "legacy-dst-pid"); err != nil {
		t.Fatalf("CopyPayload failed: %v", err)
	}

	if len(coord.incrementCalls) != 0 {
		t.Errorf("got %d IncrementRefCount calls on legacy path, want 0", len(coord.incrementCalls))
	}

	dstFile, _ := ms.GetFile(ctx, dstHandle)
	if dstFile == nil {
		t.Fatal("dst not found after CopyPayload")
	}
	if len(dstFile.Blocks) != 0 {
		t.Errorf("legacy dst.Blocks = %v, want empty", dstFile.Blocks)
	}

	// Cache invalidation still fires (dst content notionally changed).
	if len(cache.calls) != 1 {
		t.Errorf("got %d InvalidateFile calls, want 1 on legacy success path", len(cache.calls))
	}
}

// TestCopyPayload_NilCacheTolerated asserts that a nil cache argument is
// tolerated by the helper (callers pass nil until the engine.Cache is
// wired).
func TestCopyPayload_NilCacheTolerated(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.BlockRef{
		{Hash: block.ContentHash{0xAA}, Offset: 0, Size: 4096},
	}
	srcHandle := putTestFile(t, ms, "/src3.bin", "src3-pid", srcBlocks, 4096)
	dstHandle := putTestFile(t, ms, "/dst3.bin", "dst3-pid", nil, 0)

	if err := CopyPayload(ctx, bs, ms, nil, srcHandle, dstHandle, "src3-pid", "dst3-pid"); err != nil {
		t.Fatalf("CopyPayload(nil cache) failed: %v", err)
	}

	if len(coord.incrementCalls) != 1 {
		t.Errorf("got %d IncrementRefCount calls, want 1", len(coord.incrementCalls))
	}
}

// newCopyTestEngineWithMS constructs an engine wired against a caller-
// supplied MemoryMetadataStore so the test can both seed files and observe
// post-txn state via the same store.
func newCopyTestEngineWithMS(t *testing.T, coord *fakeCoordinator, ms *metadatamemory.MemoryMetadataStore) *engine.Store {
	t.Helper()

	tmpDir := t.TempDir()
	localStore, err := fs.NewWithOptions(tmpDir, 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions failed: %v", err)
	}

	syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())

	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		FileBlockStore:  ms,
		Coordinator:     coord,
		ReadBufferBytes: 0,
		PrefetchWorkers: 0,
	})
	if err != nil {
		t.Fatalf("engine.New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	return bs
}
