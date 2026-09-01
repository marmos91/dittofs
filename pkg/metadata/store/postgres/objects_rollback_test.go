//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestTransactionChunkWritesRollBack pins the reason the transaction-level
// FileChunk methods take an Executor over the open pgx.Tx rather than the pool.
//
// A write that reached the pool instead would run on a separate connection,
// commit independently, and survive the enclosing transaction's rollback. The
// suite otherwise never notices: every other test commits, and a chunk written
// to the pool is indistinguishable from one written to a committed transaction.
func TestTransactionChunkWritesRollBack(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	const chunkID = "rollback-probe/0"
	now := time.Now().UTC().Truncate(time.Second)
	chunk := &metadata.FileChunk{
		ID:          chunkID,
		Hash:        hashOfSeed("rollback-probe"),
		DataSize:    64,
		StartOffset: 0,
		RefCount:    1,
		LastAccess:  now,
		CreatedAt:   now,
		State:       block.BlockStateRemote,
	}

	// Assert the row is absent to begin with rather than deleting blindly: a
	// leftover from an earlier run would otherwise make the final check pass or
	// fail for the wrong reason.
	if _, err := store.GetFileChunk(ctx, chunkID); !errors.Is(err, metadata.ErrFileChunkNotFound) {
		t.Fatalf("probe chunk %q already present before the test (err = %v)", chunkID, err)
	}

	sentinel := errors.New("roll this back")
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		if putErr := tx.Put(ctx, chunk); putErr != nil {
			return putErr
		}
		// Read-your-writes: the row must be visible inside the transaction that
		// wrote it, which only holds if the write went to the transaction.
		got, getErr := tx.GetFileChunk(ctx, chunkID)
		if getErr != nil {
			return getErr
		}
		if got.ID != chunkID {
			t.Errorf("in-transaction read got id %q, want %q", got.ID, chunkID)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTransaction error = %v, want the sentinel", err)
	}

	// The rollback must have taken the chunk with it.
	if _, err := store.GetFileChunk(ctx, chunkID); !errors.Is(err, metadata.ErrFileChunkNotFound) {
		// The database is shared across tests, so clear the stray row before
		// failing or the next run reports the leftover instead of the bug.
		_ = store.Delete(ctx, chunkID)
		t.Fatalf("chunk survived a rolled-back transaction (err = %v) — the write escaped to the pool", err)
	}
}
