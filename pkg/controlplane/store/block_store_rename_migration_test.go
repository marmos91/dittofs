package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// makeSplitStoreSchema builds a database in the two-column shape that predates
// the single block store: block_store_id renamed back to remote_block_store_id,
// with local_block_store_id beside it. The seed then writes rows in that shape.
//
// Both siblings end in the name the migration looks for, which is the property
// the rename has to survive: a column check that matches a suffix rather than
// the whole name reports block_store_id present on this table, and the upgrade
// refuses the rename it should have performed.
func makeSplitStoreSchema(t *testing.T, path string, seed func(s *GORMStore)) {
	t.Helper()

	// Built from a model carrying the old field set rather than by reshaping
	// the current table: a rename would drag the current NOT NULL onto the
	// remote column, which was a pointer before the collapse and holds NULL
	// for a share that never had a remote store. Letting AutoMigrate write the
	// table also quotes the columns the way every real database has them —
	// the SQLite driver matches that quoting when it adds and drops columns.
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if err := db.AutoMigrate(&splitShare{}, &splitBlockStoreConfig{}); err != nil {
		t.Fatalf("build the split-store schema: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying db: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s := openSplitStore(t, path)
	seed(s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// splitShare is the share row as it stood while a share named a local and a
// remote block store separately. Only the columns the migration reads are
// declared; AutoMigrate adds the rest when the server opens the database.
type splitShare struct {
	ID                 string  `gorm:"primaryKey;size:36"`
	Name               string  `gorm:"uniqueIndex;not null;size:255"`
	MetadataStoreID    string  `gorm:"not null;size:36"`
	LocalBlockStoreID  string  `gorm:"not null;size:36"`
	RemoteBlockStoreID *string `gorm:"size:36"`
}

func (splitShare) TableName() string { return "shares" }

// splitBlockStoreConfig carries the kind discriminator that told a local store
// from a remote one.
type splitBlockStoreConfig struct {
	ID        string `gorm:"primaryKey;size:36"`
	Name      string `gorm:"not null;size:255"`
	Kind      string `gorm:"not null;size:10"`
	Type      string `gorm:"not null;size:50"`
	Config    string
	CreatedAt time.Time
}

func (splitBlockStoreConfig) TableName() string { return "block_store_configs" }

// openSplitStore opens the database for seeding without running the migration,
// which the seed has to precede.
func openSplitStore(t *testing.T, path string) *GORMStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open split store: %v", err)
	}
	return &GORMStore{db: db}
}

// insertSplitShare writes a share in the old two-column shape. remoteID is nil
// for a share that never had a remote store: the column it lands in was a
// pointer in the old model, so an unbound share holds NULL there rather than
// the empty string, and NULL is the shape the guards meet in the field.
func insertSplitShare(t *testing.T, s *GORMStore, name string, remoteID any, localID string) {
	t.Helper()
	if err := s.DB().Exec(
		`INSERT INTO shares (id, name, metadata_store_id, remote_block_store_id, local_block_store_id)
		 VALUES (?, ?, ?, ?, ?)`,
		name+"-id", name, "meta-id", remoteID, localID,
	).Error; err != nil {
		t.Fatalf("insert share %s: %v", name, err)
	}
}

// insertLocalStore writes the local block store a split-model share hung its
// journal off. Its config records the root directory to compare against.
func insertLocalStore(t *testing.T, s *GORMStore, id, path string) {
	t.Helper()
	if err := s.DB().Exec(
		`INSERT INTO block_store_configs (id, name, kind, type, config, created_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		id, id, "local", "fs", `{"path":"`+path+`"}`,
	).Error; err != nil {
		t.Fatalf("insert local store: %v", err)
	}
}

// openWithRoot runs the migration the way a server does, with a journal root
// resolved. Every upgrade in the field has one; leaving it empty skips the
// containment check entirely, so it is the wrong input to test against.
func openWithRoot(t *testing.T, path, journalRoot string) (*GORMStore, error) {
	t.Helper()
	return New(&Config{
		Type:        DatabaseTypeSQLite,
		SQLite:      SQLiteConfig{Path: path},
		JournalRoot: journalRoot,
	})
}

// A share's block store reference must survive the rename. Losing it leaves the
// share bound to nothing, which fails at startup and skips the share.
func TestMigration_RemoteBlockStoreRenamePreservesTheBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")
	makeSplitStoreSchema(t, path, func(s *GORMStore) {
		insertLocalStore(t, s, "local-bs-id", "/srv/blocks")
		insertSplitShare(t, s, "/legacy", "remote-bs-id", "local-bs-id")
	})

	s2, err := openWithRoot(t, path, "/srv/blocks")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var got string
	if err := s2.DB().Raw("SELECT block_store_id FROM shares WHERE name = ?", "/legacy").Scan(&got).Error; err != nil {
		t.Fatalf("read block_store_id: %v", err)
	}
	if got != "remote-bs-id" {
		t.Errorf("block_store_id after migration = %q, want %q", got, "remote-bs-id")
	}
	if hasColumn(s2.DB(), &models.Share{}, "remote_block_store_id") {
		t.Error("remote_block_store_id still present; the binding would be read from the wrong column")
	}
	if hasColumn(s2.DB(), &models.Share{}, "local_block_store_id") {
		t.Error("local_block_store_id still present; the journal no longer hangs off a per-share store")
	}
}

// A journal root that names somewhere other than where a share's bytes already
// sit would open an empty journal beside them and read back zeros, so the
// upgrade refuses while the recorded location can still be compared.
func TestMigration_RefusesAJournalRootThatDoesNotMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")
	makeSplitStoreSchema(t, path, func(s *GORMStore) {
		insertLocalStore(t, s, "local-bs-id", "/srv/elsewhere")
		insertSplitShare(t, s, "/legacy", "remote-bs-id", "local-bs-id")
	})

	_, err := openWithRoot(t, path, "/var/lib/dittofs/blocks")
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("New = %v, want ErrJournalRootMismatch", err)
	}
}

// A share that only ever had a local store has nothing to carry into the new
// model. Dropping the column would leave it bound to nothing and silently
// absent after the next restart, so the upgrade refuses and names it.
func TestMigration_RefusesALocalOnlyShare(t *testing.T) {
	// Both shapes an unbound column can take: NULL is what the old pointer
	// field actually wrote, the empty string is what a row rewritten by a
	// later migration can leave behind. The guard has to refuse either.
	for _, tc := range []struct {
		name    string
		unbound any
	}{
		{"null", nil},
		{"empty string", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cp.db")
			makeSplitStoreSchema(t, path, func(s *GORMStore) {
				insertLocalStore(t, s, "local-bs-id", "/srv/blocks")
				insertSplitShare(t, s, "/localonly", tc.unbound, "local-bs-id")
			})

			_, err := openWithRoot(t, path, "/srv/blocks")
			if !errors.Is(err, ErrLocalOnlyShareUnbound) {
				t.Fatalf("New = %v, want ErrLocalOnlyShareUnbound", err)
			}
		})
	}
}
