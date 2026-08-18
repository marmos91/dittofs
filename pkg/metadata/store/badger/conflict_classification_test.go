package badger

import (
	"context"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestUpdateWithConflictRetry_ClassifiesExhaustedConflict pins the two retry
// loops to the same contract. Both withTransaction and updateWithConflictRetry
// give up after maxTransactionRetries consecutive SSI aborts, and what they
// return then has to be recognizable: IsConflictError matches only a
// *StoreError carrying ErrConflict, so returning badger's bare sentinel would
// leave an exhausted conflict unclassified on this backend while the SQL
// backends classify the same condition. Each abort must also reach the shared
// counter, which is what tests read to assert a workload stayed conflict-free.
func TestUpdateWithConflictRetry_ClassifiesExhaustedConflict(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	restore := SetMaxTransactionRetriesForTest(3)
	defer restore()

	before := store.TransactionConflictsForTest()

	attempts := 0
	err = store.updateWithConflictRetry(ctx, func(*badgerdb.Txn) error {
		attempts++
		return badgerdb.ErrConflict
	})

	require.Error(t, err)
	require.Equal(t, 3, attempts, "every configured attempt should run")
	require.True(t, metadata.IsConflictError(err),
		"exhausted conflict must classify as ErrConflict, got %#v", err)

	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrConflict, storeErr.Code)
	require.ErrorIs(t, err, badgerdb.ErrConflict,
		"the raw sentinel must stay reachable through the unwrap chain")

	require.Equal(t, before+3, store.TransactionConflictsForTest(),
		"each abort should reach the shared conflict counter")
}

// TestUpdateWithConflictRetry_PassesThroughNonConflict confirms the mapping
// added for the exhausted case does not swallow or reclassify an unrelated
// failure, which short-circuits without retrying.
func TestUpdateWithConflictRetry_PassesThroughNonConflict(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	attempts := 0
	sentinel := badgerdb.ErrKeyNotFound
	err = store.updateWithConflictRetry(ctx, func(*badgerdb.Txn) error {
		attempts++
		return sentinel
	})

	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, attempts, "a non-conflict error must not retry")
	require.False(t, metadata.IsConflictError(err))
}
