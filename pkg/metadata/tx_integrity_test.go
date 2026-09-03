package metadata_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
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

// errPutFileInjected is returned by the fault-injecting tx when UpdateAttrs is
// called for the targeted file ID.
var errPutFileInjected = errors.New("injected UpdateAttrs failure")

// errLinkCountInjected is returned by the fault-injecting tx when GetLinkCount
// is called for the targeted file ID.
var errLinkCountInjected = errors.New("injected GetLinkCount failure")

// faultyStore wraps a MetadataStore and, inside WithTransaction, injects
// targeted failures into tx.UpdateAttrs, tx.GetLinkCount and tx.GetChild — each
// keyed by its own field, so a test arms only the one it needs. Everything
// else delegates to the real store, so the operation under test runs normally
// up to the injected failure. The faults key on the inode ID rather than
// File.Path, because Path is always derived on read and so is not a reliable
// discriminator at call time.
type faultyStore struct {
	metadata.Store
	failID uuid.UUID // file ID whose UpdateAttrs should fail
	// failLinkCountID is the file ID whose in-transaction GetLinkCount fails.
	failLinkCountID uuid.UUID
	// getChild, when set, intercepts in-transaction GetChild lookups by name.
	// It returns the substituted result plus ok=true to override, or ok=false
	// to fall through to the real transaction. This simulates a concurrent
	// rename/unlink retargeting a name in the window between the advisory
	// pre-transaction read and the transaction body.
	getChild func(name string) (metadata.FileHandle, error, bool)
}

func (f *faultyStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	return f.Store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return fn(&faultyTx{
			Transaction:     tx,
			failID:          f.failID,
			failLinkCountID: f.failLinkCountID,
			getChild:        f.getChild,
		})
	})
}

type faultyTx struct {
	metadata.Transaction
	failID          uuid.UUID
	failLinkCountID uuid.UUID
	getChild        func(name string) (metadata.FileHandle, error, bool)
}

func (t *faultyTx) UpdateAttrs(ctx context.Context, file *metadata.File) error {
	if file.ID == t.failID {
		return errPutFileInjected
	}
	return t.Transaction.UpdateAttrs(ctx, file)
}

func (t *faultyTx) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if t.failLinkCountID != uuid.Nil {
		if _, id, err := metadata.DecodeFileHandle(handle); err == nil && id == t.failLinkCountID {
			return 0, errLinkCountInjected
		}
	}
	return t.Transaction.GetLinkCount(ctx, handle)
}

func (t *faultyTx) GetChild(ctx context.Context, dir metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if t.getChild != nil {
		if handle, err, ok := t.getChild(name); ok {
			return handle, err
		}
	}
	return t.Transaction.GetChild(ctx, dir, name)
}

// faultyFixture is newTestFixture with the store wrapped so faults can be
// injected into the transaction body. Callers arm the fault just before the
// operation under test so setup writes run unimpeded.
type faultyFixture struct {
	store  *memory.MemoryMetadataStore
	faulty *faultyStore
	svc    *metadata.Service
	root   metadata.FileHandle
	ctx    *metadata.AuthContext
}

func newFaultyFixture(t *testing.T) *faultyFixture {
	t.Helper()

	base := newTestFixture(t)
	faulty := &faultyStore{Store: base.store}
	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(base.shareName, faulty))

	return &faultyFixture{
		store:  base.store,
		faulty: faulty,
		svc:    svc,
		root:   base.rootHandle,
		ctx:    base.rootContext(),
	}
}

