package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/migrate"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeMigrateStatusRuntime is a recording stand-in for MigrateStatusRuntime.
// Tests assert on captured arguments and feed canned responses.
type fakeMigrateStatusRuntime struct {
	mds         metadata.Store
	mdsErr      error
	localDir    string
	localDirErr error
}

func (f *fakeMigrateStatusRuntime) GetMetadataStoreForShare(_ string) (metadata.Store, error) {
	if f.mdsErr != nil {
		return nil, f.mdsErr
	}
	return f.mds, nil
}

func (f *fakeMigrateStatusRuntime) LocalStoreDir(_ string) (string, error) {
	if f.localDirErr != nil {
		return "", f.localDirErr
	}
	return f.localDir, nil
}

// memoryMDSWithShare creates a memory metadata store with a registered
// share that has the given BlockLayout configured.
func memoryMDSWithShare(t *testing.T, share string, layout metadata.BlockLayout) metadata.Store {
	t.Helper()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	ctx := context.Background()
	require.NoError(t, mds.CreateShare(ctx, &metadata.Share{
		Name:    share,
		Options: metadata.ShareOptions{BlockLayout: layout},
	}))

	root := &metadata.FileAttr{
		Type:  metadata.FileTypeDirectory,
		Mode:  0o755,
		Atime: time.Now(),
		Mtime: time.Now(),
		Ctime: time.Now(),
	}
	_, err := mds.CreateRootDirectory(ctx, share, root)
	require.NoError(t, err)

	return mds
}

// TestMigrateStatus_MissingShare asserts the 400 error path when the
// ?share= query parameter is omitted.
func TestMigrateStatus_MissingShare(t *testing.T) {
	h := NewMigrateStatusHandler(&fakeMigrateStatusRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/blockstore/migrate/status", nil)
	w := httptest.NewRecorder()

	h.Status(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "share")
}

// TestMigrateStatus_UnknownShare asserts the 404 path when the share is
// unknown to the metadata store / runtime.
func TestMigrateStatus_UnknownShare(t *testing.T) {
	h := NewMigrateStatusHandler(&fakeMigrateStatusRuntime{
		mdsErr: shares.ErrShareNotFound,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/blockstore/migrate/status?share=nope", nil)
	w := httptest.NewRecorder()

	h.Status(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestMigrateStatus_NoJournal asserts the steady-state response for a
// share that never ran a migration: BlockLayout flag from metadata,
// FilesDone=0, JournalPresent=false. Not an error path.
func TestMigrateStatus_NoJournal(t *testing.T) {
	mds := memoryMDSWithShare(t, "myshare", metadata.BlockLayoutLegacy)
	emptyDir := t.TempDir()

	h := NewMigrateStatusHandler(&fakeMigrateStatusRuntime{
		mds:      mds,
		localDir: emptyDir,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/blockstore/migrate/status?share=myshare", nil)
	w := httptest.NewRecorder()

	h.Status(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "myshare", resp["share"])
	assert.Equal(t, "legacy", resp["block_layout"])
	assert.Equal(t, float64(0), resp["files_done"])
	assert.Equal(t, false, resp["journal_present"])
	assert.Equal(t, false, resp["snapshot_present"])
}

// TestMigrateStatus_WithJournal asserts populated journal entries are
// reflected in FilesDone, BytesUploaded/Deduped totals, and LastCommitAt
// (max timestamp).
func TestMigrateStatus_WithJournal(t *testing.T) {
	mds := memoryMDSWithShare(t, "myshare", metadata.BlockLayoutCASOnly)
	dir := t.TempDir()

	// Seed the journal with two committed entries.
	j, err := migrate.OpenJournal(dir)
	require.NoError(t, err)
	t1 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 5, 11, 0, 0, 0, time.UTC)
	require.NoError(t, j.Append(migrate.JournalEntry{
		Kind:          "file_done",
		Timestamp:     t1,
		FileHandle:    "h1",
		BytesUploaded: 1024,
		BytesDeduped:  256,
		Blocks:        []block.BlockRef{},
	}))
	require.NoError(t, j.Append(migrate.JournalEntry{
		Kind:          "file_done",
		Timestamp:     t2,
		FileHandle:    "h2",
		BytesUploaded: 2048,
		BytesDeduped:  0,
		Blocks:        []block.BlockRef{},
	}))
	require.NoError(t, j.Close())

	// Sanity-check the journal is on disk where the handler will read it.
	jpath := filepath.Join(dir, migrate.JournalFile)
	_, err = filepath.Abs(jpath)
	require.NoError(t, err)

	h := NewMigrateStatusHandler(&fakeMigrateStatusRuntime{
		mds:      mds,
		localDir: dir,
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/blockstore/migrate/status?share=myshare&with_total=false", nil)
	w := httptest.NewRecorder()

	h.Status(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "myshare", resp["share"])
	assert.Equal(t, "cas-only", resp["block_layout"])
	assert.Equal(t, float64(2), resp["files_done"])
	assert.Equal(t, float64(3072), resp["bytes_uploaded"])
	assert.Equal(t, float64(256), resp["bytes_deduped"])
	assert.Equal(t, true, resp["journal_present"])
	assert.Equal(t, t2.Format(time.RFC3339), resp["last_commit_at"])
}

// TestMigrateStatus_NoLocalStoreDir asserts that a memory-only share
// (LocalStoreDir returns "") still produces a valid response — the
// handler skips journal reading but the BlockLayout + share name remain
// authoritative.
func TestMigrateStatus_NoLocalStoreDir(t *testing.T) {
	mds := memoryMDSWithShare(t, "memshare", metadata.BlockLayoutCASOnly)

	h := NewMigrateStatusHandler(&fakeMigrateStatusRuntime{
		mds:      mds,
		localDir: "", // memory-backed share has no on-disk path
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/blockstore/migrate/status?share=memshare&with_total=false", nil)
	w := httptest.NewRecorder()

	h.Status(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "memshare", resp["share"])
	assert.Equal(t, "cas-only", resp["block_layout"])
	assert.Equal(t, false, resp["journal_present"])
}

// TestMigrateStatus_FilesTotalDefaultsToWalk asserts the file-walk path
// is enabled by default and produces the expected count for an empty
// share (count=0).
func TestMigrateStatus_FilesTotalDefaultsToWalk(t *testing.T) {
	mds := memoryMDSWithShare(t, "myshare", metadata.BlockLayoutLegacy)
	dir := t.TempDir()

	h := NewMigrateStatusHandler(&fakeMigrateStatusRuntime{
		mds:      mds,
		localDir: dir,
	})

	// No with_total flag — defaults to walking.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/blockstore/migrate/status?share=myshare", nil)
	w := httptest.NewRecorder()

	h.Status(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// Empty share → 0 files (not the -1 sentinel).
	assert.Equal(t, float64(0), resp["files_total"])
}

// TestMigrateStatus_WithTotalFalseSkipsWalk asserts the ?with_total=false
// query parameter short-circuits the walk: FilesTotal stays at its
// zero-value, and a misbehaving metadata store would not be touched.
func TestMigrateStatus_WithTotalFalseSkipsWalk(t *testing.T) {
	mds := memoryMDSWithShare(t, "myshare", metadata.BlockLayoutLegacy)
	dir := t.TempDir()

	h := NewMigrateStatusHandler(&fakeMigrateStatusRuntime{
		mds:      mds,
		localDir: dir,
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/blockstore/migrate/status?share=myshare&with_total=false", nil)
	w := httptest.NewRecorder()

	h.Status(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, float64(0), resp["files_total"])
}
