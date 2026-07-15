package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// testCoordinator is a faithful, test-local re-implementation of the
// production engine.MetadataCoordinator (pkg/controlplane/runtime/shares/
// coordinator.go) — that package may not be imported here per the
// strict-grep boundary. It drives PersistFileChunks the exact way the
// production wrapper does: resolve payloadID -> existing file row via
// GetFileByPayloadID, mutate Blocks + ObjectID, PutFile under the existing
// id, all in one metadata transaction, wrapping a backend object_id
// uniqueness conflict into engine.ErrObjectIDConflict.
type testCoordinator struct {
	store metadata.Store
}

var _ engine.MetadataCoordinator = (*testCoordinator)(nil)

func (c *testCoordinator) IncrementRefCount(ctx context.Context, hash block.ContentHash) error {
	fb, err := c.store.GetByHash(ctx, hash)
	if err != nil {
		return err
	}
	if fb == nil {
		return metadata.ErrFileChunkNotFound
	}
	return c.store.IncrementRefCount(ctx, fb.ID)
}

func (c *testCoordinator) DecrementRefCount(ctx context.Context, hash block.ContentHash) (uint32, error) {
	fb, err := c.store.GetByHash(ctx, hash)
	if err != nil {
		return 0, err
	}
	if fb == nil {
		return 0, nil
	}
	return c.store.DecrementRefCount(ctx, fb.ID)
}

