package sqlite

import "database/sql"

// DBForBench exposes the underlying pool to same-package-adjacent benchmarks so
// they can prototype alternative write statements against the real schema.
// Test-only: the file is a _test.go, so it is never compiled into the binary.
func (s *SQLiteMetadataStore) DBForBench() *sql.DB { return s.db }
