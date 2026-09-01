package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// errFileReadTxRollback forces a rollback in the discard subtest.
var errFileReadTxRollback = errors.New("forced rollback")

// runFileReadTxOps pins that the transaction-level file and directory reads
// run inside the caller's transaction rather than beside it.
//
// A backend can satisfy every other test in this suite while its
// transaction-level GetFile reads from the pool: such a read still returns the
// right answer for anything already committed, which is all the rest of the
// suite asks about. What separates the two is covered here — a write is
// visible to a read in the same transaction before commit, and a rolled-back
// write is visible to nobody afterwards.
//
// A pooled backend fails this by wedging rather than erroring: the escaped
// read waits on a connection the enclosing transaction is still holding, so
// the symptom is the package's test timeout, not an assertion message.
func runFileReadTxOps(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("ReadYourWrites", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		root := createTestShare(t, store, "txread")

		const name = "in-flight.txt"
		path := childFullPath(t, store, root, name)
		handle, err := store.GenerateHandle(ctx, "txread", path)
		require.NoError(t, err)
		_, id, err := metadata.DecodeFileHandle(handle)
		require.NoError(t, err)

		payloadID := metadata.PayloadID("txread-payload-ryw")
		file := &metadata.File{
			ID:        id,
			ShareName: "txread",
			Path:      path,
			FileAttr: metadata.FileAttr{
				PayloadID: payloadID,
				Type:      metadata.FileTypeRegular,
				Mode:      0644,
				UID:       1000,
				GID:       1000,
			},
		}

		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			require.NoError(t, tx.UpdateAttrs(ctx, file))
			require.NoError(t, tx.SetParent(ctx, handle, root))
			require.NoError(t, tx.SetChild(ctx, root, name, handle))
			require.NoError(t, tx.SetLinkCount(ctx, handle, 1))

			got, err := tx.GetFile(ctx, handle)
			require.NoError(t, err, "GetFile must see the write from its own transaction")
			assert.Equal(t, id, got.ID)

			child, err := tx.GetChild(ctx, root, name)
			require.NoError(t, err, "GetChild must see the entry from its own transaction")
			assert.Equal(t, handle, child)

			parent, err := tx.GetParent(ctx, handle)
			require.NoError(t, err, "GetParent must see the edge from its own transaction")
			assert.Equal(t, root, parent)

			nlink, err := tx.GetLinkCount(ctx, handle)
			require.NoError(t, err)
			assert.Equal(t, uint32(1), nlink, "GetLinkCount must see the count set in its own transaction")

			byPayload, err := tx.GetFileByPayloadID(ctx, payloadID)
			require.NoError(t, err, "GetFileByPayloadID must see the write from its own transaction")
			assert.Equal(t, id, byPayload.ID)

			entries, _, err := tx.ListChildren(ctx, root, "", 0)
			require.NoError(t, err)
			assert.True(t, hasEntryNamed(entries, name),
				"ListChildren must see the entry added by its own transaction, got %v", entryNames(entries))
			return nil
		}))

		// And it survived the commit.
		got, err := store.GetFile(ctx, handle)
		require.NoError(t, err)
		assert.Equal(t, id, got.ID)
	})

	t.Run("RollbackDiscards", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		root := createTestShare(t, store, "txread")

		const name = "rolled-back.txt"
		path := childFullPath(t, store, root, name)
		handle, err := store.GenerateHandle(ctx, "txread", path)
		require.NoError(t, err)
		_, id, err := metadata.DecodeFileHandle(handle)
		require.NoError(t, err)

		err = store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			require.NoError(t, tx.UpdateAttrs(ctx, &metadata.File{
				ID:        id,
				ShareName: "txread",
				Path:      path,
				FileAttr: metadata.FileAttr{
					Type: metadata.FileTypeRegular,
					Mode: 0644,
					UID:  1000,
					GID:  1000,
				},
			}))
			require.NoError(t, tx.SetChild(ctx, root, name, handle))
			return errFileReadTxRollback
		})
		require.ErrorIs(t, err, errFileReadTxRollback)

		_, err = store.GetFile(ctx, handle)
		assert.True(t, metadata.IsNotFoundError(err),
			"a rolled-back file must not be readable afterwards, got %v", err)

		_, err = store.GetChild(ctx, root, name)
		assert.True(t, metadata.IsNotFoundError(err),
			"a rolled-back directory entry must not resolve afterwards, got %v", err)

		entries, _, err := store.ListChildren(ctx, root, "", 0)
		require.NoError(t, err)
		assert.False(t, hasEntryNamed(entries, name),
			"a rolled-back entry must not appear in a listing, got %v", entryNames(entries))
	})
}

func hasEntryNamed(entries []metadata.DirEntry, name string) bool {
	for i := range entries {
		if entries[i].Name == name {
			return true
		}
	}
	return false
}

func entryNames(entries []metadata.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for i := range entries {
		names = append(names, entries[i].Name)
	}
	return names
}
