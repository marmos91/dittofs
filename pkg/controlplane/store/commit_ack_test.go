package store

import (
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// A share that predates the column must come up acknowledging on the journal,
// which is the behaviour it already had. Leaving it empty would make the
// durability promise unreadable.
func TestMigration_CommitAckBackfillsToJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")

	s := openAt(t, path)
	if err := s.DB().Exec(
		`INSERT INTO shares (id, name, metadata_store_id, local_block_store_id, commit_ack)
		 VALUES (?, ?, ?, ?, ?)`,
		"s1", "/legacy", "meta", "local", "",
	).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := openAt(t, path)
	defer func() { _ = s2.Close() }()

	var got string
	if err := s2.DB().Raw("SELECT commit_ack FROM shares WHERE name = ?", "/legacy").Scan(&got).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != string(models.CommitAckJournal) {
		t.Errorf("commit_ack = %q, want %q", got, models.CommitAckJournal)
	}
}
