package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// runBlockRecordTxOps pins that the transaction-level block-record operations
// run inside the caller's transaction rather than beside it.
//
// BlockRecordOps drives the same five operations through the store, where a
// backend whose transaction path reached for the pool would still answer
// correctly: everything it reads is already committed. Only two things
// separate the paths — a write is visible to a read in the same transaction
// before commit, and a rolled-back write is visible to nobody afterwards.
//
// CommitBlock already exercises the transaction's Get and Put indirectly, so
// the operations with no other transaction-level coverage at all are Delete,
// Walk and DecrLiveChunkCount.
//
// A backend that escapes to the pool fails this by wedging rather than
// erroring: the escaped statement waits on a connection the enclosing
// transaction still holds, so the symptom is the package's test timeout, not
// an assertion message.
func runBlockRecordTxOps(t *testing.T, factory StoreFactory) {
	t.Helper()

	rec := func(id string, live uint32) block.BlockRecord {
		var h block.ContentHash
		copy(h[:], id)
		return block.BlockRecord{
			BlockID:        id,
			BlockHash:      h,
			Length:         4096,
			LiveChunkCount: live,
			SyncState:      block.BlockStateRemote,
		}
	}

	t.Run("ReadYourWrites", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()

		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			require.NoError(t, tx.PutBlockRecord(ctx, rec("tx-ryw", 10)))

			got, found, err := tx.GetBlockRecord(ctx, "tx-ryw")
			require.NoError(t, err)
			require.True(t, found, "a record written in this transaction was not visible to it")
			require.Equal(t, uint32(10), got.LiveChunkCount)

			// Walk must see it too: it is a separate statement, so a backend
			// can get the single-row read right and still stream from the pool.
			var seen int
			require.NoError(t, tx.WalkBlockRecords(ctx, func(r block.BlockRecord) error {
				if r.BlockID == "tx-ryw" {
					seen++
				}
				return nil
			}))
			require.Equal(t, 1, seen, "walk did not see a record written in the same transaction")

			// The floor and the returned remainder both have to come from the
			// uncommitted row.
			remaining, err := tx.DecrLiveChunkCount(ctx, "tx-ryw", 4)
			require.NoError(t, err)
			require.Equal(t, uint32(6), remaining)
			return nil
		}))

		got, found, err := store.GetBlockRecord(ctx, "tx-ryw")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, uint32(6), got.LiveChunkCount, "the committed count is not what the transaction computed")
	})

	t.Run("RollbackDiscardsWrites", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()

		require.NoError(t, store.PutBlockRecord(ctx, rec("tx-rollback", 5)))

		errAbort := errors.New("abort after the block-record writes")
		err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			require.NoError(t, tx.PutBlockRecord(ctx, rec("tx-rollback-new", 1)))
			_, decErr := tx.DecrLiveChunkCount(ctx, "tx-rollback", 5)
			require.NoError(t, decErr)
			require.NoError(t, tx.DeleteBlockRecord(ctx, "tx-rollback"))
			return errAbort
		})
		require.ErrorIs(t, err, errAbort)

		// The delete, the decrement and the insert must all be undone.
		got, found, err := store.GetBlockRecord(ctx, "tx-rollback")
		require.NoError(t, err)
		require.True(t, found, "a rolled-back delete removed the record anyway")
		require.Equal(t, uint32(5), got.LiveChunkCount, "a rolled-back decrement survived")

		_, found, err = store.GetBlockRecord(ctx, "tx-rollback-new")
		require.NoError(t, err)
		require.False(t, found, "a rolled-back insert survived")
	})
}
