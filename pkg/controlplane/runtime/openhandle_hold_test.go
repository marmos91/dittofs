package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeOpenFileSource is a test OpenFileEnumerator registered through the
// adapter-provider registry, exactly like the NFSv4 state manager and the
// SMB handler are in production.
type fakeOpenFileSource struct {
	handles [][]byte
	err     error
}

func (f *fakeOpenFileSource) EnumerateOpenFiles(_ context.Context, fn func(fileHandle []byte) error) error {
	if f.err != nil {
		return f.err
	}
	for _, h := range f.handles {
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// newOpenHoldRuntime builds a runtime with a single named share backed by an
// in-memory metadata store, sufficient for handle→share routing and
// GetMetadataStoreForShare resolution.
func newOpenHoldRuntime(t *testing.T, shareName string) (*Runtime, metadata.Store) {
	t.Helper()
	rt := New(nil)
	store := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          shareName,
		MetadataStore: "memory",
	})
	require.NoError(t, rt.storesSvc.RegisterMetadataStore("memory", store))
	require.NoError(t, rt.GetMetadataService().RegisterStoreForShare(shareName, store))
	return rt, store
}

// openHoldPutFile persists a file object with the supplied nlink, payload and
// blocks, returning its handle.
func openHoldPutFile(t *testing.T, store metadata.Store, shareName, path string, nlink uint32, payloadID string, blocks []block.BlockRef) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	h, err := store.GenerateHandle(ctx, shareName, path)
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(h)
	require.NoError(t, err)
	require.NoError(t, store.PutFile(ctx, &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			Nlink:     nlink,
			PayloadID: metadata.PayloadID(payloadID),
			Blocks:    blocks,
		},
	}))
	// The memory store derives Nlink from its internal link-count tracking
	// (defaulting to 1), so pin the requested value explicitly.
	require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, h, nlink)
	}))
	return h
}

// heldHashes runs the open-handle hold provider scoped to shareName and
// collects the emitted hashes.
func heldHashes(t *testing.T, rt *Runtime, shareName string) map[block.ContentHash]struct{} {
	t.Helper()
	p := &openHandleHoldProvider{rt: rt, shares: map[string]struct{}{shareName: {}}}
	got := make(map[block.ContentHash]struct{})
	require.NoError(t, p.HeldHashes(context.Background(), "remote-1", nil, func(h block.ContentHash) error {
		got[h] = struct{}{}
		return nil
	}))
	return got
}

// TestOpenHandleHold_OpenUnlinkedFileHeld is the core #1448 scenario: a file
// that is unlinked (nlink=0) while still open contributes its block hashes to
// the GC mark live set; once the last handle closes, the hold disappears.
func TestOpenHandleHold_OpenUnlinkedFileHeld(t *testing.T) {
	rt, store := newOpenHoldRuntime(t, "share")
	h1, h2 := hashAll(0x11), hashAll(0x22)
	fh := openHoldPutFile(t, store, "share", "/scratch.bin", 0, "p1", []block.BlockRef{
		{Hash: h1, Offset: 0, Size: 128},
		{Hash: h2, Offset: 128, Size: 128},
	})

	src := &fakeOpenFileSource{handles: [][]byte{fh}}
	rt.SetAdapterProvider("test_open_files", src)

	got := heldHashes(t, rt, "share")
	assert.Len(t, got, 2)
	assert.Contains(t, got, h1)
	assert.Contains(t, got, h2)

	// Last close: the enumerator no longer yields the handle → no hold.
	src.handles = nil
	assert.Empty(t, heldHashes(t, rt, "share"))
}

