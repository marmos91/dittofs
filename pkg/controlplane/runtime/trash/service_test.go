package trash

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test harness
// ============================================================================

// stubTrashPolicy enables trash for every share so MetadataService recycles on
// RemoveFile/RemoveDirectory instead of destroying content.
type stubTrashPolicy struct {
	cfg metadata.TrashConfig
}

func (p stubTrashPolicy) TrashConfigForShare(string) (metadata.TrashConfig, bool) {
	return p.cfg, true
}

// testDeps is an in-test implementation of Deps backed by a single real
// in-memory MetadataService. FreeBlocks records the payloadIDs it is asked to
// free so tests can assert block reclamation happens for permanent deletes.
type testDeps struct {
	shareName  string
	svc        *metadata.MetadataService
	rootHandle metadata.FileHandle
	cfg        Config
	freed      []string
}

func (d *testDeps) MetadataServiceForShare(shareName string) (*metadata.MetadataService, metadata.FileHandle, bool) {
	if shareName != d.shareName {
		return nil, nil, false
	}
	return d.svc, d.rootHandle, true
}

func (d *testDeps) TrashConfigForShare(shareName string) (Config, bool) {
	if shareName != d.shareName {
		return Config{}, false
	}
	return d.cfg, true
}

func (d *testDeps) EnabledTrashShares() []string {
	return []string{d.shareName}
}

func (d *testDeps) FreeBlocks(_ context.Context, _ string, _ metadata.FileHandle, payloadID string) error {
	if payloadID == "" {
		return nil
	}
	d.freed = append(d.freed, payloadID)
	return nil
}

// trashTest bundles the service under test, its deps, and the auth context the
// subtests act as.
type trashTest struct {
	t    *testing.T
	svc  *Service
	deps *testDeps
	ctx  *metadata.AuthContext
}

// newTestTrash builds a real in-memory MetadataService with trash enabled
// (mirroring pkg/metadata/file_recycle_test.go's newRecycleFixture ordering:
// CreateShare BEFORE CreateRootDirectory + SetTrashPolicy), wraps it in a Deps
// impl, and constructs the trash.Service over it.
func newTestTrash(t *testing.T) *trashTest {
	t.Helper()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	bg := context.Background()
	shareName := "/test"

	require.NoError(t, store.CreateShare(bg, &metadata.Share{Name: shareName}))
	rootFile, err := store.CreateRootDirectory(bg, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	msvc := metadata.New()
	require.NoError(t, msvc.RegisterStoreForShare(shareName, store))
	msvc.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

	deps := &testDeps{
		shareName:  shareName,
		svc:        msvc,
		rootHandle: rootHandle,
		cfg:        Config{Enabled: true},
	}

	return &trashTest{
		t:    t,
		svc:  New(deps, 0),
		deps: deps,
		ctx:  rootAuthContext(),
	}
}

// rootAuthContext returns a root (uid/gid 0) AuthContext.
func rootAuthContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
}

// createFile creates a regular file under parent.
func (tt *trashTest) createFile(parent metadata.FileHandle, name string) metadata.FileHandle {
	tt.t.Helper()
	_, err := tt.deps.svc.CreateFile(tt.ctx, parent, name, &metadata.FileAttr{Mode: 0o644})
	require.NoError(tt.t, err)
	h, err := tt.deps.svc.GetChild(tt.ctx.Context, parent, name)
	require.NoError(tt.t, err)
	return h
}

// mkdir creates a directory under parent and returns its handle.
func (tt *trashTest) mkdir(parent metadata.FileHandle, name string) metadata.FileHandle {
	tt.t.Helper()
	dir, err := tt.deps.svc.CreateDirectory(tt.ctx, parent, name, &metadata.FileAttr{Mode: 0o755})
	require.NoError(tt.t, err)
	h, err := metadata.EncodeFileHandle(dir)
	require.NoError(tt.t, err)
	return h
}

// recycle creates a file at the share root and RemoveFile's it so it lands in
// the bin, then returns the recycled entry's BinPath (its name under #recycle).
func (tt *trashTest) recycle(name string) {
	tt.t.Helper()
	tt.createFile(tt.deps.rootHandle, name)
	_, err := tt.deps.svc.RemoveFile(tt.ctx, tt.deps.rootHandle, name)
	require.NoError(tt.t, err)
}

// recycleAt creates a file at share-relative dir/name (creating dir if needed)
// and RemoveFile's it so it lands in the bin under #recycle/dir/name, exercising
// the recreated intermediary-parent chain.
func (tt *trashTest) recycleAt(dir, name string) {
	tt.t.Helper()
	parent := tt.deps.rootHandle
	if dir != "" {
		if h, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, dir); err == nil {
			parent = h
		} else {
			parent = tt.mkdir(tt.deps.rootHandle, dir)
		}
	}
	tt.createFile(parent, name)
	_, err := tt.deps.svc.RemoveFile(tt.ctx, parent, name)
	require.NoError(tt.t, err)
}

// binHandle resolves the share's #recycle directory handle.
func (tt *trashTest) binHandle() metadata.FileHandle {
	tt.t.Helper()
	h, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, metadata.RecycleDirName)
	require.NoError(tt.t, err)
	return h
}

// isAlreadyExists reports whether err is a StoreError with the AlreadyExists
// code. The metadata package surfaces "destination exists" as a coded
// StoreError rather than a sentinel error value.
func isAlreadyExists(err error) bool {
	var se *metadata.StoreError
	return stderrors.As(err, &se) && se.Code == metadata.ErrAlreadyExists
}

// ============================================================================
// Tests
// ============================================================================

