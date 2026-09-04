package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
	"github.com/stretchr/testify/require"
)

// windowStore is a sqlite store that runs beforeTx once, immediately before the
// next transaction it opens. Service.Move reads the source inode outside any
// transaction and then opens one to stamp it; the hook lands exactly in that
// gap, which is the window an in-transaction re-read exists to cover.
//
// Only WithTransaction is overridden. sqlite does not implement
// RelaxedTransactor, so Move's withRelaxedTransaction falls through to this
// method; if sqlite ever gains WithTransactionRelaxed the promoted method would
// bypass the hook, and the test then fails on "hook did not fire" rather than
// passing vacuously.
//
// The hook fires once per WithTransaction CALL, before any attempt. A backend
// that retries by re-running the closure inside one call — badger does — could
// not re-fire it, and would take the relaxed path in any case, which is why this
// is sqlite-bound. Nothing here touches production code: it works only because
// RegisterStoreForShare accepts the metadata.Store interface.
type windowStore struct {
	*sqlite.SQLiteMetadataStore
	beforeTx func()
}

func (w *windowStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	// Cleared before it runs: the hook itself commits through this store, and
	// that inner transaction must not re-enter it.
	if hook := w.beforeTx; hook != nil {
		w.beforeTx = nil
		hook()
	}
	return w.SQLiteMetadataStore.WithTransaction(ctx, fn)
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
// What this does NOT pin: anything about postgres. There the pre-read is a plain
// SELECT under READ COMMITTED, so a writer can still commit between that read
// and the row lock the update takes. What closes this window on sqlite, badger
// and memory is the store's isolation, not anything about how Move is written.
func TestRenameCtime_AdvanceInsideMoveWindowIsNotErased(t *testing.T) {
	ws := &windowStore{SQLiteMetadataStore: newSQLiteRenameStore(t)}
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
