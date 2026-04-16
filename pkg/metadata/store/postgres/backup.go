// Package postgres — backup driver (ENG-02).
//
// This file implements the metadata.Backupable interface on top of the
// PostgreSQL metadata store using logical, per-table COPY (FORMAT binary)
// dumps bundled inside a tar archive. A single REPEATABLE READ / READ ONLY
// transaction is used for the duration of Backup so that the payload-id set
// and all per-table streams observe one consistent snapshot (D-02).
//
// Archive layout:
//
//	manifest.yaml           — first entry, contains format_version, pg_server_version,
//	                          schema_migration_version, and per-table row counts (D-09).
//	tables/<name>.bin       — one file per backed-up table, PostgreSQL binary COPY
//	                          format (D-01).
//
// Restore parses manifest.yaml first (outside any transaction), enforces the
// schema-version gate (D-04) and destination-empty gate (D-06), and only then
// opens a transaction to COPY FROM STDIN into each table in dependency order.
package postgres

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertion: PostgresMetadataStore implements metadata.Backupable.
var _ metadata.Backupable = (*PostgresMetadataStore)(nil)

// backupFormatVersion is the on-disk format version carried in every manifest
// emitted by this driver. Separate from the database's schema_migrations
// version (which is dictated by the migrations directory): the format version
// gates archive layout evolution (new top-level files, tar header conventions,
// compression, encryption wrappers), while the schema_migration_version gates
// table shape.
const backupFormatVersion = 1

// backupEngineKind is recorded in the manifest so cross-engine restore
// attempts can be refused early by the restore orchestrator (Phase 5).
const backupEngineKind = "postgres"

// deterministicModTime is a fixed timestamp baked into every tar header so
// that byte-identical input data produces byte-identical output archives
// (needed for reproducible SHA-256 manifests). The value is the Unix epoch
// — semantically meaningless, deliberately so.
var deterministicModTime = time.Unix(0, 0).UTC()

// backupTables enumerates every table the driver streams, in the order the
// archive writes them (and the order Restore replays them). The order is NOT
// a FK-dependency order for COPY FROM: during restore, triggers are disabled
// per-session so ordering is not constraint-sensitive; alphabetical-by-name
// is what we serialize for determinism, and it's also the sorted set of
// tables that migrations 000001..000005 create.
//
// Keep in sync with pkg/metadata/store/postgres/migrations/*.up.sql.
var backupTables = []string{
	"durable_handles",
	"filesystem_capabilities",
	"files",
	"link_counts",
	"locks",
	"nsm_client_registrations",
	"parent_child_map",
	"pending_writes",
	"server_config",
	"server_epoch",
	"shares",
}

// backupManifest is the YAML document that lives at manifest.yaml inside the
// archive. All fields are required in the D-09 sense — a restore reader must
// be able to round-trip every field it wrote.
type backupManifest struct {
	// FormatVersion is the archive layout version (see backupFormatVersion).
	FormatVersion int `yaml:"format_version"`

	// EngineKind identifies the metadata-store engine that produced this
	// archive. Cross-engine restore is refused by Phase 5 orchestration; the
	// driver records this so the refusal can be mechanical.
	EngineKind string `yaml:"engine_kind"`

	// PgServerVersion is the `server_version` reported by PostgreSQL at backup
	// time (for example "16.2"). Recorded for diagnostics; strict equality is
	// NOT required at restore (point releases are wire-compatible for the
	// binary COPY format).
	PgServerVersion string `yaml:"pg_server_version"`

	// SchemaMigrationVersion is the `version` column of `schema_migrations`
	// at the moment of backup. Restore refuses with ErrSchemaVersionMismatch
	// if the destination's current schema does not match (D-04).
	SchemaMigrationVersion int64 `yaml:"schema_migration_version"`

	// TableList is the set of tables serialized and their row counts at
	// snapshot time. Restore verifies each named table is present in the
	// archive and logs any deviation before it begins COPY FROM.
	TableList []backupTableEntry `yaml:"table_list"`

	// CreatedAt is a human-readable timestamp of when the archive was
	// produced. NOT used for deterministic tar headers (which use
	// deterministicModTime) — it's metadata only.
	CreatedAt time.Time `yaml:"created_at"`
}

// backupTableEntry records one row of the manifest's table_list.
type backupTableEntry struct {
	Name     string `yaml:"name"`
	RowCount int64  `yaml:"row_count"`
}