// TestMove_RollsBackOnPutFileFailure asserts the rename is atomic: when the
// UpdateAttrs(srcFile) ctime write fails mid-transaction, the whole Move rolls
// back — the source stays at its original name and the destination name is not
// created. Previously Move discarded these errors with `_ =` and committed a
// partial rename (entry relinked but the inode write lost).
func TestMove_RollsBackOnPutFileFailure(t *testing.T) {
	t.Parallel()
	fx := newFaultyFixture(t)
	ctx := context.Background()

	_, _, err := fx.svc.CreateFile(fx.ctx, fx.root, "myfile.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	_, _, err = fx.svc.CreateDirectory(fx.ctx, fx.root, "dest", &metadata.FileAttr{Mode: 0755})
	require.NoError(t, err)
	destHandle, err := fx.store.GetChild(ctx, fx.root, "dest")
	require.NoError(t, err)

	srcHandle, err := fx.store.GetChild(ctx, fx.root, "myfile.txt")
	require.NoError(t, err)
	_, srcID, err := metadata.DecodeFileHandle(srcHandle)
	require.NoError(t, err)

	// Arm the fault on the moved inode's ctime UpdateAttrs. It is keyed on the
	// inode ID (stable across the rename) rather than File.Path, which Move no
	// longer mutates/persists (#1166).
	fx.faulty.failID = srcID

	// The move must fail with the injected error.
	_, _, err = fx.svc.Move(fx.ctx, fx.root, "myfile.txt", destHandle, "moved.txt")
	require.ErrorIs(t, err, errPutFileInjected, "Move must surface the injected UpdateAttrs failure, not swallow it")

	// Full rollback: source still at its original name/path...
	stillThere, err := fx.store.GetChild(ctx, fx.root, "myfile.txt")
	require.NoError(t, err, "source entry must survive the rolled-back rename")
	assert.Equal(t, string(srcHandle), string(stillThere))

	srcFile, err := fx.store.GetFile(ctx, srcHandle)
	require.NoError(t, err)
	assert.Equal(t, "/myfile.txt", srcFile.Path, "File.Path must not be left at the new (uncommitted) path")

	// ...and the destination name was NOT created.
	_, err = fx.store.GetChild(ctx, destHandle, "moved.txt")
	require.Error(t, err, "destination entry must not exist after rollback")
}

// ============================================================================
// In-transaction rechecks of state read before the transaction
// ============================================================================

// TestCreateDirectory_AbortsOnParentLinkCountFailure asserts that a failed
// parent GetLinkCount rolls the whole mkdir back. Previously the error was
// discarded and the transaction still committed the new directory and its
// parent edge, leaving the parent's ".." link count permanently one short.
func TestCreateDirectory_AbortsOnParentLinkCountFailure(t *testing.T) {
	t.Parallel()
	fx := newFaultyFixture(t)
	ctx := context.Background()

	_, rootID, err := metadata.DecodeFileHandle(fx.root)
	require.NoError(t, err)

	// Arm the fault on the parent's link-count read.
	fx.faulty.failLinkCountID = rootID

	_, _, err = fx.svc.CreateDirectory(fx.ctx, fx.root, "sub", &metadata.FileAttr{Mode: 0755})
	require.ErrorIs(t, err, errLinkCountInjected, "mkdir must surface the parent link-count read failure")

	// Full rollback: the directory entry was not created.
	_, err = fx.store.GetChild(ctx, fx.root, "sub")
	require.Error(t, err, "the new directory must not survive the rolled-back mkdir")
}

// TestMove_AbortsWhenSourceEntryChangedInTransaction asserts Move re-resolves
// the source name inside the transaction. The pre-transaction GetChild is
// advisory: a concurrent rename or unlink can drop the name in the gap, and
// without the recheck Move would relink the stale handle under the destination
// name, resurrecting an entry that no longer exists.
func TestMove_AbortsWhenSourceEntryChangedInTransaction(t *testing.T) {
	t.Parallel()
	fx := newFaultyFixture(t)
	ctx := context.Background()

	_, _, err := fx.svc.CreateFile(fx.ctx, fx.root, "myfile.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)

	// The source name vanishes between the advisory read and the transaction.
	fx.faulty.getChild = func(name string) (metadata.FileHandle, error, bool) {
		if name == "myfile.txt" {
			return nil, &metadata.StoreError{Code: metadata.ErrNotFound, Message: "concurrently unlinked"}, true
		}
		return nil, nil, false
	}

	_, _, err = fx.svc.Move(fx.ctx, fx.root, "myfile.txt", fx.root, "moved.txt")
	require.Error(t, err, "Move must abort when the source entry changed inside the transaction")

	// The destination name was never created.
	_, err = fx.store.GetChild(ctx, fx.root, "moved.txt")
	require.Error(t, err, "destination entry must not exist after the aborted rename")
}

// TestMove_AbortsWhenDestinationAppearedInTransaction asserts Move re-resolves
// the destination name inside the transaction. Two renames onto the same
// destination both saw it absent before their transactions; without the
// recheck both SetChild and the loser's inode is orphaned.
func TestMove_AbortsWhenDestinationAppearedInTransaction(t *testing.T) {
	t.Parallel()
	fx := newFaultyFixture(t)
	ctx := context.Background()

	_, _, err := fx.svc.CreateFile(fx.ctx, fx.root, "myfile.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	_, _, err = fx.svc.CreateFile(fx.ctx, fx.root, "winner.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)

	srcHandle, err := fx.store.GetChild(ctx, fx.root, "myfile.txt")
	require.NoError(t, err)
	winner, err := fx.store.GetChild(ctx, fx.root, "winner.txt")
	require.NoError(t, err)

	// A racing rename claimed the destination name after the advisory read
	// reported it free.
	fx.faulty.getChild = func(name string) (metadata.FileHandle, error, bool) {
		if name == "moved.txt" {
			return winner, nil, true
		}
		return nil, nil, false
	}

	_, _, err = fx.svc.Move(fx.ctx, fx.root, "myfile.txt", fx.root, "moved.txt")
	require.Error(t, err, "Move must abort when the destination appeared inside the transaction")
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, metadata.ErrConflict, storeErr.Code)

	// The source entry survives the aborted rename.
	stillThere, err := fx.store.GetChild(ctx, fx.root, "myfile.txt")
	require.NoError(t, err, "source entry must survive the aborted rename")
	assert.Equal(t, string(srcHandle), string(stillThere))
}

// TestRemoveFile_LinkCountReadFailureAborts pins that an unreadable link count
// aborts the remove instead of being guessed as "last link". Guessing reports
// the content free (a non-empty PayloadID the caller reaps) while a second hard
// link still references it, so the blocks vanish out from under that link.
func TestRemoveFile_LinkCountReadFailureAborts(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	faulty := &faultyStore{Store: fx.store}
	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(fx.shareName, faulty))
	authCtx := fx.rootContext()

	_, _, err := svc.CreateFile(authCtx, fx.rootHandle, "a.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	target, err := fx.store.GetChild(ctx, fx.rootHandle, "a.txt")
	require.NoError(t, err)
	_, err = svc.CreateHardLink(authCtx, fx.rootHandle, "b.txt", target)
	require.NoError(t, err)

	_, targetID, err := metadata.DecodeFileHandle(target)
	require.NoError(t, err)

	// Arm the fault on the removed inode's link-count read, after setup.
	faulty.failLinkCountID = targetID

	removed, _, err := svc.RemoveFile(authCtx, fx.rootHandle, "a.txt")
	require.ErrorIs(t, err, errLinkCountInjected, "an unreadable link count must abort the remove")
	assert.Nil(t, removed, "no file record may be returned when the remove aborted")

	// The entry survives, so the second link still resolves the content the
	// caller would otherwise have reaped.
	faulty.failLinkCountID = uuid.Nil
	_, err = fx.store.GetChild(ctx, fx.rootHandle, "a.txt")
	require.NoError(t, err, "the entry must survive an aborted remove")
	lc, err := fx.store.GetLinkCount(ctx, target)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), lc, "link count must be untouched by the aborted remove")
}
