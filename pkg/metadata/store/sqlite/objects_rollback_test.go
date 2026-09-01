package sqlite_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// TestTransactionChunkWritesRollBack pins the reason a transaction embeds its
// own executor over the open transaction rather than reusing the store's pool
// one.
//
// A write that reached the pool instead would run on a separate connection,
// commit independently, and survive the enclosing transaction's rollback. The
// rest of the suite never notices, because every other test commits: a chunk
// written to the pool is indistinguishable from one written to a committed
// transaction. On SQLite the mistake is even quieter to diagnose — the pool
// write blocks on the write lock the transaction is holding, so it presents as
// a hang rather than a wrong answer.
func TestTransactionChunkWritesRollBack(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.NewSQLiteMetadataStore(ctx,
		&sqlite.SQLiteMetadataStoreConfig{Path: t.TempDir() + "/m.db", AutoMigrate: true},
		sqliteTestCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const chunkID = "rollback-probe/0"
	sum := sha256.Sum256([]byte("rollback-probe"))
	var hash block.ContentHash
	copy(hash[:], sum[:])

	now := time.Now().UTC().Truncate(time.Second)
	chunk := &metadata.FileChunk{
		ID:          chunkID,
		Hash:        hash,
		DataSize:    64,
		StartOffset: 0,
		RefCount:    1,
		LastAccess:  now,
		CreatedAt:   now,
		State:       block.BlockStateRemote,
	}

	sentinel := errors.New("roll this back")
	err = store.WithTransaction(ctx, func(tx metadata.Transaction) error {
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
		t.Fatalf("chunk survived a rolled-back transaction (err = %v) — the write escaped to the pool", err)
	}
}