func (c *testCoordinator) DecrementRefCountAndReap(ctx context.Context, payloadID string, offset uint64) (uint32, error) {
	// By EXACT ID — mirrors the production coordinator (no hash resolution).
	id := fmt.Sprintf("%s/%d", payloadID, offset)
	count, err := c.store.DecrementRefCountAndReap(ctx, id)
	if err != nil {
		if errors.Is(err, metadata.ErrFileChunkNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

func (c *testCoordinator) PersistFileChunks(ctx context.Context, payloadID string, blocks []block.ChunkRef, objectID block.ObjectID) error {
	return c.store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		file, err := tx.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
		if err != nil {
			return fmt.Errorf("coordinator: GetFileByPayloadID(%s): %w", payloadID, err)
		}
		if file == nil {
			return fmt.Errorf("coordinator: GetFileByPayloadID(%s): nil file (no row)", payloadID)
		}
		file.Blocks = blocks
		file.ObjectID = objectID
		if perr := tx.PutFile(ctx, file); perr != nil {
			return mapObjectIDConflict(perr)
		}
		return nil
	})
}

// mapObjectIDConflict mirrors the production runtime coordinator: an
// object_id uniqueness violation (Memory/Badger surface mderrors.ErrConflict;
// Postgres now maps the files_object_id_idx 23505 to the same ErrConflict
// code) is wrapped into engine.ErrObjectIDConflict so the rollup-persist
// path recognises the benign file-level-dedup conflict uniformly across
// backends.
func mapObjectIDConflict(err error) error {
	if err == nil {
		return nil
	}
	var storeErr *mderrors.StoreError
	if errors.As(err, &storeErr) && storeErr.Code == mderrors.ErrConflict {
		return errors.Join(engine.ErrObjectIDConflict, err)
	}
	return err
}

func (c *testCoordinator) GetPersistedBlocks(ctx context.Context, payloadID string) ([]block.ChunkRef, error) {
	file, err := c.store.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
	if err != nil {
		if metadata.IsNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}
	if file == nil {
		return nil, nil
	}
	if len(file.Blocks) > 0 {
		return file.Blocks, nil
	}
	// Memory returns blocks inline with a zero ID; an unresolvable zero-ID
	// handle means "no blocks yet". Postgres sets ID and omits blocks, so it
	// loads them via the handle fallback.
	if file.ID == uuid.Nil {
		return nil, nil
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		return nil, err
	}
	full, err := c.store.GetFile(ctx, handle)
	if err != nil {
		return nil, err
	}
	if full == nil {
		return nil, nil
	}
	return full.Blocks, nil
}

func (c *testCoordinator) FindByObjectID(ctx context.Context, objectID block.ObjectID) ([]block.ChunkRef, error) {
	if objectID.IsZero() {
		return nil, nil
	}
	return c.store.FindByObjectID(ctx, objectID)
}

func (c *testCoordinator) GetFileObjectID(ctx context.Context, payloadID string) (block.ObjectID, error) {
	var zero block.ObjectID
	file, err := c.store.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
	if err != nil {
		return zero, nil
	}
	if file == nil {
		return zero, nil
	}
	return file.ObjectID, nil
}

// newEngineOverStore builds a full engine.Store over a real fs local
// store + the supplied metadata store, wired with a faithful coordinator,
// and a LARGE stabilization window with the rollup worker pool NOT started
// so the only path from append-log bytes -> CAS + manifest is an explicit
// DrainRollups (the snapshot-create primitive).
func newEngineOverStore(t *testing.T, ms metadata.Store) *engine.Store {
	t.Helper()
	if _, ok := ms.(metadata.RollupStore); !ok {
		t.Fatalf("metadata store %T does not implement metadata.RollupStore", ms)
	}
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	coord := &testCoordinator{store: ms}
	// Chunking now happens at CARVE, which needs a wired remote block sink (the
	// journal has no local-only rollup). Use ManualSync so DrainRollups/Flush are
	// the deterministic carve drivers (no background dispatcher racing asserts).
	rem := remotememory.New()
	cfg := engine.DefaultConfig()
	cfg.ManualSync = true
	syncer := engine.NewSyncer(localStore, rem, ms, cfg)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          rem,
		Syncer:          syncer,
		FileChunkStore:  ms,
		Coordinator:     coord,
		SyncedHashStore: syncedHashStore,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	// Wire the carve substrate (SetSyncedHashStore ran inside engine.New; this
	// completes the deps so recomputeCarveActive wires the journal's sink).
	syncer.SetRemoteBlockStore(rem)
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// createRealFile creates a file row through the metadata store the way the
// production CreateFile path does — crucially setting a UUID-based PayloadID
// (buildPayloadID(share, id), see #1166 PR-3) so the rollup-persist
// GetFileByPayloadID lookup resolves to THIS row. It does NOT pre-populate
// Blocks (doing so would mask the rollup-persist bug under test).
func createRealFile(t *testing.T, store metadata.Store, shareName, name string, rootHandle metadata.FileHandle) (string, metadata.FileHandle) {
	t.Helper()
	ctx := context.Background()
	path := "/" + name
	handle, err := store.GenerateHandle(ctx, shareName, path)
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	payloadID := strings.TrimPrefix(shareName, "/") + "/" + id.String()
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			UID:       1000,
			GID:       1000,
			PayloadID: metadata.PayloadID(payloadID),
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile %q: %v", name, err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent %q: %v", name, err)
	}
	if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
		t.Fatalf("SetChild %q: %v", name, err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount %q: %v", name, err)
	}
	return payloadID, handle
}

// fileChunks reads FileAttr.Blocks for a handle via GetFile (which loads
// blocks from the per-file block manifest — GetFileByPayloadID does NOT on
// the Postgres backend).
func fileChunks(t *testing.T, store metadata.Store, handle metadata.FileHandle) []block.ChunkRef {
	t.Helper()
	f, err := store.GetFile(context.Background(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	return f.Blocks
}

func createShare(t *testing.T, store metadata.Store, shareName string) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	// CreateRootDirectory inserts BOTH the "/" files-row AND the share row
	// (Postgres' shares.root_file_id is NOT NULL, so the share row cannot be
	// created independently). It is idempotent on the share name.
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	return rootHandle
}

// distinctContent builds an n-byte buffer whose bytes derive from a per-test
// seed so that NO two test functions sharing the same metadata store produce
// colliding Merkle-root ObjectIDs (object_id dedup is store-wide, crossing
// share boundaries — see the CrossShareDedupScope conformance scenario).
// Within a single test, passing the same seed yields byte-identical content
// (used to drive the file-level-dedup conflict).
func distinctContent(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed ^ byte(i*31+17)
	}
	return b
}

// manifestHashes returns the set of DISTINCT content hashes the metadata
// snapshot manifest exposes, plus the serialized manifest size.
