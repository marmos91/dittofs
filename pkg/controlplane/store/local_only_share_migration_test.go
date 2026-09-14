package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// oldShapeDB builds the shares table as it looked before the block-store
// collapse: a binding per tier, either of which could be empty.
func oldShapeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(`CREATE TABLE shares (
		name TEXT,
		block_store_id TEXT,
		local_block_store_id TEXT
	)`).Error; err != nil {
		t.Fatalf("create shares: %v", err)
	}
	return db
}

func insertShare(t *testing.T, db *gorm.DB, name, blockStoreID, localID string) {
	t.Helper()
	if err := db.Exec(
		"INSERT INTO shares (name, block_store_id, local_block_store_id) VALUES (?, ?, ?)",
		name, blockStoreID, localID,
	).Error; err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
}

// TestCheckLocalOnlyShares_RefusesAShareLeftWithoutABlockStore is the point of
// the guard: the rename empties block_store_id for a share that never had a
// remote, so dropping the local column would strand it.
func TestCheckLocalOnlyShares_RefusesAShareLeftWithoutABlockStore(t *testing.T) {
	db := oldShapeDB(t)
	insertShare(t, db, "/archive", "", "local-store-id")

	err := checkLocalOnlyShares(db)
	if !errors.Is(err, ErrLocalOnlyShareUnbound) {
		t.Fatalf("checkLocalOnlyShares = %v, want ErrLocalOnlyShareUnbound", err)
	}
	if !strings.Contains(err.Error(), "/archive") {
		t.Errorf("the refusal must name the share an operator has to fix, got: %v", err)
	}
}

// TestCheckLocalOnlyShares_PassesAShareThatCarriedARemote covers the share the
// rename migrates correctly — it must not be caught by the guard.
func TestCheckLocalOnlyShares_PassesAShareThatCarriedARemote(t *testing.T) {
	db := oldShapeDB(t)
	insertShare(t, db, "/backed", "remote-store-id", "local-store-id")

	if err := checkLocalOnlyShares(db); err != nil {
		t.Fatalf("a share whose remote binding survived the rename must pass, got %v", err)
	}
}

// TestCheckLocalOnlyShares_PassesWhenNothingIsBound leaves both columns empty:
// there is no binding to lose, so this guard is not the one to complain.
func TestCheckLocalOnlyShares_PassesWhenNothingIsBound(t *testing.T) {
	db := oldShapeDB(t)
	insertShare(t, db, "/empty", "", "")

	if err := checkLocalOnlyShares(db); err != nil {
		t.Fatalf("a share with no binding at all is not this guard's business, got %v", err)
	}
}
