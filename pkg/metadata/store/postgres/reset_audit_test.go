package postgres

// This audit test lives in package `postgres` (not `postgres_test`) so it can
// read the unexported `backupTables` slice directly. It has NO build tag and
// requires NO database — it's a pure file-system audit over the migrations
// directory: every new CREATE TABLE that lands without a backupTables update
// fails the test.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestBackupTablesCoversAllMigrations is a CI guard: every CREATE TABLE in
// pkg/metadata/store/postgres/migrations/*.up.sql must appear in the
// `backupTables` slice. Catches the case where a new migration lands
// without a corresponding backupTables update — which would silently
// leave the new table un-backed-up AND un-truncated by Reset, producing
// a half-restored state. Runs without a Postgres DSN.
func TestBackupTablesCoversAllMigrations(t *testing.T) {
	migrationsDir := "migrations"
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	// CREATE TABLE [IF NOT EXISTS] <name> — capture the bare table name.
	createTableRE := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	// ALTER TABLE [IF EXISTS] <old> RENAME TO <new> — a rename retires <old>
	// and introduces <new> (e.g. files -> inodes in 000032).
	renameTableRE := regexp.MustCompile(`(?im)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)\s+RENAME\s+TO\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// Migrations apply in lexical filename order; process them that way so a
	// later RENAME correctly supersedes an earlier CREATE.
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	seen := map[string]string{} // live table -> migration file that created it
	for _, name := range files {
		path := filepath.Join(migrationsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range createTableRE.FindAllStringSubmatch(string(data), -1) {
			tbl := m[1]
			// schema_migrations is owned by golang-migrate (see backupTables doc-comment).
			if tbl == "schema_migrations" {
				continue
			}
			if _, exists := seen[tbl]; !exists {
				seen[tbl] = name
			}
		}
		// Apply renames after creates in the same file: the old name is no
		// longer a live table; the new name inherits its provenance.
		for _, m := range renameTableRE.FindAllStringSubmatch(string(data), -1) {
			oldName, newName := m[1], m[2]
			src, existed := seen[oldName]
			delete(seen, oldName)
			if !existed {
				src = name
			}
			if _, exists := seen[newName]; !exists {
				seen[newName] = src
			}
		}
	}

	if len(seen) == 0 {
		t.Fatal("found zero CREATE TABLE statements in migrations dir — regex/path drift?")
	}

	covered := map[string]bool{}
	for _, tbl := range backupTables {
		covered[tbl] = true
	}

	var missing []string
	for tbl, src := range seen {
		if !covered[tbl] {
			missing = append(missing, tbl+" (from "+src+")")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("backupTables is missing %d table(s); update pkg/metadata/store/postgres/backup.go:\n  - %s",
			len(missing), strings.Join(missing, "\n  - "))
	}
}
