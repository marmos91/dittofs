package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// openAt opens (and migrates) a file-backed store, which an upgrade test needs:
// an in-memory database does not survive the close and reopen that separates
// the old schema from the migration under test.
func openAt(t *testing.T, path string) *GORMStore {
	t.Helper()
	s, err := New(&Config{Type: DatabaseTypeSQLite, SQLite: SQLiteConfig{Path: path}})
	if err != nil {
		t.Fatalf("open store at %s: %v", path, err)
	}
	return s
}

// A share's size ceiling must survive the column rename. Losing it reads back
// as "never set", which silently swaps the operator's ceiling for the deduced
// default.
func TestMigration_JournalSizeRenamePreservesTheValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")

	// Build the current schema, then rename the column backwards to stand in
	// for a database created before the rename.
	s := openAt(t, path)
	if err := s.DB().Exec(
		`INSERT INTO shares (id, name, metadata_store_id, local_block_store_id, journal_size)
		 VALUES (?, ?, ?, ?, ?)`,
		"share-id", "/legacy", "meta-id", "local-id", 12345,
	).Error; err != nil {
		t.Fatalf("insert share: %v", err)
	}
	if err := s.DB().Migrator().RenameColumn(&models.Share{}, "journal_size", "local_store_size"); err != nil {
		t.Fatalf("simulate the old schema: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening runs the migration.
	s2 := openAt(t, path)
	defer func() { _ = s2.Close() }()

	var got int64
	if err := s2.DB().Raw("SELECT journal_size FROM shares WHERE name = ?", "/legacy").Scan(&got).Error; err != nil {
		t.Fatalf("read journal_size: %v", err)
	}
	if got != 12345 {
		t.Errorf("journal_size after migration = %d, want 12345", got)
	}
	if s2.DB().Migrator().HasColumn(&models.Share{}, "local_store_size") {
		t.Error("local_store_size still present after migration; the value would be read from the wrong column")
	}
}

// If an earlier upgrade added journal_size without moving the values across,
// which column is authoritative cannot be recovered from the schema. Guessing
// changes every share's ceiling silently, so startup must refuse.
func TestMigration_RefusesWhenBothColumnsExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")

	s := openAt(t, path)
	if err := s.DB().Exec(`ALTER TABLE shares ADD COLUMN local_store_size INTEGER DEFAULT 0`).Error; err != nil {
		t.Fatalf("add legacy column: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := New(&Config{Type: DatabaseTypeSQLite, SQLite: SQLiteConfig{Path: path}})
	if err == nil {
		t.Fatal("New: got nil, want a refusal when both columns exist")
	}
	for _, want := range []string{"local_store_size", "journal_size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q; got: %v", want, err)
		}
	}
}