func TestRestoreClearsDeletionMetadata(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("doc.txt")

	require.NoError(t, tt.svc.Restore(tt.ctx, tt.deps.shareName, "doc.txt", ""))

	// Restored file is live at its original path with deletion metadata cleared.
	restored, err := tt.deps.svc.Lookup(tt.ctx, tt.deps.rootHandle, "doc.txt")
	require.NoError(t, err)
	assert.Nil(t, restored.DeletedAt, "restored file must not be marked deleted")
	assert.Empty(t, restored.OriginalPath)
	assert.Empty(t, restored.DeletedBy)

	// The bin no longer holds it.
	_, err = tt.deps.svc.Lookup(tt.ctx, tt.binHandle(), "doc.txt")
	assert.True(t, metadata.IsNotFoundError(err))
}

func TestRestoreConflictReturnsErrExist(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("doc.txt")
	// Recreate a live "doc.txt" at the root: restore must not clobber it.
	tt.createFile(tt.deps.rootHandle, "doc.txt")

	err := tt.svc.Restore(tt.ctx, tt.deps.shareName, "doc.txt", "")
	require.Error(t, err)
	assert.True(t, isAlreadyExists(err), "expected AlreadyExists, got %v", err)

	// The recycled copy is left untouched in the bin.
	binned, err := tt.deps.svc.Lookup(tt.ctx, tt.binHandle(), "doc.txt")
	require.NoError(t, err)
	require.NotNil(t, binned.DeletedAt)
}

func TestListReturnsRecycledRoots(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("a.txt")
	tt.recycle("b.txt")

	// A non-empty directory subtree recycles as ONE entry.
	dir := tt.mkdir(tt.deps.rootHandle, "docs")
	tt.createFile(dir, "inner.txt")
	require.NoError(t, tt.deps.svc.RemoveDirectory(tt.ctx, tt.deps.rootHandle, "docs"))

	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	require.Len(t, entries, 3, "expected 2 files + 1 subtree root, got %+v", entries)

	byOrig := make(map[string]Entry, len(entries))
	for _, e := range entries {
		assert.False(t, e.DeletedAt.IsZero(), "entry %q missing DeletedAt", e.BinPath)
		byOrig[e.OriginalPath] = e
	}
	require.Contains(t, byOrig, "a.txt")
	require.Contains(t, byOrig, "b.txt")
	require.Contains(t, byOrig, "docs")
	assert.True(t, byOrig["docs"].IsDir, "docs should be reported as a directory")
	assert.False(t, byOrig["a.txt"].IsDir)

	// The subtree child must NOT appear as its own top-level entry.
	for _, e := range entries {
		assert.NotEqual(t, "docs/inner.txt", e.OriginalPath)
	}
}

func TestEmptyRemovesAllBinEntries(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("a.txt")
	tt.recycle("b.txt")

	n, err := tt.svc.Empty(tt.ctx, tt.deps.shareName, false)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// The bin is empty afterwards.
	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.Empty(t, entries)

	// FreeBlocks was invoked once per permanently-removed file.
	assert.Len(t, tt.deps.freed, 2, "each emptied file should free its blocks")
}

func TestListUnknownShareReturnsNotFound(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	_, err := tt.svc.List(tt.ctx, "/nope")
	assert.True(t, metadata.IsNotFoundError(err))
}

// TestListReturnsNestedEntries covers two files recycled from a shared original
// subdir: they land under #recycle/a/file1.txt and #recycle/a/file2.txt. List
// must return BOTH as entries (with the nested BinPath) and must NOT report the
// recreated intermediary "a" dir as its own entry.
func TestListReturnsNestedEntries(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycleAt("a", "file1.txt")
	tt.recycleAt("a", "file2.txt")

	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	require.Len(t, entries, 2, "expected both nested files, got %+v", entries)

	byBin := make(map[string]Entry, len(entries))
	for _, e := range entries {
		byBin[e.BinPath] = e
	}
	require.Contains(t, byBin, "a/file1.txt")
	require.Contains(t, byBin, "a/file2.txt")
	assert.Equal(t, "a/file1.txt", byBin["a/file1.txt"].OriginalPath)
	assert.Equal(t, "a/file2.txt", byBin["a/file2.txt"].OriginalPath)

	// The intermediary "a" directory must never be listed as an entry.
	for _, e := range entries {
		assert.NotEqual(t, "a", e.BinPath, "intermediary dir must not be an entry")
		assert.False(t, e.IsDir, "nested entries are files, not the parent dir")
	}
}

// TestEmptyPrunesOrphanIntermediaryDirs verifies that after recycling a file
// from a deep original path and then Empty, the recreated intermediary "a"
// directory is swept out of #recycle (no orphan left behind), while #recycle
// itself remains.
func TestEmptyPrunesOrphanIntermediaryDirs(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycleAt("a", "b.txt")

	// Sanity: the intermediary "a" dir exists under #recycle before Empty.
	_, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.binHandle(), "a")
	require.NoError(t, err, "intermediary dir should exist before Empty")

	n, err := tt.svc.Empty(tt.ctx, tt.deps.shareName, false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// No orphan "a" dir remains under #recycle.
	_, err = tt.deps.svc.GetChild(tt.ctx.Context, tt.binHandle(), "a")
	assert.True(t, metadata.IsNotFoundError(err), "orphan intermediary dir must be pruned")

	// The bin is logically empty.
	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.Empty(t, entries)

	// #recycle root itself survives (only OnDisable removes it).
	_, err = tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, metadata.RecycleDirName)
	assert.NoError(t, err, "#recycle root must survive Empty")
}