// TestOpenHandleHold_LinkedFileSkipped verifies still-linked files are not
// re-emitted by the hold: their hashes are already in the store live set.
func TestOpenHandleHold_LinkedFileSkipped(t *testing.T) {
	rt, store := newOpenHoldRuntime(t, "share")
	fh := openHoldPutFile(t, store, "share", "/linked.bin", 1, "p1", []block.BlockRef{
		{Hash: hashAll(0x33), Offset: 0, Size: 64},
	})
	rt.SetAdapterProvider("test_open_files", &fakeOpenFileSource{handles: [][]byte{fh}})
	assert.Empty(t, heldHashes(t, rt, "share"))
}

// TestOpenHandleHold_StaleAndForeignHandlesSkipped verifies that handles for
// purged files, unknown shares, and shares outside the GC scope are skipped
// without failing the mark phase.
func TestOpenHandleHold_StaleAndForeignHandlesSkipped(t *testing.T) {
	rt, store := newOpenHoldRuntime(t, "share")
	ctx := context.Background()

	// Handle whose file object does not exist (purged).
	stale, err := store.GenerateHandle(ctx, "share", "/gone.bin")
	require.NoError(t, err)

	// Handle for a share the runtime does not know.
	foreign, err := store.GenerateHandle(ctx, "othershare", "/f.bin")
	require.NoError(t, err)

	rt.SetAdapterProvider("test_open_files", &fakeOpenFileSource{handles: [][]byte{stale, foreign}})
	assert.Empty(t, heldHashes(t, rt, "share"))

	// Open-unlinked file in a share outside the provider's scope contributes
	// nothing to this share's pass.
	fh := openHoldPutFile(t, store, "share", "/scoped.bin", 0, "p2", []block.BlockRef{
		{Hash: hashAll(0x44), Offset: 0, Size: 64},
	})
	rt.SetAdapterProvider("test_open_files", &fakeOpenFileSource{handles: [][]byte{fh}})
	p := &openHandleHoldProvider{rt: rt, shares: map[string]struct{}{"unrelated": {}}}
	var emitted int
	require.NoError(t, p.HeldHashes(ctx, "remote-1", nil, func(block.ContentHash) error {
		emitted++
		return nil
	}))
	assert.Zero(t, emitted)
}

// TestOpenHandleHold_EnumeratorErrorFailsClosed verifies an enumerator
// failure aborts the mark phase instead of silently sweeping held blocks.
func TestOpenHandleHold_EnumeratorErrorFailsClosed(t *testing.T) {
	rt, _ := newOpenHoldRuntime(t, "share")
	boom := errors.New("adapter unavailable")
	rt.SetAdapterProvider("test_open_files", &fakeOpenFileSource{err: boom})
	p := &openHandleHoldProvider{rt: rt, shares: map[string]struct{}{"share": {}}}
	err := p.HeldHashes(context.Background(), "remote-1", nil, func(block.ContentHash) error { return nil })
	require.ErrorIs(t, err, boom)
}

