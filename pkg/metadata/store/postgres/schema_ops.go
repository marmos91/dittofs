package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// RestoreOrphan represents one leftover restore-temp schema discovered by
// ListSchemasByPrefix. It mirrors stores.PostgresRestoreOrphan but lives here
// so the postgres engine can return it without a dependency on the runtime
// layer (the runtime layer converts between the two where needed).
type RestoreOrphan struct {
	// Name is the fully-qualified schema name (no quoting, no namespacing).
	Name string

	// CreatedAt is the schema creation timestamp. Postgres does not expose
	// schema-creation timestamps natively; we derive the value from the ULID
	// suffix embedded in every Phase-5-generated temp schema name (see D-14).
	// When the suffix is not a valid ULID, CreatedAt is the zero time and
	// callers may choose to skip the entry during orphan-age filtering.
	CreatedAt time.Time
}

// ListSchemasByPrefix returns every schema whose name starts with the given
// prefix. Used by Phase-5 restore's startup orphan sweep (D-14) to find
// leftover `<origSchema>_restore_<ulid>` schemas that a prior crash-aborted
// restore could not clean up.
//
// The returned slice is always non-nil: an empty prefix match returns
// []RestoreOrphan{} rather than nil so Plan 07's sweep can range safely.
//
// Timestamp derivation:
//
//	Postgres does NOT record schema creation timestamps in information_schema
//	or pg_namespace (unlike tables, which have pg_stat_user_tables).
//	Phase-5 temp schemas encode a millisecond-precision timestamp in the
//	ULID portion of the name — we parse that instead of hitting the
//	filesystem via pg_stat_file (unportable, permission-sensitive).
//	Entries whose suffix after the prefix is not a valid ULID get a zero
//	CreatedAt; the caller's grace-window filter treats them as "unknown age".
//
// Errors propagate verbatim from the connection pool (connection failures,
// permission denials, query timeouts). This method is REQUIRED (non-optional)
// per Phase-5 D-14; silent degradation is unacceptable.
func (s *PostgresMetadataStore) ListSchemasByPrefix(ctx context.Context, prefix string) ([]RestoreOrphan, error) {
	if prefix == "" {
		return nil, fmt.Errorf("ListSchemasByPrefix: prefix must not be empty")
	}

	const query = `
		SELECT schema_name
		FROM information_schema.schemata
		WHERE schema_name LIKE $1
		ORDER BY schema_name
	`
	rows, err := s.pool.Query(ctx, query, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("query schemata: %w", err)
	}
	defer rows.Close()

	orphans := []RestoreOrphan{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan schemata: %w", err)
		}
		orphan := RestoreOrphan{Name: name}
		// Extract the ULID portion (everything after the prefix) and parse
		// its embedded timestamp. Invalid / non-ULID suffixes yield a zero
		// time — retained in output so the caller can decide how to age.
		suffix := strings.TrimPrefix(name, prefix)
		if id, perr := ulid.ParseStrict(suffix); perr == nil {
			orphan.CreatedAt = ulid.Time(id.Time())
		}
		orphans = append(orphans, orphan)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schemata: %w", err)
	}
	return orphans, nil
}

// DropSchema issues `DROP SCHEMA <name> CASCADE IF EXISTS` against the live
// pool. Used by Phase-5 restore's CommitSwap path to reclaim the old
// schema's storage after a successful swap, and by the D-14 orphan sweep
// to reclaim `<orig>_restore_<ulid>` schemas older than the grace window.
//
// The operation is idempotent — `IF EXISTS` swallows the case where the
// schema has already been dropped by a concurrent process.
//
// Identifier quoting uses the pg_catalog.quote_ident shape (double-quote,
// with embedded quotes doubled). Callers must still avoid passing
// operator-controlled strings; Phase-5 temp schemas are generated from
// ULIDs, which never contain characters that require escaping.
//
// Errors surface verbatim from the pool — connection failure, permission
// denied, dependent object still locked, etc.
func (s *PostgresMetadataStore) DropSchema(ctx context.Context, schemaName string) error {
	if schemaName == "" {
		return fmt.Errorf("DropSchema: schema name must not be empty")
	}
	// Double any embedded `"` per Postgres identifier quoting rules so a
	// malformed name cannot escape the identifier context. ULIDs generated
	// by Phase-5 never contain `"`; this is defense in depth only.
	safe := `"` + strings.ReplaceAll(schemaName, `"`, `""`) + `"`
	sql := fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, safe)
	if _, err := s.pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("drop schema %q: %w", schemaName, err)
	}
	return nil
}
