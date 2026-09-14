package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// installLegacyBlockStoreSchema replaces the migrated table with the shape it
// had while block stores still carried a kind: uniqueness over (name, kind),
// and a kind column that is NOT NULL with no default. ALTER TABLE cannot
// reproduce that last detail, so the table is recreated outright.
func installLegacyBlockStoreSchema(t *testing.T, s *GORMStore) {
	t.Helper()
	for _, stmt := range []string{
		"DROP TABLE block_store_configs",
		"CREATE TABLE `block_store_configs` (`id` text,`name` text NOT NULL,`kind` text NOT NULL," +
			"`type` text NOT NULL,`config` text,`created_at` datetime,PRIMARY KEY (`id`))",
		"CREATE INDEX `idx_block_store_configs_kind` ON `block_store_configs`(`kind`)",
		"CREATE UNIQUE INDEX `idx_block_store_name_kind` ON `block_store_configs`(`name`,`kind`)",
	} {
		if err := s.DB().Exec(stmt).Error; err != nil {
			t.Fatalf("install legacy schema (%s): %v", stmt, err)
		}
	}
}

// The old unique index was (name, kind), so a local and a remote store could
// share a name. Dropping kind makes name globally unique and those rows
// collide. Renaming one under the operator breaks their scripts later instead
// of now, and deleting one orphans any share that referenced it.
func TestMigration_RefusesBlockStoreNameCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")

	s := openAt(t, path)
	installLegacyBlockStoreSchema(t, s)
	for _, kind := range []string{"local", "remote"} {
		if err := s.DB().Exec(
			`INSERT INTO block_store_configs (id, name, kind, type, config)
			 VALUES (?, ?, ?, ?, ?)`,
			"id-"+kind, "blocks", kind, "memory", "{}",
		).Error; err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := New(&Config{Type: DatabaseTypeSQLite, SQLite: SQLiteConfig{Path: path}})
	if err == nil {
		t.Fatal("got nil, want a refusal for the colliding rows")
	}
	if !errors.Is(err, ErrBlockStoreNameCollision) {
		t.Fatalf("errors.Is(err, ErrBlockStoreNameCollision) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "blocks") {
		t.Errorf("error must name the colliding store; got %v", err)
	}
}

// The kind column has to leave with the discriminator, not merely stop being
// read: it is NOT NULL with no default, so every later insert that no longer
// names it would be rejected. Its own index has to go first, because SQLite
// refuses to drop an indexed column.
func TestMigration_DropsTheKindColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")

	s := openAt(t, path)
	installLegacyBlockStoreSchema(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := openAt(t, path)
	defer func() { _ = s2.Close() }()

	if s2.DB().Migrator().HasColumn("block_store_configs", "kind") {
		t.Error("kind column survived the migration")
	}
	if err := s2.DB().Exec(
		"INSERT INTO block_store_configs (id, name, type, config) VALUES (?, ?, ?, ?)",
		"id-1", "blocks", "memory", "{}",
	).Error; err != nil {
		t.Errorf("insert without kind: %v", err)
	}
}
