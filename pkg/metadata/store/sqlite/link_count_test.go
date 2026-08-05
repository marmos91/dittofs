package sqlite_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// TestSQLite_GetLinkCountPropagatesQueryFailure pins that GetLinkCount reports
// only a missing row as zero links and surfaces every other query failure.
// Callers branch on the count to decide whether a file's content is still
// referenced, so a fabricated 0 makes the removal path free live content.
//
// The failure is injected by dropping the inodes table out from under the
// store, so the SELECT fails with "no such table" rather than sql.ErrNoRows.
// That is safe here because each sqlite test store is a private database file.
// The postgres store carries the identical branch, but its tests share one
// database, so the same DDL would strand every test after it.
func TestSQLite_GetLinkCountPropagatesQueryFailure(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteStoreFactory()(t)

	handle, err := metadata.EncodeShareHandle("testshare", uuid.New())
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}

	// An absent row is not an error: the count is genuinely zero.
	count, err := store.GetLinkCount(ctx, handle)
	if err != nil {
		t.Fatalf("GetLinkCount on a missing row must return (0, nil); got err %v", err)
	}
	if count != 0 {
		t.Fatalf("GetLinkCount on a missing row = %d, want 0", count)
	}

	db := store.(*sqlite.SQLiteMetadataStore).DBForBench()
	if _, err := db.ExecContext(ctx, `DROP TABLE inodes`); err != nil {
		t.Fatalf("dropping inodes to inject a query failure: %v", err)
	}

	if _, err := store.GetLinkCount(ctx, handle); err == nil {
		t.Error("pool GetLinkCount reported success on a failing query")
	}

	txErr := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		_, err := tx.GetLinkCount(ctx, handle)
		return err
	})
	if txErr == nil {
		t.Error("transaction GetLinkCount reported success on a failing query")
	}
}
