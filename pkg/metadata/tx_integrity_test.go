package metadata_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// RemoveFile link-count TOCTOU (read inside the transaction)
// ============================================================================

// TestRemoveFile_HardLinkSurvives is the baseline: removing one of two hard
// links must decrement nlink to 1 and leave the content referenced (empty
// PayloadID return so the caller does not delete content).
func TestRemoveFile_HardLinkSurvives(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "a.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)

	target, err := fx.store.GetChild(ctx, fx.rootHandle, "a.txt")
	require.NoError(t, err)

	// Add a second link → nlink=2.
	_, hlErr := fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "b.txt", target)
	require.NoError(t, hlErr)

	// Removing one link drops nlink to 1, content stays referenced.
	removed, _, err := fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(1), removed.Nlink, "one surviving link expected")
	assert.Empty(t, removed.PayloadID, "content must not be eligible for deletion while a link remains")

	// The surviving link still resolves and reports nlink=1.
	survivor, err := fx.store.GetChild(ctx, fx.rootHandle, "b.txt")
	require.NoError(t, err)
	lc, err := fx.store.GetLinkCount(ctx, survivor)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), lc)
}

// TestRemoveFile_ConcurrentCreateHardLink stresses the TOCTOU window between
// the link-count read and the decrement. With the read now INSIDE the
// transaction, a concurrent CreateHardLink can never cause RemoveFile to drop
// nlink to 0 while a valid link still references the file. Run under -race.
//
// Invariant under test: after a RemoveFile of one link and a concurrent
// CreateHardLink of another, the remaining link's count is consistent with the
// links that actually exist in the directory (never 0 while a link is present).
func TestRemoveFile_ConcurrentCreateHardLink(t *testing.T) {
	t.Parallel()

	for iter := 0; iter < 200; iter++ {
		fx := newTestFixture(t)
		ctx := context.Background()

		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "orig.txt", &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)
		target, err := fx.store.GetChild(ctx, fx.rootHandle, "orig.txt")
		require.NoError(t, err)

		// Pre-create a second link so the file starts at nlink=2.
		_, hlErr := fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "link1.txt", target)
		require.NoError(t, hlErr)

		var wg sync.WaitGroup
		wg.Add(2)
		// Goroutine A: add a third link concurrently.
		go func() {
			defer wg.Done()
			_, _ = fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "link2.txt", target)
		}()
		// Goroutine B: remove the original link concurrently.
		go func() {
			defer wg.Done()
			_, _, _ = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "orig.txt")
		}()
		wg.Wait()

		// Count the links that actually still exist in the directory.
		var present int
		for _, name := range []string{"orig.txt", "link1.txt", "link2.txt"} {
			if _, gcErr := fx.store.GetChild(ctx, fx.rootHandle, name); gcErr == nil {
				present++
			}
		}
		// The file must still be referenced by the surviving links and its
		// stored nlink must equal the number of present directory entries —
		// never 0 while links exist.
		lc, lcErr := fx.store.GetLinkCount(ctx, target)
		require.NoError(t, lcErr)
		if present > 0 {
			assert.Equalf(t, uint32(present), lc,
				"iter %d: nlink=%d but %d links present (TOCTOU drop)", iter, lc, present)
		}
	}
}

// ============================================================================
// Move atomicity (full rollback on a mid-rename failure)
// ============================================================================

// errPutFileInjected is returned by the fault-injecting tx when PutFile is
// called for the targeted file ID.
var errPutFileInjected = errors.New("injected PutFile failure")

// faultyStore wraps a MetadataStore and, inside WithTransaction, makes
// tx.PutFile fail for a single targeted ShareName/Path. Everything else
// delegates to the real store so the rest of the rename runs normally up to
// the injected failure.
type faultyStore struct {
	metadata.MetadataStore
	failPath string // File.Path that should fail PutFile
}

func (f *faultyStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	return f.MetadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return fn(&faultyTx{Transaction: tx, failPath: f.failPath})
	})
}

type faultyTx struct {
	metadata.Transaction
	failPath string
}

func (t *faultyTx) PutFile(ctx context.Context, file *metadata.File) error {
	if file.Path == t.failPath {
		return errPutFileInjected
	}
	return t.Transaction.PutFile(ctx, file)
}

// TestMove_RollsBackOnPutFileFailure asserts the rename is atomic: when
// PutFile(srcFile) (the new-path write) fails mid-transaction, the whole Move
// rolls back — the source stays at its original name/path and the destination
// name is not created. Previously Move discarded these errors with `_ =` and
// committed a partial rename (entry relinked, File.Path stale).
func TestMove_RollsBackOnPutFileFailure(t *testing.T) {
	t.Parallel()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName := "/test"

	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	svc := metadata.New()
	// Register a store that fails PutFile for the moved file's NEW path.
	faulty := &faultyStore{MetadataStore: store, failPath: "/dest/moved.txt"}
	require.NoError(t, svc.RegisterStoreForShare(shareName, faulty))

	rootCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}

	_, _, err = svc.CreateFile(rootCtx, rootHandle, "myfile.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	_, _, err = svc.CreateDirectory(rootCtx, rootHandle, "dest", &metadata.FileAttr{Mode: 0755})
	require.NoError(t, err)
	destHandle, err := store.GetChild(ctx, rootHandle, "dest")
	require.NoError(t, err)

	srcHandle, err := store.GetChild(ctx, rootHandle, "myfile.txt")
	require.NoError(t, err)

	// The move must fail with the injected error.
	_, err = svc.Move(rootCtx, rootHandle, "myfile.txt", destHandle, "moved.txt")
	require.Error(t, err, "Move must surface the injected PutFile failure, not swallow it")
	require.ErrorIs(t, err, errPutFileInjected)

	// Full rollback: source still at its original name/path...
	stillThere, err := store.GetChild(ctx, rootHandle, "myfile.txt")
	require.NoError(t, err, "source entry must survive the rolled-back rename")
	assert.Equal(t, string(srcHandle), string(stillThere))

	srcFile, err := store.GetFile(ctx, srcHandle)
	require.NoError(t, err)
	assert.Equal(t, "/myfile.txt", srcFile.Path, "File.Path must not be left at the new (uncommitted) path")

	// ...and the destination name was NOT created.
	_, err = store.GetChild(ctx, destHandle, "moved.txt")
	require.Error(t, err, "destination entry must not exist after rollback")
}
