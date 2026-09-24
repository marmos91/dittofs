package common

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeCoordinator records IncrementRefCount/DecrementRefCount/PersistFileChunks
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
	blocks    []block.ChunkRef
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
// EXACT ID "{payloadID}/{offset}" (never by hash). The clone tests never reap,
// so this just satisfies the interface.
func (f *fakeCoordinator) DecrementRefCountAndReap(_ context.Context, payloadID string, offset uint64) (uint32, error) {
	f.reapIDs = append(f.reapIDs, fmt.Sprintf("%s/%d", payloadID, offset))
	return 0, nil
}

func (f *fakeCoordinator) PersistFileChunks(_ context.Context, payloadID string, blocks []block.ChunkRef, objectID block.ObjectID) error {
	f.persistCalls = append(f.persistCalls, persistCall{payloadID: payloadID, blocks: blocks, objectID: objectID})
	return nil
}

func (f *fakeCoordinator) GetPersistedBlocks(_ context.Context, _ string) ([]block.ChunkRef, error) {
	return nil, nil
}

// FindByObjectID stub. Adapter-common tests don't exercise short-circuit
// lookups (those live in pkg/block/engine and pkg/metadata/storetest);
// satisfy the interface so the fake satisfies engine.MetadataCoordinator.
func (f *fakeCoordinator) FindByObjectID(_ context.Context, _ block.ObjectID) ([]block.ChunkRef, error) {
	return nil, nil
}

// GetFileObjectID stub. Adapter-common tests do not drive the
// RemoteSync.Flush short-circuit path; returning the zero ObjectID + nil is
// the "never quiesced" disposition that keeps the interface satisfied
// without affecting any assertions.
func (f *fakeCoordinator) GetFileObjectID(_ context.Context, _ string) (block.ObjectID, error) {
	return block.ObjectID{}, nil
}

// ReprojectBlocks is a no-op: this fake does not model the Blocks
// projection.
func (f *fakeCoordinator) ReprojectBlocks(_ context.Context, _ string) error { return nil }

// DecrementRefCountAndReapMany loops the single-offset form so this double
// keeps whatever bookkeeping and error injection that form already carries.
func (f *fakeCoordinator) DecrementRefCountAndReapMany(ctx context.Context, payloadID string, offsets []uint64) error {
	for _, offset := range offsets {
		if _, err := f.DecrementRefCountAndReap(ctx, payloadID, offset); err != nil {
			return err
		}
	}
	return nil
}

// putTestFile creates a file with the given Blocks list in the metadata
// store and returns its handle. Used to seed src/dst before a clone.
func putTestFile(t *testing.T, ms metadata.Store, path string, payloadID metadata.PayloadID, blocks []block.ChunkRef, size uint64) metadata.FileHandle {
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

	if err := ms.UpdateAttrs(ctx, file); err != nil {
		t.Fatalf("UpdateAttrs(%s) failed: %v", path, err)
	}

	// Memory store derives the handle via EncodeShareHandle(ShareName, ID).
	// The same encoding is used in tx.UpdateAttrs, so a re-encoded handle here
	// matches the key the file was stored under.
	handle, err := metadata.EncodeShareHandle(file.ShareName, file.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle failed: %v", err)
	}
	return handle
}

// newCloneTestEngineWithMS constructs an engine wired against a caller-
// supplied MemoryMetadataStore so the test can both seed files and observe
// post-txn state via the same store.
func newCloneTestEngineWithMS(t *testing.T, coord *fakeCoordinator, ms *metadatamemory.MemoryMetadataStore) *engine.Store {
	t.Helper()
	bs, _ := newCloneTestEngineWithLocal(t, coord, ms)
	return bs
}

// newCloneTestEngineWithLocal is newCloneTestEngineWithMS with the journal-backed
// local tier handed back too, for the assertions that are about what the index
// describes rather than what the manifest holds.
func newCloneTestEngineWithLocal(t *testing.T, coord *fakeCoordinator, ms *metadatamemory.MemoryMetadataStore) (*engine.Store, *journal.Store) {
	t.Helper()

	tmpDir := t.TempDir()
	localStore, err := journal.Open(tmpDir, journal.Config{MaxLocalBytes: 100 * 1024 * 1024})
	if err != nil {
		t.Fatalf("journal.Open failed: %v", err)
	}

	// Wire a remote store so HasRemoteStore() is true: the O(1) manifest-row
	// reflink these tests assert is the remote-share path. A local-only share
	// (no remote) materializes real bytes instead — covered separately by the
	// journal-backed engine test.
	mem := remotememory.New()
	syncedHashStore, ok := metadata.Store(ms).(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	syncer := engine.NewRemoteSync(localStore, mem, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(syncedHashStore)
	syncer.SetRemoteBlockStore(mem)

	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          mem,
		RemoteSync:      syncer,
		FileChunkStore:  ms,
		Coordinator:     coord,
		SyncedHashStore: syncedHashStore,
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

	return bs, localStore
}
