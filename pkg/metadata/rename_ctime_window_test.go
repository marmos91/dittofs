package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

// windowStore is a store that runs beforeTx once, immediately before the next
// transaction it opens. Service.Move reads the source inode outside any
// transaction and then opens one to stamp it; the hook lands exactly in that
// gap, which is the window an in-transaction re-read exists to cover.
//
// Both transaction entry points are overridden, so the relaxed path Move takes
// cannot bypass the hook. The hook fires once per call, before any attempt; a
// conflict retry re-runs the closure inside the same call without re-firing
// it. Nothing here touches production code: it works only because
// RegisterStoreForShare accepts the metadata.Store interface.
type windowStore struct {
	*badger.BadgerMetadataStore
	beforeTx func()
}

// fire runs and clears the hook. Cleared before it runs: the hook itself
// commits through this store, and that inner transaction must not re-enter it.
func (w *windowStore) fire() {
	if hook := w.beforeTx; hook != nil {
		w.beforeTx = nil
		hook()
	}
}

func (w *windowStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	w.fire()
	return w.BadgerMetadataStore.WithTransaction(ctx, fn)
}

func (w *windowStore) WithTransactionRelaxed(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	w.fire()
	return w.BadgerMetadataStore.WithTransactionRelaxed(ctx, fn)
}

// TestRenameCtime_AdvanceInsideMoveWindowIsNotErased pins that SourcePreCtime is
// read inside Move's transaction rather than from the copy Move took before
// opening it.
//
// Those two reads return identical bytes unless a writer commits between them,
// so the distinction is invisible to any test that cannot place a write in that
// gap — which is why a test that merely advances the ChangeTime before calling
// Move pins nothing. This one places it exactly, by hooking the store's
// transaction entry. With the pre-rename value taken from the outside-tx read,
// the advance committed in the window is erased and the ChangeTime lands back at
// the pre-advance value.
//
// What closes this window on badger is the store's isolation (a conflicting
// commit aborts and retries the transaction), not anything about how Move is
// written.
func TestRenameCtime_AdvanceInsideMoveWindowIsNotErased(t *testing.T) {
	ws := &windowStore{BadgerMetadataStore: newRenameStore(t)}
	svc, rootHandle, share := registerRenameStore(t, ws)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "w.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	c0, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)

	var mid *metadata.File
	ws.beforeTx = func() {
		advanced := c0.Ctime.Add(90 * time.Second)
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Ctime: &advanced})
		require.NoError(t, hookErr)
		mid, hookErr = svc.GetFile(root.Context, handle)
		require.NoError(t, hookErr)
	}

	_, wcc, err := svc.Move(root, rootHandle, "w.bin", rootHandle, "x.bin")
	require.NoError(t, err)
	require.NotNil(t, wcc)
	require.NotNil(t, mid, "hook did not fire: Move opened no transaction through the wrapper")
	require.True(t, mid.Ctime.After(c0.Ctime), "precondition: the injected advance must have landed")

	require.NoError(t, svc.RestoreChangeTimeIfUnchanged(
		root.Context, handle, wcc.SourceCtime, wcc.SourcePreCtime))

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.True(t, after.Ctime.Equal(mid.Ctime),
		"ChangeTime = %v; want the advance %v that committed inside the rename's own window "+
			"(pre-rename value was %v) — the pre-read came from outside the transaction and erased it",
		after.Ctime.UTC(), mid.Ctime.UTC(), c0.Ctime.UTC())
}

// TestRenameCtime_SizeCommittedInMoveWindowSurvives pins that Move writes the
// row its own transaction read, not the copy it took before opening one.
//
// ChangeTime is the only field a rename changes on the source inode, so every
// other column on the row it writes must come from committed state. Writing the
// earlier snapshot back restores whatever that snapshot held — here a Size that
// a WRITE has since superseded — which is a lost update rather than a rename.
//
// The distinction is invisible unless a writer commits between the two reads,
// so the hook places one exactly there, the same way the ChangeTime case does.
// Asserting on Size rather than ChangeTime is deliberate: the rename is entitled
// to move ChangeTime, so ChangeTime cannot show whether the rest of the row came
// from the stale copy.
func TestRenameCtime_SizeCommittedInMoveWindowSurvives(t *testing.T) {
	ws := &windowStore{BadgerMetadataStore: newRenameStore(t)}
	svc, rootHandle, share := registerRenameStore(t, ws)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "s.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	const grown = uint64(4096)
	var mid *metadata.File
	ws.beforeTx = func() {
		size := grown
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Size: &size})
		require.NoError(t, hookErr)
		mid, hookErr = svc.GetFile(root.Context, handle)
		require.NoError(t, hookErr)
	}

	_, _, err = svc.Move(root, rootHandle, "s.bin", rootHandle, "t.bin")
	require.NoError(t, err)
	require.NotNil(t, mid, "hook did not fire: Move opened no transaction through the wrapper")
	require.Equal(t, grown, mid.Size, "precondition: the injected size must have committed")

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.Equal(t, grown, after.Size,
		"Size = %d; want the %d committed inside the rename's own window — Move wrote back the "+
			"inode snapshot it read before opening its transaction, discarding the newer size",
		after.Size, grown)
}
