package store

import (
	"path/filepath"
	"testing"
)

// seedShareWithStoreConfig creates a block store carrying cfg and a share
// pointing at it, standing in for an install that configured its durability
// tier in the local block store config.
func seedShareWithStoreConfig(t *testing.T, s *GORMStore, share, cfg string) {
	t.Helper()
	if err := s.DB().Exec(
		"INSERT INTO block_store_configs (id, name, type, config) VALUES (?, ?, ?, ?)",
		"bs-"+share, "bs-"+share, "fs", cfg,
	).Error; err != nil {
		t.Fatalf("insert block store: %v", err)
	}
	if err := s.DB().Exec(
		`INSERT INTO shares (id, name, metadata_store_id, local_block_store_id, commit_ack)
		 VALUES (?, ?, ?, ?, ?)`,
		"id-"+share, share, "meta", "bs-"+share, "",
	).Error; err != nil {
		t.Fatalf("insert share: %v", err)
	}
}

// A durability tier an operator chose must survive the move from the block
// store config onto the share. Defaulting it back to journal would quietly
// weaken a promise they made deliberately, which no observation of the running
// system would reveal.
func TestMigration_CarriesDurabilityOntoTheShare(t *testing.T) {
	for _, tc := range []struct {
		name        string
		storeConfig string
		wantAck     string
		wantRelaxed bool
	}{
		{"durability remote", `{"durability":"remote"}`, "block-store", false},
		{"durability writeback", `{"durability":"writeback"}`, "journal", true},
		{"durability local", `{"durability":"local"}`, "journal", false},
		{"legacy require_durable_commit", `{"require_durable_commit":true}`, "block-store", false},
		{"legacy writeback bool", `{"writeback":true}`, "journal", true},
		{"both legacy bools", `{"writeback":true,"require_durable_commit":true}`, "block-store", true},
		{"nothing configured", `{}`, "journal", false},
		{"unknown tier defaults to local", `{"durability":"bogus"}`, "journal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cp.db")
			s := openAt(t, path)
			seedShareWithStoreConfig(t, s, "/sh", tc.storeConfig)
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			s2 := openAt(t, path)
			defer func() { _ = s2.Close() }()

			var got struct {
				CommitAck             string
				RelaxedMetadataCommit bool
			}
			if err := s2.DB().Raw(
				"SELECT commit_ack, relaxed_metadata_commit FROM shares WHERE name = ?", "/sh",
			).Scan(&got).Error; err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got.CommitAck != tc.wantAck {
				t.Errorf("commit_ack = %q, want %q", got.CommitAck, tc.wantAck)
			}
			if got.RelaxedMetadataCommit != tc.wantRelaxed {
				t.Errorf("relaxed_metadata_commit = %v, want %v", got.RelaxedMetadataCommit, tc.wantRelaxed)
			}
		})
	}
}

// An operator who changes the promise after upgrading must not have the old
// block store config put back on the next restart.
func TestMigration_DoesNotReapplyOverAnOperatorChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")
	s := openAt(t, path)
	seedShareWithStoreConfig(t, s, "/sh", `{"durability":"remote"}`)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// First restart migrates it across; the operator then moves it back.
	s2 := openAt(t, path)
	if err := s2.DB().Exec("UPDATE shares SET commit_ack = ? WHERE name = ?", "journal", "/sh").Error; err != nil {
		t.Fatalf("operator change: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s3 := openAt(t, path)
	defer func() { _ = s3.Close() }()

	var got string
	if err := s3.DB().Raw("SELECT commit_ack FROM shares WHERE name = ?", "/sh").Scan(&got).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "journal" {
		t.Errorf("commit_ack = %q, want the operator's journal to stand", got)
	}
}
