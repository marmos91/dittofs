package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// errTxWriteRollback forces a rollback.
var errTxWriteRollback = errors.New("forced rollback")

// runTxWriteRollbackOps pins that the transaction-level *writes* land inside
// the caller's transaction rather than beside it.
//
// FileReadTxOps covers the reads. This covers the other half, and the two fail
// for opposite reasons. A read that escapes to the pool returns a stale-but
// plausible answer; a write that escapes to the pool is *durable* — it
// survives the rollback that was supposed to erase it, and every assertion in
// the rest of the suite still passes, because the rest of the suite only ever
// asks what is committed.
//
// This matters most while transaction bodies are being consolidated onto a
// shared implementation. Such a body reaches its executor through a struct
// field, so pointing one at the pool instead of the transaction is a
// one-character mistake that compiles, races nothing, and leaves no trace but
// a write that would not roll back.
//
// Each subtest writes through one method, aborts, and asserts the store shows
// no trace afterwards. They are separate so a failure names the method that
// escaped rather than the batch it travelled in — but only on a backend whose
// pool can hand the escaped write a second connection. On a single-connection
// backend the escaped write instead blocks on the connection the enclosing
// transaction is still holding, and the failure arrives as the package's test
// timeout with no subtest named. Both are detections; only one is legible, so
// do not read a hang here as an unrelated flake.
func runTxWriteRollbackOps(t *testing.T, factory StoreFactory) {
	t.Helper()

	// seedFile commits a file and returns its handle, so a subtest that needs
	// something to mutate is not also testing creation.
	seedFile := func(t *testing.T, store metadata.Store, share string, root metadata.FileHandle, name string) metadata.FileHandle {
		t.Helper()
		ctx := context.Background()
		path := childFullPath(t, store, root, name)
		handle, err := store.GenerateHandle(ctx, share, path)
		require.NoError(t, err)
		_, id, err := metadata.DecodeFileHandle(handle)
		require.NoError(t, err)
		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			require.NoError(t, tx.UpdateAttrs(ctx, &metadata.File{
				ID: id, ShareName: share, Path: path,
				FileAttr: metadata.FileAttr{
					Type: metadata.FileTypeRegular, Mode: 0644, UID: 1000, GID: 1000,
				},
			}))
			require.NoError(t, tx.SetParent(ctx, handle, root))
			require.NoError(t, tx.SetChild(ctx, root, name, handle))
			require.NoError(t, tx.SetLinkCount(ctx, handle, 1))
			return nil
		}))
		return handle
	}

	// abort runs fn in a transaction that always rolls back.
	abort := func(t *testing.T, store metadata.Store, fn func(tx metadata.Transaction)) {
		t.Helper()
		err := store.WithTransaction(context.Background(), func(tx metadata.Transaction) error {
			fn(tx)
			return errTxWriteRollback
		})
		require.ErrorIs(t, err, errTxWriteRollback, "transaction must surface the rollback cause")
	}

	t.Run("SetChild", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		root := createTestShare(t, store, "txwrite-setchild")
		victim := seedFile(t, store, "txwrite-setchild", root, "seed.txt")

		abort(t, store, func(tx metadata.Transaction) {
			require.NoError(t, tx.SetChild(ctx, root, "ghost.txt", victim))
		})

		_, err := store.GetChild(ctx, root, "ghost.txt")
		assert.True(t, metadata.IsNotFoundError(err),
			"SetChild survived the rollback: it ran outside the transaction (got %v)", err)
	})

	t.Run("DeleteChild", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		root := createTestShare(t, store, "txwrite-delchild")
		handle := seedFile(t, store, "txwrite-delchild", root, "keep.txt")

		abort(t, store, func(tx metadata.Transaction) {
			require.NoError(t, tx.DeleteChild(ctx, root, "keep.txt"))
		})

		got, err := store.GetChild(ctx, root, "keep.txt")
		require.NoError(t, err, "DeleteChild survived the rollback: it ran outside the transaction")
		assert.Equal(t, handle, got)
	})

	t.Run("SetLinkCount", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		root := createTestShare(t, store, "txwrite-nlink")
		handle := seedFile(t, store, "txwrite-nlink", root, "nlink.txt")

		abort(t, store, func(tx metadata.Transaction) {
			require.NoError(t, tx.SetLinkCount(ctx, handle, 9))
		})

		nlink, err := store.GetLinkCount(ctx, handle)
		require.NoError(t, err)
		assert.Equal(t, uint32(1), nlink, "SetLinkCount survived the rollback: it ran outside the transaction")
	})

	t.Run("DeleteFile", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		root := createTestShare(t, store, "txwrite-delfile")
		handle := seedFile(t, store, "txwrite-delfile", root, "doomed.txt")

		abort(t, store, func(tx metadata.Transaction) {
			require.NoError(t, tx.DeleteFile(ctx, handle))
		})

		_, err := store.GetFile(ctx, handle)
		assert.NoError(t, err, "DeleteFile survived the rollback: it ran outside the transaction")
	})

	t.Run("UpdateShareOptions", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		const share = "txwrite-opts"
		createTestShare(t, store, share)

		before, err := store.GetShareOptions(ctx, share)
		require.NoError(t, err)

		abort(t, store, func(tx metadata.Transaction) {
			flipped := *before
			flipped.ReadOnly = !before.ReadOnly
			require.NoError(t, tx.UpdateShareOptions(ctx, share, &flipped))
		})

		after, err := store.GetShareOptions(ctx, share)
		require.NoError(t, err)
		assert.Equal(t, before.ReadOnly, after.ReadOnly,
			"UpdateShareOptions survived the rollback: it ran outside the transaction, "+
				"or the commit path dropped the share cache for a write that never landed")
	})

	t.Run("SetServerConfig", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()

		before, err := store.GetServerConfig(ctx)
		require.NoError(t, err)

		abort(t, store, func(tx metadata.Transaction) {
			next := before
			next.CustomSettings = map[string]any{"txwrite-rollback": "ghost"}
			require.NoError(t, tx.SetServerConfig(ctx, next))
		})

		after, err := store.GetServerConfig(ctx)
		require.NoError(t, err)
		assert.NotContains(t, after.CustomSettings, "txwrite-rollback",
			"SetServerConfig survived the rollback: it ran outside the transaction")
	})
}
