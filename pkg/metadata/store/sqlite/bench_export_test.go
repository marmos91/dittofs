package sqlite

import "database/sql"

// DBForBench exposes the underlying pool to same-package-adjacent benchmarks so
// they can prototype alternative write statements against the real schema.
// Test-only: a _test.go file is compiled into the test binary and excluded
// from every non-test build, so nothing ships.
func (s *SQLiteMetadataStore) DBForBench() *sql.DB { return s.db }

// SetAfterCommitFold installs a function to run between a transaction's commit
// and the fold of its quota delta into the cache, and returns a function that
// removes it again. Test-only.
func SetAfterCommitFold(f func()) func() {
	afterCommitFold = f
	return func() { afterCommitFold = nil }
}
