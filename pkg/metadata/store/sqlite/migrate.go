package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite/migrations"
)

// runMigrations applies the embedded `*.up.sql` migrations in ascending numeric
// order, tracking applied versions in a schema_migrations table.
//
// A bespoke runner (rather than golang-migrate) is used deliberately: the
// golang-migrate sqlite driver blank-imports modernc.org/sqlite, which
// registers the database/sql driver name "sqlite" a SECOND time alongside the
// glebarez/go-sqlite driver the control-plane GORM layer registers — and two
// registrations of the same name panic at init. Running the SQL ourselves over
// the already-registered "sqlite" driver avoids pulling in that conflicting
// registration while keeping the migrations file-based and versioned.
func runMigrations(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	log.Info("Running database migrations...")

	// Migrating a database that is already ahead of this build is not a no-op
	// worth reporting as "up to date" — refuse it here too, so the manual
	// migrate path fails as loudly as the open path.
	if err := checkFormatVersion(ctx, db); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`,
	); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	files, err := upMigrationFiles()
	if err != nil {
		return err
	}

	count := 0
	for _, m := range files {
		if applied[m.version] {
			continue
		}
		body, rerr := migrations.FS.ReadFile(m.name)
		if rerr != nil {
			return fmt.Errorf("read migration %s: %w", m.name, rerr)
		}
		if err := applyMigration(ctx, db, m.version, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		count++
	}

	if count == 0 {
		log.Info("No migrations to apply (database is up to date)")
	} else {
		log.Info("Migrations completed successfully", "applied", count)
	}
	return nil
}

// checkFormatVersion refuses to open a database migrated past what this build
// knows how to run against.
//
// schema_migrations already records every applied version, so the newest
// applied row versus the newest embedded migration is the whole comparison — no
// second stamp is needed. A row above that ceiling means a newer release
// migrated this database and may have dropped or renamed columns this build's
// queries still name. Without the check the database opens cleanly (there is
// nothing to apply, so migration reports "up to date") and then fails query by
// query with raw SQL errors, long after startup.
func checkFormatVersion(ctx context.Context, db *sql.DB) error {
	var tables int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`,
	).Scan(&tables); err != nil {
		return fmt.Errorf("probe schema_migrations: %w", err)
	}
	if tables == 0 {
		// A database no migration has ever touched; runMigrations creates the
		// table and brings it to the embedded ceiling.
		return nil
	}

	var stored sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&stored); err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	if !stored.Valid {
		return nil
	}

	files, err := upMigrationFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	newest := files[len(files)-1].version

	if stored.Int64 > int64(newest) {
		return fmt.Errorf("%w: metadata database is at schema version %d, this build reads up to %d",
			block.ErrFutureFormat, stored.Int64, newest)
	}
	return nil
}

// applyMigration runs one migration's SQL and records its version atomically.
func applyMigration(ctx context.Context, db *sql.DB, version int, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, stmt := range splitStatements(body) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement %q: %w", truncateForError(stmt), err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

type migrationFile struct {
	version int
	name    string
}

// upMigrationFiles lists embedded `NNNNNN_*.up.sql` files in ascending version
// order.
func upMigrationFiles() ([]migrationFile, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var out []migrationFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		idx := strings.IndexByte(name, '_')
		if idx <= 0 {
			continue
		}
		v, perr := strconv.Atoi(name[:idx])
		if perr != nil {
			continue
		}
		out = append(out, migrationFile{version: v, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// splitStatements splits a migration body into individual statements on the
// `;` terminator. `--` line comments are stripped FIRST so a `;` inside a
// comment (e.g. prose punctuation) never splits a statement. The schema uses no
// triggers or stored procedures and no string literals containing `;`, so a
// `;` split of the comment-free text is correct.
func splitStatements(body string) []string {
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	parts := strings.Split(sb.String(), ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// RunMigrations is a public wrapper for manual migration execution (e.g. CLI).
func RunMigrations(ctx context.Context, cfg *SQLiteMetadataStoreConfig) error {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	log := logger.With("component", "sqlite_migration")

	db, err := sql.Open(sqliteDriverName, cfg.DSN())
	if err != nil {
		return fmt.Errorf("failed to open sqlite database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping sqlite database: %w", err)
	}
	return runMigrations(ctx, db, log)
}
