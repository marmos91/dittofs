package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errSyncedTxRollback forces a transaction rollback in the rollback subtests.
var errSyncedTxRollback = errors.New("forced rollback")

// runSyncedHashTxOps covers the transaction-level SyncedHashStore surface
// (metadata.Transaction embeds SyncedHashStore): read-your-writes inside a
// transaction, rollback discarding pending marks/deletes, commit persisting
// them, and the first-wins/overwrite interplay DefaultCommitBlock builds on.
func runSyncedHashTxOps(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()

	makeHash := func(b byte) block.ContentHash {
		var h block.ContentHash
		h[0] = 0x7A // namespace away from other suites
		h[1] = b
		return h
	}
	blockLoc := func(id string) block.ChunkLocator {
		return block.ChunkLocator{BlockID: id, WireOffset: 0, WireLength: 512}
	}

	t.Run("ReadYourWrites", func(t *testing.T) {
		h := makeHash(0x01)
		locA := blockLoc("tx-ryw-a")
		locB := blockLoc("tx-ryw-b")

		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			// Unmarked at entry.
			synced, err := tx.IsSynced(ctx, h)
			require.NoError(t, err)
			require.False(t, synced, "hash must start unsynced")

			// Mark: visible within the same tx.
			require.NoError(t, tx.MarkSynced(ctx, h, locA))
			synced, err = tx.IsSynced(ctx, h)
			require.NoError(t, err)
			assert.True(t, synced, "MarkSynced must be visible to IsSynced in the same tx")
			got, found, err := tx.GetLocator(ctx, h)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, locA, got)

			// Delete: also visible.
			require.NoError(t, tx.DeleteSynced(ctx, h))
			synced, err = tx.IsSynced(ctx, h)
			require.NoError(t, err)
			assert.False(t, synced, "DeleteSynced must be visible to IsSynced in the same tx")
			_, found, err = tx.GetLocator(ctx, h)
			require.NoError(t, err)
			assert.False(t, found)

			// Re-mark after the in-tx delete: the NEW locator wins.
			require.NoError(t, tx.MarkSynced(ctx, h, locB))
			got, found, err = tx.GetLocator(ctx, h)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, locB, got, "MarkSynced after DeleteSynced in the same tx must record the new locator")
			return nil
		}))

		// After commit the final state (locB) persists.
		got, found, err := store.GetLocator(ctx, h)
		require.NoError(t, err)
		require.True(t, found, "hash must be synced after commit")
		assert.Equal(t, locB, got, "committed locator must be the post-delete re-mark")
	})

	t.Run("FirstWinsWithinTx", func(t *testing.T) {
		h := makeHash(0x02)
		locA := blockLoc("tx-fw-a")
		locB := blockLoc("tx-fw-b")

		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			require.NoError(t, tx.MarkSynced(ctx, h, locA))
			// Second mark without an intervening delete is a no-op (first wins,
			// matching the direct method's contract).
			require.NoError(t, tx.MarkSynced(ctx, h, locB))
			return nil
		}))

		got, found, err := store.GetLocator(ctx, h)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, locA, got, "first MarkSynced in the tx must win")
	})

	t.Run("FirstWinsAgainstStore", func(t *testing.T) {
		h := makeHash(0x03)
		locA := blockLoc("tx-fws-a")
		locB := blockLoc("tx-fws-b")

		require.NoError(t, store.MarkSynced(ctx, h, locA))
		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			synced, err := tx.IsSynced(ctx, h)
			require.NoError(t, err)
			assert.True(t, synced, "committed marker must be visible inside the tx")
			// Mark on an already-committed hash is a no-op.
			return tx.MarkSynced(ctx, h, locB)
		}))

		got, found, err := store.GetLocator(ctx, h)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, locA, got, "pre-existing locator must survive a first-wins tx mark")
	})

	t.Run("RollbackDiscards", func(t *testing.T) {
		hKept := makeHash(0x04)
		hNew := makeHash(0x05)
		locKept := blockLoc("tx-rb-kept")
		locNew := blockLoc("tx-rb-new")

		require.NoError(t, store.MarkSynced(ctx, hKept, locKept))

		err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			if err := tx.MarkSynced(ctx, hNew, locNew); err != nil {
				return err
			}
			if err := tx.DeleteSynced(ctx, hKept); err != nil {
				return err
			}
			return errSyncedTxRollback
		})
		require.ErrorIs(t, err, errSyncedTxRollback)

		// The pending mark was discarded…
		synced, err := store.IsSynced(ctx, hNew)
		require.NoError(t, err)
		assert.False(t, synced, "rolled-back MarkSynced must not persist")

		// …and the pending delete too.
		got, found, err := store.GetLocator(ctx, hKept)
		require.NoError(t, err)
		require.True(t, found, "rolled-back DeleteSynced must not remove the marker")
		assert.Equal(t, locKept, got)
	})

	t.Run("DeleteThenMarkOverwritesCommittedLocator", func(t *testing.T) {
		// The exact sequence DefaultCommitBlock uses for locator overwrite: a
		// committed standalone (zero-BlockID) locator is replaced by a block
		// locator via DeleteSynced+MarkSynced in one tx.
		h := makeHash(0x06)
		newLoc := blockLoc("tx-ow-block")

		require.NoError(t, store.MarkSynced(ctx, h, block.ChunkLocator{}))
		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			if err := tx.DeleteSynced(ctx, h); err != nil {
				return err
			}
			return tx.MarkSynced(ctx, h, newLoc)
		}))

		got, found, err := store.GetLocator(ctx, h)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, newLoc, got, "tx delete+mark must overwrite the committed standalone locator")
	})
}