// TestOpenHandleHold_CanceledContextStopsEarly verifies that a canceled GC
// pass aborts the open-handle enumeration promptly and propagates the context
// error, instead of iterating every open handle after cancellation.
func TestOpenHandleHold_CanceledContextStopsEarly(t *testing.T) {
	rt, store := newOpenHoldRuntime(t, "share")
	// An open-unlinked file that would otherwise be held.
	fh := openHoldPutFile(t, store, "share", "/scratch.bin", 0, "p1", []block.BlockRef{
		{Hash: hashAll(0x77), Offset: 0, Size: 64},
	})
	rt.SetAdapterProvider("test_open_files", &fakeOpenFileSource{handles: [][]byte{fh}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &openHandleHoldProvider{rt: rt, shares: map[string]struct{}{"share": {}}}
	emitted := 0
	err := p.HeldHashes(ctx, "remote-1", nil, func(block.ContentHash) error {
		emitted++
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, emitted, "a canceled pass must not emit held hashes")
}

// TestGetMetadataStoreForShare_UnknownShareIsErrShareNotFound guards the
// linchpin the open-handle hold relies on: a share removed mid-scan surfaces
// as an ErrShareNotFound-wrapped error, so forEachOpenUnlinkedFile can skip it
// rather than fail the whole GC pass closed.
func TestGetMetadataStoreForShare_UnknownShareIsErrShareNotFound(t *testing.T) {
	rt, _ := newOpenHoldRuntime(t, "share")
	_, err := rt.GetMetadataStoreForShare("ghost")
	require.ErrorIs(t, err, shares.ErrShareNotFound)
}

// TestOpenHandleHold_GCHoldForRemoteCombines verifies the combined provider
// wired into the GC options includes the open-handle contribution.
func TestOpenHandleHold_GCHoldForRemoteCombines(t *testing.T) {
	rt, store := newOpenHoldRuntime(t, "share")
	h := hashAll(0x55)
	fh := openHoldPutFile(t, store, "share", "/combined.bin", 0, "p3", []block.BlockRef{
		{Hash: h, Offset: 0, Size: 64},
	})
	rt.SetAdapterProvider("test_open_files", &fakeOpenFileSource{handles: [][]byte{fh}})

	hold := rt.gcHoldForRemote([]string{"share"})
	got := make(map[block.ContentHash]struct{})
	require.NoError(t, hold.HeldHashes(context.Background(), "remote-1", []string{"share"}, func(h block.ContentHash) error {
		got[h] = struct{}{}
		return nil
	}))
	assert.Contains(t, got, h)
}

// TestOpenHandleHold_CrossProtocolDedup verifies a file open via multiple
// protocol enumerators at once (e.g. NFSv4 + SMB) is visited exactly once.
func TestOpenHandleHold_CrossProtocolDedup(t *testing.T) {
	rt, store := newOpenHoldRuntime(t, "share")
	fh := openHoldPutFile(t, store, "share", "/both.bin", 0, "p4", []block.BlockRef{
		{Hash: hashAll(0x66), Offset: 0, Size: 64},
	})
	rt.SetAdapterProvider("test_nfs_open", &fakeOpenFileSource{handles: [][]byte{fh}})
	rt.SetAdapterProvider("test_smb_open", &fakeOpenFileSource{handles: [][]byte{fh}})

	visits := 0
	require.NoError(t, rt.forEachOpenUnlinkedFile(context.Background(),
		map[string]struct{}{"share": {}},
		func(string, *metadata.File) error {
			visits++
			return nil
		}))
	assert.Equal(t, 1, visits, "same handle from two enumerators must be visited once")
}

// TestReapStrandedRows_SkipsOpenUnlinkedPayloads verifies the stranded-row
// reconcile leaves the payload rows of an open-but-unlinked file intact (the
// open handle still reads through them) and reaps them after last close.
func TestReapStrandedRows_SkipsOpenUnlinkedPayloads(t *testing.T) {
	rt, store := newOpenHoldRuntime(t, "share")
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour)

	const heldPID = "held"
	fh := openHoldPutFile(t, store, "share", "/held.bin", 0, heldPID, nil)
	seedReconcileFB(t, ctx, store, heldPID, 2, old)
	seedReconcileFB(t, ctx, store, "stranded", 1, old)

	src := &fakeOpenFileSource{handles: [][]byte{fh}}
	rt.SetAdapterProvider("test_open_files", src)

	graceCutoff := time.Now().Add(-time.Hour)
	reaped, err := rt.reapStrandedRows(ctx, "share", store, graceCutoff, false)
	require.NoError(t, err)
	assert.Equal(t, 1, reaped, "only the truly stranded payload is reaped")

	rows, err := store.ListFileChunks(ctx, heldPID)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "open-but-unlinked payload rows must survive the reconcile")

	// Last close releases the hold: the next reconcile reaps.
	src.handles = nil
	reaped, err = rt.reapStrandedRows(ctx, "share", store, graceCutoff, false)
	require.NoError(t, err)
	assert.Equal(t, 2, reaped, "after last close the payload is reclaimed")
}