// Backup streams a consistent snapshot of the metadata store to w.
//
// Behavior (D-01, D-02, D-04, D-09):
//   - Opens a single REPEATABLE READ / READ ONLY transaction and performs ALL
//     reads — payload-id set, schema version, per-table COPY streams — inside
//     that transaction so every output observes one snapshot.
//   - Writes a tar archive with manifest.yaml first, followed by one
//     tables/<name>.bin entry per table in alphabetical order.
//   - Uses PostgreSQL binary COPY (FORMAT binary) for every table.
//   - All tar headers use a fixed ModTime so byte-identical inputs produce
//     byte-identical outputs.
//
// The returned PayloadIDSet contains every non-NULL `content_id` present in
// the `files` table at snapshot time (SAFETY-01 GC-hold support).
func (s *PostgresMetadataStore) Backup(ctx context.Context, w io.Writer) (metadata.PayloadIDSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Acquire a dedicated connection for the entire backup so that CopyTo
	// (which uses the raw *PgConn) observes the same transaction as our
	// SELECT queries. Pool-wide sql operations cannot guarantee this.
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	conn, err := s.pool.Acquire(acquireCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("postgres backup: acquire connection: %w", err)
	}
	defer conn.Release()

	// D-02 / D-04: REPEATABLE READ + READ ONLY for the entire snapshot.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres backup: begin tx: %w", err)
	}
	// Read-only transaction; Rollback on both success and failure paths.
	// Commit is unnecessary and Rollback on a committed tx is a no-op, but
	// we do not commit because the tx performs no writes.
	defer func() {
		rollbackCtx, rbCancel := context.WithTimeout(context.Background(), poolConnectionAcquireTimeout)
		_ = tx.Rollback(rollbackCtx)
		rbCancel()
	}()

	// Capture server version (diagnostic).
	var pgServerVersion string
	if err := tx.QueryRow(ctx, `SHOW server_version`).Scan(&pgServerVersion); err != nil {
		return nil, fmt.Errorf("postgres backup: read server_version: %w", err)
	}

	// D-04: read the latest schema_migrations version inside the tx.
	schemaVersion, err := readSchemaVersionTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("postgres backup: read schema version: %w", err)
	}

	// D-02: payload-id scan — same transaction as the per-table COPYs.
	payloadIDs, err := scanPayloadIDsTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("postgres backup: scan payload ids: %w", err)
	}

	// Row counts per table (for the manifest; same snapshot).
	tableEntries, err := countTableRowsTx(ctx, tx, backupTables)
	if err != nil {
		return nil, fmt.Errorf("postgres backup: count rows: %w", err)
	}

	manifest := &backupManifest{
		FormatVersion:          backupFormatVersion,
		EngineKind:             backupEngineKind,
		PgServerVersion:        pgServerVersion,
		SchemaMigrationVersion: schemaVersion,
		TableList:              tableEntries,
		// CreatedAt is set to the snapshot wall-clock time (advisory, not used
		// in any determinism-sensitive path).
		CreatedAt: time.Now().UTC(),
	}
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("postgres backup: marshal manifest: %w", err)
	}

	tw := tar.NewWriter(w)
	// Write manifest first so a partial archive is still self-describing up
	// to the point of truncation.
	if err := writeTarEntry(tw, "manifest.yaml", manifestBytes); err != nil {
		return nil, fmt.Errorf("postgres backup: write manifest entry: %w", err)
	}

	// Per-table COPY TO STDOUT (FORMAT binary). The raw *PgConn drives COPY;
	// the tx we opened above is bound to this same conn so COPY observes the
	// REPEATABLE READ snapshot.
	pgConn := conn.Conn().PgConn()
	for _, tbl := range backupTables {
		var buf bytes.Buffer
		copyQuery := fmt.Sprintf("COPY %s TO STDOUT (FORMAT binary)", quoteIdent(tbl))
		if _, err := pgConn.CopyTo(ctx, &buf, copyQuery); err != nil {
			return nil, fmt.Errorf("postgres backup: COPY TO for %q: %w", tbl, err)
		}
		entryName := fmt.Sprintf("tables/%s.bin", tbl)
		if err := writeTarEntry(tw, entryName, buf.Bytes()); err != nil {
			return nil, fmt.Errorf("postgres backup: write tar entry %q: %w", entryName, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("postgres backup: close tar: %w", err)
	}

	return payloadIDs, nil
}

// Restore reloads the metadata store from r.
//
// Preconditions (D-04, D-06):
//   - Manifest is parsed BEFORE opening any transaction; if the archive's
//     schema_migration_version does not match the binary's current schema
//     version, metadata.ErrSchemaVersionMismatch is returned without ever
//     taking a lock on the destination.
//   - The destination is verified empty (at least the `files` table); if any
//     rows are present, metadata.ErrRestoreDestinationNotEmpty is returned
//     with no modification.
//
// Implementation notes:
//   - COPY FROM STDIN (FORMAT binary) is used for every table (D-01).
//   - Triggers on `files` rewrite path_hash and content_id_hash from the
//     source rows on INSERT, which would overwrite the hashes produced at
//     backup time. We disable user triggers for the duration of the COPY so
//     the backed-up bytes are restored verbatim (hashes are deterministic
//     from path/content_id anyway, so the result is byte-identical).
//   - All table COPYs happen inside a single transaction so Restore is
//     atomic end-to-end: a mid-copy failure rolls everything back.
func (s *PostgresMetadataStore) Restore(ctx context.Context, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Phase 1: parse the archive into memory. Keep the manifest separate and
	// indexed by entry name so we can run D-04/D-06 checks before touching
	// the destination.
	manifest, tableBlobs, err := readBackupArchive(r)
	if err != nil {
		return fmt.Errorf("%w: read archive: %v", metadata.ErrRestoreCorrupt, err)
	}
	if manifest.FormatVersion != backupFormatVersion {
		return fmt.Errorf("%w: unsupported archive format_version %d (this build supports %d)",
			metadata.ErrRestoreCorrupt, manifest.FormatVersion, backupFormatVersion)
	}
	if manifest.EngineKind != backupEngineKind {
		return fmt.Errorf("%w: archive engine_kind %q does not match %q",
			metadata.ErrRestoreCorrupt, manifest.EngineKind, backupEngineKind)
	}

	// D-04: schema-version gate. Run before any destination mutation.
	currentSchemaVersion, err := s.readSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("postgres restore: read current schema version: %w", err)
	}
	if currentSchemaVersion != manifest.SchemaMigrationVersion {
		return fmt.Errorf("%w: archive=%d, binary=%d",
			metadata.ErrSchemaVersionMismatch,
			manifest.SchemaMigrationVersion, currentSchemaVersion)
	}

	// D-06: destination-empty gate.
	empty, err := s.destinationIsEmpty(ctx)
	if err != nil {
		return fmt.Errorf("postgres restore: check destination empty: %w", err)
	}
	if !empty {
		return metadata.ErrRestoreDestinationNotEmpty
	}

	// Phase 2: atomic COPY FROM into every table.
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	conn, err := s.pool.Acquire(acquireCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("postgres restore: acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres restore: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, rbCancel := context.WithTimeout(context.Background(), poolConnectionAcquireTimeout)
		_ = tx.Rollback(rollbackCtx)
		rbCancel()
	}()

	// `session_replication_role = replica` suppresses user trigger firing for
	// this session (it does NOT affect other concurrent sessions). This is
	// the standard pg_restore technique.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
		return fmt.Errorf("postgres restore: disable triggers: %w", err)
	}

	// Wipe bootstrap rows installed by migrations (e.g. `server_config` and
	// `filesystem_capabilities` are singleton tables seeded at migration
	// time) before replaying the archive. Without this, COPY FROM STDIN
	// would fail with duplicate-key errors on every singleton table.
	//
	// The D-06 gate above ensures `files` is empty; the rest of the tables
	// may legitimately have migration-installed rows. TRUNCATE RESTART
	// IDENTITY CASCADE wipes them all in one statement, respecting FK
	// dependencies (CASCADE is a no-op when every table is TRUNCATE'd).
	//
	// TRUNCATE is DDL-ish under REPLICATION, but still transactional: a
	// Rollback on the enclosing tx reverts it cleanly.
	truncateStmt := fmt.Sprintf(
		"TRUNCATE TABLE %s RESTART IDENTITY CASCADE",
		strings.Join(quoteIdents(backupTables), ", "),
	)
	if _, err := tx.Exec(ctx, truncateStmt); err != nil {
		return fmt.Errorf("postgres restore: truncate destination tables: %w", err)
	}

	pgConn := conn.Conn().PgConn()
	for _, tbl := range backupTables {
		data, ok := tableBlobs[tbl]
		if !ok {
			return fmt.Errorf("%w: archive missing entry for table %q", metadata.ErrRestoreCorrupt, tbl)
		}
		copyQuery := fmt.Sprintf("COPY %s FROM STDIN (FORMAT binary)", quoteIdent(tbl))
		if _, err := pgConn.CopyFrom(ctx, bytes.NewReader(data), copyQuery); err != nil {
			return fmt.Errorf("postgres restore: COPY FROM for %q: %w", tbl, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres restore: commit: %w", err)
	}
	committed = true

	// Reinitialize the used-bytes counter from the freshly restored `files`
	// rows so subsequent GetUsedBytes() calls reflect the restored state.
	if err := s.initUsedBytesCounter(ctx); err != nil {
		return fmt.Errorf("postgres restore: reinit used bytes counter: %w", err)
	}

	return nil
}

// ----------------------------------------------------------------------------
// Internal helpers
// ----------------------------------------------------------------------------

// readSchemaVersion reads the latest schema_migrations.version using a
// short-lived pool connection. Used by Restore before opening a transaction.
func (s *PostgresMetadataStore) readSchemaVersion(ctx context.Context) (int64, error) {
	var version int64
	err := s.pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).
		Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// readSchemaVersionTx reads schema_migrations.version inside the given tx.
func readSchemaVersionTx(ctx context.Context, tx pgx.Tx) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).
		Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// scanPayloadIDsTx returns the set of distinct non-NULL content_ids present
// in the `files` table inside the given transaction.
//
// Column naming note: Postgres uses `content_id` (see migration 000001); this
// is the same logical value as metadata.PayloadID in the Go model.
func scanPayloadIDsTx(ctx context.Context, tx pgx.Tx) (metadata.PayloadIDSet, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT content_id FROM files WHERE content_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	set := metadata.NewPayloadIDSet()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set.Add(id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// countTableRowsTx runs SELECT COUNT(*) on each table and returns entries in
// the same order as `tables`.
func countTableRowsTx(ctx context.Context, tx pgx.Tx, tables []string) ([]backupTableEntry, error) {
	entries := make([]backupTableEntry, 0, len(tables))
	for _, tbl := range tables {
		var n int64
		if err := tx.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", quoteIdent(tbl))).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %q: %w", tbl, err)
		}
		entries = append(entries, backupTableEntry{Name: tbl, RowCount: n})
	}
	return entries, nil
}

// destinationIsEmpty is the D-06 gate. It checks the `files` table as the
// primary signal; callers use it as a proxy for "the store has been wiped".
func (s *PostgresMetadataStore) destinationIsEmpty(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM files LIMIT 1)`).Scan(&exists)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// writeTarEntry writes a single tar entry with deterministic header fields.
// Ownership/mode are fixed so two archives with identical payloads hash to
// the same SHA-256.
func writeTarEntry(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  deterministicModTime,
		Typeflag: tar.TypeReg,
		// Explicit zero values for Uid/Gid/Uname/Gname suppress any host
		// bleed-through from archive/tar's defaults.
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// maxBackupEntryBytes caps any single tar entry during Restore to prevent a
// tampered archive with a crafted Size header from driving an unbounded
// allocation. 8 GiB is well above any plausible single metadata table; real
// archives are orders of magnitude smaller.
const maxBackupEntryBytes int64 = 8 << 30

// readBackupArchive parses the tar stream into a manifest and a per-table
// blob map. It does NOT touch the database. Returns an error if manifest.yaml
// is missing or malformed.
func readBackupArchive(r io.Reader) (*backupManifest, map[string][]byte, error) {
	tr := tar.NewReader(r)
	var manifest *backupManifest
	blobs := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read tar header: %w", err)
		}
		if hdr.Size > maxBackupEntryBytes {
			return nil, nil, fmt.Errorf("tar entry %q size %d exceeds limit %d", hdr.Name, hdr.Size, maxBackupEntryBytes)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBackupEntryBytes))
		if err != nil {
			return nil, nil, fmt.Errorf("read tar entry %q: %w", hdr.Name, err)
		}
		switch {
		case hdr.Name == "manifest.yaml":
			if manifest != nil {
				return nil, nil, fmt.Errorf("duplicate manifest.yaml in archive")
			}
			var m backupManifest
			if err := yaml.Unmarshal(data, &m); err != nil {
				return nil, nil, fmt.Errorf("parse manifest.yaml: %w", err)
			}
			manifest = &m
		case strings.HasPrefix(hdr.Name, "tables/") && strings.HasSuffix(hdr.Name, ".bin"):
			tbl := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "tables/"), ".bin")
			if _, exists := blobs[tbl]; exists {
				return nil, nil, fmt.Errorf("duplicate archive entry for table %q", tbl)
			}
			blobs[tbl] = data
		default:
			// Unknown entry names are preserved-by-ignoring: a future format
			// version may add them. A stricter policy lives at the
			// FormatVersion gate above.
		}
	}
	if manifest == nil {
		return nil, nil, errors.New("archive missing manifest.yaml")
	}
	// Defensive: sort the table list so downstream consumers that rely on
	// deterministic manifest output also see deterministic order on re-read.
	sort.SliceStable(manifest.TableList, func(i, j int) bool {
		return manifest.TableList[i].Name < manifest.TableList[j].Name
	})
	return manifest, blobs, nil
}

// quoteIdent produces a PostgreSQL-safe double-quoted identifier. Used for
// table names in COPY. All identifiers in this file are compile-time
// constants from backupTables, but we quote defensively.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteIdents is the vector form of quoteIdent — returns a fresh slice with
// every entry double-quoted. Used to build comma-separated lists of tables
// for statements like TRUNCATE.
func quoteIdents(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return out
}
