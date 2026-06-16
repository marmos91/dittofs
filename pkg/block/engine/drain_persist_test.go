package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// testCoordinator is a faithful, test-local re-implementation of the
// production engine.MetadataCoordinator (pkg/controlplane/runtime/shares/
// coordinator.go) — that package may not be imported here per the
// strict-grep boundary. It drives PersistFileBlocks the exact way the
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
		return metadata.ErrFileBlockNotFound
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
		if errors.Is(err, metadata.ErrFileBlockNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

func (c *testCoordinator) PersistFileBlocks(ctx context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error {
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

func (c *testCoordinator) GetPersistedBlocks(ctx context.Context, payloadID string) ([]block.BlockRef, error) {
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

func (c *testCoordinator) FindByObjectID(ctx context.Context, objectID block.ObjectID) ([]block.BlockRef, error) {
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
	rollupStore, ok := ms.(metadata.RollupStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.RollupStore", ms)
	}
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 3_600_000, // 1h — async/ticker rollup can never fire
		RollupStore:     rollupStore,
		SyncedHashStore: syncedHashStore,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	coord := &testCoordinator{store: ms}
	syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Syncer:          syncer,
		FileBlockStore:  ms,
		Coordinator:     coord,
		SyncedHashStore: syncedHashStore,
		ReadBufferBytes: 64 * 1024 * 1024,
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

// fileBlocks reads FileAttr.Blocks for a handle via GetFile (which loads
// blocks from the per-file block manifest — GetFileByPayloadID does NOT on
// the Postgres backend).
func fileBlocks(t *testing.T, store metadata.Store, handle metadata.FileHandle) []block.BlockRef {
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
// Backup manifest exposes, plus the serialized manifest size.
func manifestHashes(t *testing.T, ms metadata.Store) (*block.HashSet, int) {
	t.Helper()
	backupable, ok := ms.(metadata.Backupable)
	if !ok {
		t.Fatal("store must implement metadata.Backupable")
	}
	var buf bytes.Buffer
	got, err := backupable.Backup(context.Background(), &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return got, buf.Len()
}

// runIdenticalContentDrain is the shared body for the identical-content
// (file-level dedup) BUG 2 scenario. Two files with byte-identical content
// roll up; the second trips the object_id uniqueness constraint. DrainRollups
// MUST succeed and BOTH files must persist their block lists (the duplicate
// without claiming the dedup pointer), so the manifest stays complete and
// both files are restorable.
func runIdenticalContentDrain(t *testing.T, ms metadata.Store, sharePrefix string) {
	t.Helper()
	ctx := context.Background()

	shareName := sharePrefix + "-dup"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverStore(t, ms)

	uniqueA := distinctContent(0x20, 3*1024*1024)
	uniqueB := distinctContent(0x21, 3*1024*1024)

	pidA, hA := createRealFile(t, ms, shareName, "alpha.bin", rootHandle)
	pidB, hB := createRealFile(t, ms, shareName, "beta.bin", rootHandle)
	pidC, hC := createRealFile(t, ms, shareName, "alpha-copy.bin", rootHandle)

	if _, err := bs.WriteAt(ctx, pidA, nil, uniqueA, 0); err != nil {
		t.Fatalf("WriteAt alpha: %v", err)
	}
	if _, err := bs.WriteAt(ctx, pidB, nil, uniqueB, 0); err != nil {
		t.Fatalf("WriteAt beta: %v", err)
	}
	if _, err := bs.WriteAt(ctx, pidC, nil, uniqueA, 0); err != nil {
		t.Fatalf("WriteAt alpha-copy: %v", err)
	}

	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups must tolerate identical-content conflict: %v", err)
	}

	want := block.NewHashSet(0)
	for name, h := range map[string]metadata.FileHandle{
		"alpha.bin":      hA,
		"beta.bin":       hB,
		"alpha-copy.bin": hC,
	} {
		blocks := fileBlocks(t, ms, h)
		if len(blocks) == 0 {
			t.Fatalf("file %s has empty FileAttr.Blocks after DrainRollups (unrestorable duplicate)", name)
		}
		for _, b := range blocks {
			want.Add(b.Hash)
		}
	}

	got, bufLen := manifestHashes(t, ms)
	missing := 0
	for _, h := range want.Sorted() {
		if !got.Contains(h) {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("Backup manifest missing %d/%d referenced hashes (manifest len=%d, buf=%d bytes)",
			missing, want.Len(), got.Len(), bufLen)
	}
}

// TestMemoryDrainRollups_IdenticalContent locks in BUG 2 on the in-memory
// metadata backend through the REAL engine write path.
func TestMemoryDrainRollups_IdenticalContent(t *testing.T) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	runIdenticalContentDrain(t, ms, "mem")
}

// TestBadgerDrainRollups_IdenticalContent locks in BUG 2 on the Badger
// metadata backend through the REAL engine write path.
func TestBadgerDrainRollups_IdenticalContent(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	runIdenticalContentDrain(t, ms, "badger")
}
